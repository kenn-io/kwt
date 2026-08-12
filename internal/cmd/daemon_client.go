package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	kwt "go.kenn.io/kwt"
	"go.kenn.io/kwt/internal/config"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
)

var queryCLIInventory = queryInventoryForCLI
var removeDaemonWorktree = removeWorktreeThroughDaemon

func resolveSSHViaDaemon(
	ctx context.Context,
	request kwt.SSHResolveRequest,
) (kwt.SSHRouteSnapshot, error) {
	controller, err := newDaemonController()
	if err != nil {
		return kwt.SSHRouteSnapshot{}, err
	}
	for {
		observation, err := controller.Start(ctx)
		if err != nil {
			return kwt.SSHRouteSnapshot{}, err
		}
		if err := requireSSHResolveCapability(observation); err != nil {
			return kwt.SSHRouteSnapshot{}, err
		}
		result, err := observation.Client.ResolveSSH(ctx, request)
		deadline, draining := inventoryDrainDeadline(err)
		if !draining {
			return result, err
		}
		if err := waitInventoryRetry(ctx, deadline); err != nil {
			return kwt.SSHRouteSnapshot{}, err
		}
	}
}

func requireSSHResolveCapability(observation kwtdaemon.Observation) error {
	if observation.Client == nil || !slices.Contains(
		observation.Status.Capabilities,
		kwtdaemon.CapabilitySSHResolve,
	) {
		return service.NewError(
			service.DaemonIncompatible,
			"the running kwt daemon does not provide SSH route resolution",
			false,
			nil,
			nil,
		)
	}
	return nil
}

func removeProjectViaDaemon(
	ctx context.Context,
	request kwt.ProjectRemovalRequest,
) (kwt.ProjectRemovalResult, error) {
	controller, err := newDaemonController()
	if err != nil {
		return kwt.ProjectRemovalResult{}, err
	}
	for {
		observation, err := controller.Start(ctx)
		if err != nil {
			return kwt.ProjectRemovalResult{}, err
		}
		if observation.Client == nil || !slices.Contains(
			observation.Status.Capabilities,
			kwtdaemon.CapabilityProjectRemoval,
		) {
			return kwt.ProjectRemovalResult{}, service.NewError(
				service.DaemonIncompatible,
				"the running kwt daemon does not provide project removal",
				false, nil, nil,
			)
		}
		result, err := observation.Client.RemoveProject(ctx, request)
		deadline, draining := inventoryDrainDeadline(err)
		if !draining {
			return result, err
		}
		if err := waitInventoryRetry(ctx, deadline); err != nil {
			return kwt.ProjectRemovalResult{}, err
		}
	}
}

func daemonMutationRequiresRefresh(err error) bool {
	return kwtdaemon.RequiresRefresh(err)
}

func removeWorktreeThroughDaemon(
	ctx context.Context,
	request kwt.RemovalRequest,
) (kwt.RemovalResult, error) {
	controller, err := newDaemonController()
	if err != nil {
		return kwt.RemovalResult{}, err
	}
	for {
		observation, err := controller.Start(ctx)
		if err != nil {
			return kwt.RemovalResult{}, err
		}
		if observation.Client == nil ||
			!slices.Contains(observation.Status.Capabilities, kwtdaemon.CapabilityRemoval) {
			return kwt.RemovalResult{}, service.NewError(
				service.DaemonIncompatible,
				"the running kwt daemon does not provide worktree removal",
				false,
				nil,
				nil,
			)
		}
		result, err := observation.Client.RemoveWorktree(ctx, request)
		deadline, draining := inventoryDrainDeadline(err)
		if !draining {
			return result, err
		}
		if err := waitInventoryRetry(ctx, deadline); err != nil {
			return kwt.RemovalResult{}, err
		}
	}
}

func queryInventoryForCLI(
	ctx context.Context,
	request kwt.Request,
	interactive bool,
	stderr io.Writer,
) (kwt.Result, error) {
	controller, err := newDaemonController()
	if err != nil {
		return kwt.Result{}, err
	}
	if interactive {
		request.UntrustedConfig = kwt.RequireConfigInteraction
	} else {
		request.UntrustedConfig = kwt.IgnoreUntrustedConfig
	}
	declined := false
	for {
		result, queryErr := queryDaemonInventory(ctx, controller, request)
		if queryErr == nil {
			writeConfigNotes(stderr, result.Notes, interactive, declined)
			return result, nil
		}
		if !interactive || !service.IsCode(queryErr, service.InteractionRequired) {
			return kwt.Result{}, queryErr
		}
		requirement, requirementErr := trustRequirement(queryErr)
		if requirementErr != nil {
			return kwt.Result{}, requirementErr
		}
		approved, promptErr := config.PromptTrustRequirement(requirement)
		if promptErr != nil {
			return kwt.Result{}, promptErr
		}
		if !approved {
			declined = true
			request.UntrustedConfig = kwt.IgnoreUntrustedConfig
			continue
		}
		observation, startErr := controller.Start(ctx)
		if startErr != nil {
			return kwt.Result{}, startErr
		}
		if err := requireInventoryCapability(observation); err != nil {
			return kwt.Result{}, err
		}
		if err := observation.Client.ApproveConfig(ctx, kwt.ConfigApproval{
			Path: requirement.Path, Digest: requirement.Digest,
		}); err != nil {
			return kwt.Result{}, err
		}
		request.UntrustedConfig = kwt.RequireConfigInteraction
	}
}

