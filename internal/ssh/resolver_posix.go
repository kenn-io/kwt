//go:build !windows

package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"

	"go.kenn.io/kit/openssh"
)

func (r *Resolver) resolveConfig(
	ctx context.Context,
	target openssh.Target,
) (openssh.EffectiveConfig, error) {
	loginShell := r.loginShell
	if loginShell == nil {
		loginShell = accountLoginShell
	}
	shell, err := loginShell()
	if err != nil {
		return openssh.EffectiveConfig{}, err
	}
	if shell == "" {
		return openssh.EffectiveConfig{}, errors.New("account login shell is empty")
	}

	resolver := openssh.Resolver{
		Executable: r.executable,
		Run: func(ctx context.Context, argv []string) ([]byte, []byte, int, error) {
			nonce, err := r.nonce()
			if err != nil {
				return nil, nil, -1, err
			}
			if nonce == "" || strings.ContainsAny(nonce, "\x00\r\n") {
				return nil, nil, -1, errors.New("invalid SSH configuration nonce")
			}
			start := "KWT_SSH_CONFIG_START_" + nonce
			end := "KWT_SSH_CONFIG_END_" + nonce
			command := renderFramedResolveCommand(argv, start, end)
			stdout, stderr, exitCode, runErr := r.run(
				ctx,
				[]string{shell, "-lc", command},
				r.environment,
			)
			if runErr != nil {
				return nil, stderr, exitCode, runErr
			}
			framed, frameErr := framedOutput(stdout, start, end)
			if frameErr != nil {
				return nil, stderr, exitCode, frameErr
			}
			return framed, stderr, exitCode, nil
		},
	}
	return resolver.Resolve(ctx, target)
}

func renderFramedResolveCommand(argv []string, start, end string) string {
	quoted := make([]string, len(argv))
	for index, value := range argv {
		quoted[index] = shellQuote(value)
	}
	return "printf '\\n%s\\n' " + shellQuote(start) + "\n" +
		strings.Join(quoted, " ") + "\n" +
		"kwt_ssh_status=$?\n" +
		"printf '\\n%s\\n' " + shellQuote(end) + "\n" +
		"exit \"$kwt_ssh_status\""
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func framedOutput(output []byte, start, end string) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n")
	startIndex := -1
	for index, line := range lines {
		if line == start {
			startIndex = index
			break
		}
	}
	if startIndex < 0 {
		return nil, errors.New("account login shell omitted SSH configuration start marker")
	}
	for index := startIndex + 1; index < len(lines); index++ {
		if lines[index] == end {
			return []byte(strings.Join(lines[startIndex+1:index], "\n")), nil
		}
	}
	return nil, errors.New("account login shell omitted SSH configuration end marker")
}

func accountLoginShell() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current account: %w", err)
	}
	if runtime.GOOS == "darwin" {
		output, commandErr := exec.Command(
			"/usr/bin/dscl", ".", "-read", "/Users/"+current.Username, "UserShell",
		).Output()
		if commandErr == nil {
			if _, value, ok := strings.Cut(strings.TrimSpace(string(output)), ":"); ok {
				if shell := strings.TrimSpace(value); shell != "" {
					return shell, nil
				}
			}
		}
	}
	if runtime.GOOS == "linux" {
		output, commandErr := exec.Command("getent", "passwd", current.Uid).Output()
		if commandErr == nil {
			if shell := passwdShell(string(output)); shell != "" {
				return shell, nil
			}
		}
	}
	contents, readErr := os.ReadFile("/etc/passwd")
	if readErr == nil {
		for line := range strings.Lines(string(contents)) {
			fields := strings.Split(strings.TrimSpace(line), ":")
			if len(fields) >= 7 && fields[2] == current.Uid && fields[6] != "" {
				return fields[6], nil
			}
		}
	}
	return "", errors.New("account login shell could not be determined")
}

func passwdShell(value string) string {
	fields := strings.Split(strings.TrimSpace(value), ":")
	if len(fields) < 7 {
		return ""
	}
	return strings.TrimSpace(fields[6])
}
