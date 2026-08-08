package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/pkg/models"
	"go.kenn.io/kwt/service"
	"golang.org/x/mod/semver"
)

type replacementDecision uint8

const (
	reuseDaemon replacementDecision = iota
	replaceDaemon
)

type ControllerOptions struct {
	Home             string
	Build            Build
	Config           models.DaemonConfig
	Executable       string
	Environment      []string
	Progress         io.Writer
	AllowEphemeral   bool
	Inspect          func(context.Context) (Observation, error)
	Launch           func(context.Context) error
	RequestShutdown  func(context.Context, Observation, string) error
	PollInterval     time.Duration
	StartTimeout     time.Duration
	CleanupAllowance time.Duration
}

type Controller struct {
	options ControllerOptions
}

func NewController(options ControllerOptions) *Controller {
	if options.Progress == nil {
		options.Progress = io.Discard
	}
	if options.Environment == nil {
		options.Environment = os.Environ()
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 50 * time.Millisecond
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = 5 * time.Second
	}
	if options.CleanupAllowance <= 0 {
		options.CleanupAllowance = 5 * time.Second
	}
	controller := &Controller{options: options}
	if controller.options.Inspect == nil {
		controller.options.Inspect = func(ctx context.Context) (Observation, error) {
			return Inspect(ctx, RuntimeStore(controller.options.Home), controller.options.Home)
		}
	}
	if controller.options.Launch == nil {
		controller.options.Launch = controller.launchDetached
	}
	if controller.options.RequestShutdown == nil {
		controller.options.RequestShutdown = requestShutdown
	}
	return controller
}

func (c *Controller) Status(ctx context.Context) (Observation, error) {
	return c.options.Inspect(ctx)
}

func (c *Controller) Start(ctx context.Context) (Observation, error) {
	release, err := RuntimeStore(c.options.Home).AcquireStartLock(ctx)
	if err != nil {
		return Observation{}, err
	}
	defer release()
	return c.startLocked(ctx)
}

func (c *Controller) Stop(ctx context.Context) error {
	release, err := RuntimeStore(c.options.Home).AcquireStartLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	observation, err := c.options.Inspect(ctx)
	if err != nil {
		return err
	}
	switch observation.State {
	case RuntimeAbsent:
		return nil
	case RuntimeReady, RuntimeStarting, RuntimeFailed:
		if err := c.options.RequestShutdown(ctx, observation, "stop"); err != nil {
			return err
		}
	case RuntimeDraining:
		c.reportDrain(observation)
	case RuntimeIncompatible:
		return c.incompatibleError(observation)
	case RuntimeUnresponsive:
		return c.unresponsiveError(observation)
	}
	return c.waitForAbsent(ctx)
}

func (c *Controller) Restart(ctx context.Context) (Observation, error) {
	release, err := RuntimeStore(c.options.Home).AcquireStartLock(ctx)
	if err != nil {
		return Observation{}, err
	}
	defer release()

	observation, err := c.options.Inspect(ctx)
	if err != nil {
		return Observation{}, err
	}
	switch observation.State {
	case RuntimeAbsent:
		return c.startLocked(ctx)
	case RuntimeReady, RuntimeStarting, RuntimeFailed:
		if olderVersion(c.options.Build.Version, observation.Status.Version) {
			return Observation{}, daemonDowngradeError()
		}
		if err := c.options.RequestShutdown(ctx, observation, "restart"); err != nil {
			return Observation{}, err
		}
	case RuntimeDraining:
		if olderVersion(c.options.Build.Version, observation.Status.Version) {
			return Observation{}, daemonDowngradeError()
		}
		c.reportDrain(observation)
	case RuntimeIncompatible:
		return Observation{}, c.incompatibleError(observation)
	case RuntimeUnresponsive:
		return Observation{}, c.unresponsiveError(observation)
	}
	if err := c.waitForAbsent(ctx); err != nil {
		return Observation{}, err
	}
	return c.startLocked(ctx)
}

func (c *Controller) startLocked(ctx context.Context) (Observation, error) {
	observation, err := c.options.Inspect(ctx)
	if err != nil {
		return Observation{}, err
	}
	switch observation.State {
	case RuntimeAbsent:
		return c.launchAndWait(ctx)
	case RuntimeReady:
		if decideReplacement(
			c.options.Build.Version,
			observation.Status.Version,
			c.options.Config.AutoRestart,
		) == reuseDaemon {
			return observation, nil
		}
		if err := c.options.RequestShutdown(ctx, observation, "replacement"); err != nil {
			return Observation{}, err
		}
		if err := c.waitForAbsent(ctx); err != nil {
			return Observation{}, err
		}
		return c.launchAndWait(ctx)
	case RuntimeStarting:
		return c.waitForReady(ctx)
	case RuntimeFailed:
		return Observation{}, c.failedError(observation)
	case RuntimeDraining:
		if olderVersion(c.options.Build.Version, observation.Status.Version) {
			return Observation{}, daemonDowngradeError()
		}
		c.reportDrain(observation)
		if err := c.waitForAbsent(ctx); err != nil {
			return Observation{}, err
		}
		return c.launchAndWait(ctx)
	case RuntimeIncompatible:
		return Observation{}, c.incompatibleError(observation)
	case RuntimeUnresponsive:
		return Observation{}, c.unresponsiveError(observation)
	default:
		return Observation{}, errors.New("unknown kwt daemon runtime state")
	}
}