func queryDaemonInventory(
	ctx context.Context,
	controller daemonController,
	request kwt.Request,
) (kwt.Result, error) {
	for {
		observation, err := controller.Start(ctx)
		if err != nil {
			return kwt.Result{}, err
		}
		if err := requireInventoryCapability(observation); err != nil {
			return kwt.Result{}, err
		}
		result, err := observation.Client.Inventory(ctx, request)
		deadline, draining := inventoryDrainDeadline(err)
		if !draining {
			return result, err
		}
		if err := waitInventoryRetry(ctx, deadline); err != nil {
			return kwt.Result{}, err
		}
	}
}

func inventoryDrainDeadline(err error) (*time.Time, bool) {
	var typed *service.Error
	if !errors.As(err, &typed) ||
		(typed.Code != service.DaemonDraining && typed.Code != service.Busy) {
		return nil, false
	}
	switch deadline := typed.Details["drain_deadline"].(type) {
	case time.Time:
		return &deadline, true
	case *time.Time:
		return deadline, deadline != nil
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		return &parsed, err == nil
	default:
		return nil, false
	}
}

func requireInventoryCapability(observation kwtdaemon.Observation) error {
	if observation.Client == nil || !slices.Contains(observation.Status.Capabilities, kwtdaemon.CapabilityInventory) {
		return service.NewError(
			service.DaemonIncompatible,
			"the running kwt daemon does not provide worktree inventory",
			false,
			nil,
			nil,
		)
	}
	return nil
}

func waitInventoryRetry(ctx context.Context, deadline *time.Time) error {
	delay := 50 * time.Millisecond
	if deadline != nil {
		remaining := time.Until(*deadline)
		if remaining <= 0 {
			return service.NewError(
				service.DaemonDraining,
				"the kwt daemon is still draining",
				true,
				map[string]any{"drain_deadline": deadline.Format(time.RFC3339Nano)},
				nil,
			)
		}
		if remaining < delay {
			delay = remaining
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func trustRequirement(err error) (config.TrustRequiredError, error) {
	var typed *service.Error
	if !errors.As(err, &typed) {
		return config.TrustRequiredError{}, err
	}
	requirement := config.TrustRequiredError{
		Path:      detailString(typed.Details, "path"),
		Digest:    detailString(typed.Details, "digest"),
		Preview:   detailString(typed.Details, "preview"),
		Size:      detailInt(typed.Details, "size"),
		Truncated: detailBool(typed.Details, "truncated"),
	}
	if detailString(typed.Details, "kind") != "repository_config_trust" ||
		requirement.Path == "" || requirement.Digest == "" {
		return config.TrustRequiredError{}, service.NewError(
			service.DaemonTransportFailed,
			"kwt daemon returned an incomplete trust requirement",
			false,
			nil,
			err,
		)
	}
	return requirement, nil
}

func detailString(details map[string]any, key string) string {
	value, _ := details[key].(string)
	return value
}

func detailBool(details map[string]any, key string) bool {
	value, _ := details[key].(bool)
	return value
}

func detailInt(details map[string]any, key string) int {
	switch value := details[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return 0
	}
}

func writeConfigNotes(stderr io.Writer, notes []kwt.Note, interactive, declined bool) {
	for _, note := range notes {
		if note.Code == "trust_store_unavailable" {
			_, _ = fmt.Fprintf(
				stderr,
				"kwt: failed to load trust store %s (continuing empty)\n",
				note.Path,
			)
			continue
		}
		if note.Code == "unsafe_config_skipped" {
			_, _ = fmt.Fprintf(stderr, "kwt: skipping unsafe local config %s\n", note.Path)
			continue
		}
		if note.Code != "untrusted_config_skipped" {
			continue
		}
		if interactive && declined {
			_, _ = fmt.Fprintf(stderr, "kwt: local config %s not trusted, skipping\n", note.Path)
			continue
		}
		_, _ = fmt.Fprintf(stderr, "kwt: skipping untrusted local config %s (non-interactive session)\n", note.Path)
	}
}
