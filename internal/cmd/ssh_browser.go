package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kwt/service"
)

var openSSHBrowser = launchSSHBrowser

func presentSSHBrowserPrompt(ctx context.Context, cmd *cobra.Command, prompt service.OperationPrompt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.ErrOrStderr(), terminalSafeSSHPrompt(prompt.Message)); err != nil {
		return err
	}
	if sshOpenBrowser {
		return openSSHBrowserPrompt(ctx, cmd, prompt)
	}
	return nil
}

func openSSHBrowserPrompt(ctx context.Context, cmd *cobra.Command, prompt service.OperationPrompt) error {
	link, _ := prompt.Details["authentication_url"].(string)
	if link == "" {
		return service.NewError(service.InvalidRequest, "SSH browser prompt has no authentication URL", false, nil, nil)
	}
	if err := openSSHBrowser(ctx, link); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Could not open the browser. Open the URL manually: "+terminalSafeSSHPrompt(link))
		return err
	}
	return nil
}

func launchSSHBrowser(ctx context.Context, link string) error {
	command := sshBrowserCommand(runtime.GOOS, os.Getenv)
	if len(command) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, command[0], append(command[1:], link)...).Run()
}

func sshBrowserCommand(platform string, getenv func(string) string) []string {
	if getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" || getenv("SSH_CLIENT") != "" {
		return nil
	}
	switch platform {
	case "darwin":
		return []string{"open"}
	case "linux":
		if getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != "" {
			return []string{"xdg-open"}
		}
	case "windows":
		if !strings.EqualFold(getenv("SESSIONNAME"), "Services") {
			return []string{"rundll32", "url.dll,FileProtocolHandler"}
		}
	}
	return nil
}