func (c *Controller) launchAndWait(ctx context.Context) (Observation, error) {
	startupCtx, cancel := context.WithTimeout(ctx, c.options.StartTimeout)
	defer cancel()
	if err := c.options.Launch(startupCtx); err != nil {
		return Observation{}, err
	}
	return c.waitUntilReady(startupCtx)
}

func (c *Controller) waitForReady(ctx context.Context) (Observation, error) {
	startupCtx, cancel := context.WithTimeout(ctx, c.options.StartTimeout)
	defer cancel()
	return c.waitUntilReady(startupCtx)
}

func (c *Controller) waitUntilReady(ctx context.Context) (Observation, error) {
	for {
		observation, err := c.options.Inspect(ctx)
		if err != nil {
			return Observation{}, err
		}
		switch observation.State {
		case RuntimeReady:
			return observation, nil
		case RuntimeFailed:
			return Observation{}, c.failedError(observation)
		}
		if err := c.waitPoll(ctx); err != nil {
			return Observation{}, service.NewError(
				service.Busy,
				"kwt daemon did not become ready",
				true,
				nil,
				err,
			)
		}
	}
}

func (c *Controller) waitForAbsent(ctx context.Context) error {
	waitDeadline := time.Now().Add(
		c.options.Config.ReplacementGrace + c.options.CleanupAllowance,
	)
	for {
		observation, err := c.options.Inspect(ctx)
		if err != nil {
			return err
		}
		if observation.State == RuntimeAbsent {
			return nil
		}
		if observation.State == RuntimeDraining {
			c.reportDrain(observation)
			if observation.Status.DrainDeadline != nil {
				waitDeadline = observation.Status.DrainDeadline.Add(
					c.options.CleanupAllowance,
				)
			}
		}
		waitCtx, cancel := context.WithDeadline(ctx, waitDeadline)
		if err := c.waitPoll(waitCtx); err != nil {
			cancel()
			return service.NewError(
				service.Busy,
				"kwt daemon did not stop before the replacement deadline",
				true,
				nil,
				err,
			)
		}
		cancel()
	}
}

func (c *Controller) waitPoll(ctx context.Context) error {
	timer := time.NewTimer(c.options.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Controller) reportDrain(observation Observation) {
	deadline := "unknown"
	if observation.Status.DrainDeadline != nil {
		deadline = observation.Status.DrainDeadline.Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(
		c.options.Progress,
		"daemon draining, waiting on %d leases until %s\n",
		observation.Status.ActiveLeases,
		deadline,
	)
}

func (c *Controller) incompatibleError(observation Observation) error {
	return service.NewError(
		service.Unsupported,
		"the running kwt daemon uses an incompatible API",
		false,
		nil,
		observation.Err,
	)
}

func (c *Controller) unresponsiveError(observation Observation) error {
	return service.NewError(
		service.Conflict,
		"the running kwt daemon owner is unresponsive",
		true,
		nil,
		observation.Err,
	)
}

func (c *Controller) failedError(observation Observation) error {
	var cause error
	if observation.Status.LastError != nil {
		cause = errors.New(observation.Status.LastError.Message)
	}
	return service.NewError(
		service.Conflict,
		"the running kwt daemon is in a failed state",
		true,
		nil,
		cause,
	)
}

func daemonDowngradeError() error {
	return service.NewError(
		service.Unsupported,
		"an older kwt cannot replace a newer daemon",
		false,
		nil,
		nil,
	)
}

func (c *Controller) launchDetached(ctx context.Context) error {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()
	return kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable: c.options.Executable,
		Args:       []string{"serve", "--daemon-child"},
		Env: environmentWithCanonicalHome(
			c.options.Environment,
			c.options.Home,
		),
		Stdout:          devNull,
		Stderr:          devNull,
		RefuseEphemeral: !c.options.AllowEphemeral,
	})
}

func requestShutdown(
	ctx context.Context,
	observation Observation,
	reason string,
) error {
	if observation.Client == nil {
		return service.NewError(
			service.Conflict,
			"the running kwt daemon was not proof-verified",
			false,
			nil,
			nil,
		)
	}
	_, err := observation.Client.Shutdown(ctx, reason)
	return err
}

func comparableVersion(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	if value[0] != 'v' {
		value = "v" + value
	}
	if !semver.IsValid(value) {
		return "", false
	}
	return value, true
}

func decideReplacement(client, running, policy string) replacementDecision {
	if policy != "newer" {
		return reuseDaemon
	}
	clientVersion, clientOK := comparableVersion(client)
	runningVersion, runningOK := comparableVersion(running)
	if !clientOK || !runningOK {
		return reuseDaemon
	}
	if semver.Compare(clientVersion, runningVersion) > 0 {
		return replaceDaemon
	}
	return reuseDaemon
}

func olderVersion(client, running string) bool {
	clientVersion, clientOK := comparableVersion(client)
	runningVersion, runningOK := comparableVersion(running)
	return clientOK && runningOK && semver.Compare(clientVersion, runningVersion) < 0
}

func environmentWithCanonicalHome(environment []string, home string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		name, _, ok := strings.Cut(value, "=")
		if ok && strings.EqualFold(name, "KWT_HOME") {
			continue
		}
		result = append(result, value)
	}
	return append(result, "KWT_HOME="+home)
}
