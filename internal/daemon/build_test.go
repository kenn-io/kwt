package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompareBuilds(t *testing.T) {
	tests := []struct {
		name                  string
		invoking              Build
		running               Build
		runningAdvertisedTime bool
		want                  BuildOrder
	}{
		{
			name: "same full revision",
			invoking: Build{
				Version: "sha-a", Revision: strings.Repeat("a", 40),
			},
			running: Build{
				Version: "different-display", Revision: strings.Repeat("a", 40),
			},
			runningAdvertisedTime: true,
			want:                  BuildSame,
		},
		{
			name: "newer semantic version",
			invoking: Build{
				Version: "v1.3.0", Revision: "new",
			},
			running: Build{
				Version: "v1.2.0", Revision: "old",
			},
			runningAdvertisedTime: true,
			want:                  BuildNewer,
		},
		{
			name: "older semantic version",
			invoking: Build{
				Version: "v1.2.0", Revision: "old",
			},
			running: Build{
				Version: "v1.3.0", Revision: "new",
			},
			runningAdvertisedTime: true,
			want:                  BuildOlder,
		},
		{
			name: "semantic build metadata falls through to revision time",
			invoking: Build{
				Version: "v1.2.3+one", Revision: "new",
				RevisionTime: "2026-08-09T12:00:01Z",
			},
			running: Build{
				Version: "v1.2.3+two", Revision: "old",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			runningAdvertisedTime: true,
			want:                  BuildNewer,
		},
		{
			name: "same module version without VCS identity is same",
			invoking: Build{
				Version: "v1.2.3", Revision: "none",
			},
			running: Build{
				Version: "v1.2.3", Revision: "none",
			},
			runningAdvertisedTime: true,
			want:                  BuildSame,
		},
		{
			name: "same module version with conflicting revisions is unknown",
			invoking: Build{
				Version: "v1.2.3", Revision: "left",
			},
			running: Build{
				Version: "v1.2.3", Revision: "right",
			},
			runningAdvertisedTime: true,
			want:                  BuildUnknown,
		},
		{
			name: "newer revision time",
			invoking: Build{
				Version: "new", Revision: "new",
				RevisionTime: "2026-08-09T12:00:01Z",
			},
			running: Build{
				Version: "old", Revision: "old",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			runningAdvertisedTime: true,
			want:                  BuildNewer,
		},
		{
			name: "older revision time",
			invoking: Build{
				Version: "old", Revision: "old",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			running: Build{
				Version: "new", Revision: "new",
				RevisionTime: "2026-08-09T12:00:01Z",
			},
			runningAdvertisedTime: true,
			want:                  BuildOlder,
		},
		{
			name: "equal time different revisions is unknown",
			invoking: Build{
				Version: "new", Revision: "new",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			running: Build{
				Version: "old", Revision: "old",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			runningAdvertisedTime: true,
			want:                  BuildUnknown,
		},
		{
			name: "new metadata beats legacy omission",
			invoking: Build{
				Version: "new", Revision: "new",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			running: Build{
				Version: "old", Revision: "old",
			},
			runningAdvertisedTime: false,
			want:                  BuildNewer,
		},
		{
			name: "metadata-less client is older",
			invoking: Build{
				Version: "old", Revision: "old",
			},
			running: Build{
				Version: "new", Revision: "new",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			runningAdvertisedTime: true,
			want:                  BuildOlder,
		},
		{
			name: "explicit unknown time does not trigger transition order",
			invoking: Build{
				Version: "left", Revision: "left",
			},
			running: Build{
				Version: "right", Revision: "right",
			},
			runningAdvertisedTime: true,
			want:                  BuildUnknown,
		},
		{
			name: "invalid invoking time is unknown",
			invoking: Build{
				Version: "left", Revision: "left", RevisionTime: "not-a-time",
			},
			running: Build{
				Version: "right", Revision: "right",
				RevisionTime: "2026-08-09T12:00:00Z",
			},
			runningAdvertisedTime: true,
			want:                  BuildUnknown,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, CompareBuilds(
				test.invoking,
				test.running,
				test.runningAdvertisedTime,
			))
		})
	}
}
