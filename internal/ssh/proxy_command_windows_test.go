//go:build windows

package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestProxyCommandWindowsRoundTripsControlPath(t *testing.T) {
	controlPath := `C:\Users\User Name\kwt\control "quoted"`
	command := proxyCommand(
		Target{User: "relay", Hostname: "relay.example.test", Port: 2222},
		[]string{"-S", controlPath},
	)
	arguments, err := windows.DecomposeCommandLine(command)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(arguments), 3)
	assert.Equal(t, "ssh", arguments[0])
	for index, argument := range arguments {
		if argument == "-S" && index+1 < len(arguments) {
			assert.Equal(t, controlPath, arguments[index+1])
			return
		}
	}
	t.Fatal("proxy command did not contain a control path")
}
