//go:build windows

package ssh

import "os/exec"

func configureResolverCommand(*exec.Cmd) {}
