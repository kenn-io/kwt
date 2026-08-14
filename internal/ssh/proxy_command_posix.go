//go:build !windows

package ssh

func quoteProxyArgument(value string) string { return shellQuote(value) }

func proxyFailureCommand() string { return "/usr/bin/false" }
