package cmd

import (
	"context"
	"io"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kwtdaemon "go.kenn.io/kwt/internal/daemon"
)

func TestServeUsesForegroundModeUnlessDaemonChildIsSet(t *testing.T) {
	oldServe := serveDaemonHost
	oldLoad := loadServeOptions
	oldChild := serveDaemonChild
	t.Cleanup(func() {
		serveDaemonHost = oldServe
		loadServeOptions = oldLoad
		serveDaemonChild = oldChild
	})
	var modes []bool
	loadServeOptions = func() (kwtdaemon.ServeOptions, error) {
		return kwtdaemon.ServeOptions{Home: "/absolute/test-home"}, nil
	}
	serveDaemonHost = func(
		_ context.Context,
		options kwtdaemon.ServeOptions,
	) error {
		modes = append(modes, options.Foreground)
		return nil
	}
	command := &cobra.Command{}
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	serveDaemonChild = false
	require.NoError(t, runServe(command, nil))
	serveDaemonChild = true
	require.NoError(t, runServe(command, nil))
	assert.Equal(t, []bool{true, false}, modes)
}
