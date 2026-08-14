//go:build windows

package ssh

import "golang.org/x/sys/windows"

func quoteProxyArgument(value string) string { return windows.EscapeArg(value) }

func proxyFailureCommand() string { return "cmd.exe /d /c exit 255" }
