//go:build linux

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLinuxProcessStartTicks(t *testing.T) {
	ticks, err := parseLinuxProcessStartTicks(
		[]byte("42 (kwt daemon (worker)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 98765 20"),
	)
	require.NoError(t, err)
	assert.Equal(t, "98765", ticks)
}
