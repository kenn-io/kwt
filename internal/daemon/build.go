package daemon

import (
	"encoding/hex"
	"time"

	"golang.org/x/mod/semver"
)

// BuildOrder describes the invoking build relative to a running daemon.
type BuildOrder uint8

const (
	BuildUnknown BuildOrder = iota
	BuildSame
	BuildNewer
	BuildOlder
)

// CompareBuilds conservatively orders an invoking build against a running
// daemon. Hashes prove equality only; source time or semantic version supplies
// order.
func CompareBuilds(
	invoking Build,
	running Build,
	runningAdvertisedTime bool,
) BuildOrder {
	if fullRevision(invoking.Revision) && invoking.Revision == running.Revision {
		return BuildSame
	}
	if left, leftOK := comparableVersion(invoking.Version); leftOK {
		if right, rightOK := comparableVersion(running.Version); rightOK && left != right {
			if semver.Compare(left, right) > 0 {
				return BuildNewer
			}
			return BuildOlder
		}
	}
	leftTime, leftOK := parseRevisionTime(invoking.RevisionTime)
	rightTime, rightOK := parseRevisionTime(running.RevisionTime)
	if leftOK && rightOK && !leftTime.Equal(rightTime) {
		if leftTime.After(rightTime) {
			return BuildNewer
		}
		return BuildOlder
	}
	if leftOK && !runningAdvertisedTime {
		return BuildNewer
	}
	if invoking.RevisionTime == "" && rightOK {
		return BuildOlder
	}
	return BuildUnknown
}

func fullRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func parseRevisionTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339) != value {
		return time.Time{}, false
	}
	return parsed, true
}
