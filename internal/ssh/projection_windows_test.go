//go:build windows

package ssh

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/openssh"
)

func TestProjectionUsesWindowsNullDevice(t *testing.T) {
	projected, err := projectConfig(openssh.EffectiveConfig{})
	require.NoError(t, err)
	assert.Equal(t, []string{"-F", os.DevNull}, projected.Arguments[:2])
}
