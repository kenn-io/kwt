package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/kwt/internal/config"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
	"go.kenn.io/kwt/service"
	publicworktree "go.kenn.io/kwt/worktree"
)

var queryCLIInventory = queryInventoryForCLI

func queryInventoryForCLI(
	ctx context.Context,
	request publicworktree.Request,
	interactive bool,
	stderr io.Writer,
) (publicworktree.Result, error) {
	controller, err := newDaemonController()
	if err != nil {
		return publicworktree.Result{}, err
	}
	if interactive {
		request.UntrustedConfig = publicworktree.RequireConfigInteraction
	} else {
		request.UntrustedConfig = publicworktree.IgnoreUntrustedConfig
	}
	declined := false
	for {
		result, queryErr := queryDaemonInventory(ctx, controller, request)
		if queryErr == nil {
			writeConfigNotes(stderr, result.Notes, interactive, declined)
			return result, nil
		}
		if !interactive || !service.IsCode(queryErr, service.InteractionRequired) {
			return publicworktree.Result{}, queryErr
		}
		requirement, requirementErr := trustRequirement(queryErr)
		if requirementErr != nil {
			return publicworktree.Result{}, requirementErr
		}
		approved, promptErr := config.PromptTrustRequirement(requirement)
		if promptErr != nil {
			return publicworktree.Result{}, promptErr
		}
		if !approved {
			declined = true
			request.UntrustedConfig = publicworktree.IgnoreUntrustedConfig
			continue
		}
		observation, startErr := controller.Start(ctx)
		if startErr != nil {
			return publicworktree.Result{}, startErr
		}
		if err := requireInventoryCapability(observation); err != nil {
			return publicworktree.Result{}, err
		}
		if err := observation.Client.ApproveConfig(ctx, publicworktree.ConfigApproval{
			Path: requirement.Path, Digest: requirement.Digest,
		}); err != nil {
			return publicworktree.Result{}, err
		}
		request.UntrustedConfig = publicworktree.RequireConfigInteraction
	}
}

func queryDaemonInventory(
	ctx context.Context,
	controller daemonController,
	request publicworktree.Request,
) (publicworktree.Result, error) {
	for {
		observation, err := controller.Start(ctx)
		if err != nil {
			return publicworktree.Result{}, err
		}
		if err := requireInventoryCapability(observation); err != nil {
			return publicworktree.Result{}, err
		}
		result, err := observation.Client.Inventory(ctx, request)
		deadline, draining := inventoryDrainDeadline(err)
		if !draining {
			return result, err
		}
		if err := waitInventoryRetry(ctx, deadline); err != nil {
			return publicworktree.Result{}, err
		}
	}
}

func inventoryDrainDeadline(err error) (*time.Time, bool) {
	var typed *service.Error
	if !errors.As(err, &typed) || typed.Code != service.Busy {
		return nil, false
	}
	switch deadline := typed.Details["drain_deadline"].(type) {
	case time.Time:
		return &deadline, true
	case *time.Time:
		return deadline, deadline != nil
	default:
		return nil, false
	}
}

func requireInventoryCapability(observation kwtdaemon.Observation) error {
	if observation.Client == nil || !slices.Contains(observation.Status.Capabilities, kwtdaemon.CapabilityInventory) {
		return service.NewError(
			service.Unsupported,
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
			return service.NewError(service.Busy, "the kwt daemon is still draining", true, nil, nil)
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
			service.TransportFailure,
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

func writeConfigNotes(stderr io.Writer, notes []publicworktree.Note, interactive, declined bool) {
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
