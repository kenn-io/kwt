package ssh

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/kwt/service"
)

// OpenSSH writes Tailscale's browser check to stderr, not SSH_ASKPASS.
// Deliver it while the process is alive; a final diagnostic arrives too late.
type browserAuthenticationWriter struct {
	notify   func(string) error
	cancel   context.CancelCauseFunc
	pending  string
	notified bool
	timer    *time.Timer
}

func (w *browserAuthenticationWriter) Write(value []byte) (int, error) {
	if w.notified {
		return len(value), nil
	}
	w.pending += string(value)
	for {
		line, rest, found := strings.Cut(w.pending, "\n")
		if !found {
			return len(value), nil
		}
		w.pending = rest
		link, found := strings.CutPrefix(strings.TrimSpace(line), "# To authenticate, visit: ")
		if !found {
			continue
		}
		parsed, err := url.Parse(link)
		if err != nil || parsed.Scheme != "https" || parsed.Host != "login.tailscale.com" ||
			parsed.User != nil || !strings.HasPrefix(parsed.Path, "/a/") || len(parsed.Path) <= len("/a/") {
			continue
		}
		w.notified = true
		w.pending = ""
		if err := w.notify(link); err != nil {
			w.cancel(err)
			return len(value), err
		}
		return len(value), nil
	}
}

func newBrowserAuthenticationWriter(ctx context.Context, request LeaseRequest, target ResolvedTarget, cancel context.CancelCauseFunc) *browserAuthenticationWriter {
	w := &browserAuthenticationWriter{cancel: cancel}
	w.notify = func(link string) error {
		details := map[string]any{
			"logical_target":     promptTargetDetails(target.LogicalTarget),
			"effective_target":   promptTargetDetails(target.EffectiveTarget),
			"display_target":     target.DisplayTarget,
			"hop_index":          request.promptTargetIndex,
			"hop_count":          max(1, request.promptTargetCount),
			"method":             "browser",
			"authentication_url": link,
		}
		if request.Prompt == nil {
			return service.NewError(service.SSHInteractionRequired, "SSH requires browser authentication", false, details, nil)
		}
		deadline := time.Now().Add(defaultPromptTimeout)
		w.timer = time.AfterFunc(defaultPromptTimeout, func() {
			w.cancel(service.NewError(service.SSHPromptTimedOut, "SSH browser authentication timed out", false, nil, nil))
		})
		promptContext, cancelPrompt := context.WithDeadline(ctx, deadline)
		defer cancelPrompt()
		_, err := request.Prompt(promptContext, service.OperationPrompt{
			Kind:     "ssh_browser_authentication",
			Message:  "Tailscale SSH requires an additional check. To authenticate, visit: " + link,
			Deadline: &deadline,
			Details:  details,
		})
		if errors.Is(err, context.DeadlineExceeded) {
			return service.NewError(service.SSHPromptTimedOut, "SSH browser authentication timed out", false, nil, err)
		}
		if err != nil && context.Cause(ctx) != nil {
			return context.Cause(ctx)
		}
		return err
	}
	return w
}

func (w *browserAuthenticationWriter) close() {
	if w.timer != nil {
		w.timer.Stop()
	}
}
