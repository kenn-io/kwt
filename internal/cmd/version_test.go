package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCurrentBuildInfoUsesTheSameVersionSourceAsVersionOutput(t *testing.T) {
	info := currentBuildInfo()
	assert.NotEmpty(t, info.Version)
	assert.Equal(t, getVersionString(), info.Display)
}
