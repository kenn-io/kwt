//go:build windows

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestPasswordConsoleModeDisablesEcho(t *testing.T) {
	original := uint32(
		windows.ENABLE_ECHO_INPUT |
			windows.ENABLE_LINE_INPUT |
			windows.ENABLE_PROCESSED_INPUT,
	)
	mode := passwordConsoleMode(original)

	assert.Zero(t, mode&windows.ENABLE_ECHO_INPUT)
	assert.Equal(t, original&^uint32(windows.ENABLE_ECHO_INPUT), mode)
}
