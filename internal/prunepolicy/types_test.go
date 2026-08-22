package prunepolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfirmationReasonsHaveDistinctSummaryAndExitSemantics(t *testing.T) {
	tests := []struct {
		reason      Reason
		wouldRemove int
		skipped     int
		exitCode    int
	}{
		{WouldRequireConfirmation, 1, 0, 0},
		{ConfirmationDeclined, 0, 1, 0},
		{ConfirmationRequired, 0, 1, 1},
	}
	for _, test := range tests {
		t.Run(string(test.reason), func(t *testing.T) {
			report := Report{Outcomes: []Outcome{{Reason: test.reason}}}
			report.Finalize()
			assert.Equal(t, test.wouldRemove, report.Summary.WouldRemove)
			assert.Equal(t, test.skipped, report.Summary.Skipped)
			assert.Equal(t, test.exitCode, report.ExitCode())
		})
	}
	assert.Equal(t, 2, SchemaVersion)
}
