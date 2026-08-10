package cmd

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCurrentBuildInfoUsesTheSameVersionSourceAsVersionOutput(t *testing.T) {
	info := currentBuildInfo()
	assert.NotEmpty(t, info.Version)
	assert.Equal(t, getVersionString(), info.Display)
}

func TestCurrentBuildInfoSeparatesSourceRevisionTimeFromBuildDate(t *testing.T) {
	oldVersion, oldCommit, oldDate, oldRevisionTime := version, commit, date, revisionTime
	oldReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version, commit, date, revisionTime = oldVersion, oldCommit, oldDate, oldRevisionTime
		readBuildInfo = oldReadBuildInfo
	})
	version = "sha-build"
	commit = "explicit-revision"
	date = "2026-08-09T13:00:00Z"
	revisionTime = "2026-08-09T12:00:00Z"
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "embedded-revision"},
			{Key: "vcs.time", Value: "2026-08-09T11:00:00Z"},
		}}, true
	}

	info := currentBuildInfo()
	assert.Equal(t, "explicit-revision", info.Revision)
	assert.Equal(t, "2026-08-09T12:00:00Z", info.RevisionTime)
	assert.Equal(t, "2026-08-09T13:00:00Z", info.Date)
}

func TestCurrentBuildInfoUsesVCSTimeForUnstampedRevisionTime(t *testing.T) {
	oldVersion, oldCommit, oldDate, oldRevisionTime := version, commit, date, revisionTime
	oldReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version, commit, date, revisionTime = oldVersion, oldCommit, oldDate, oldRevisionTime
		readBuildInfo = oldReadBuildInfo
	})
	version, commit, date, revisionTime = "dev", "none", "unknown", ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "v1.2.3"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "embedded-revision"},
				{Key: "vcs.time", Value: "2026-08-09T12:00:00Z"},
			},
		}, true
	}

	info := currentBuildInfo()
	assert.Equal(t, "embedded-revision", info.Revision)
	assert.Equal(t, "2026-08-09T12:00:00Z", info.RevisionTime)
	assert.Equal(t, "2026-08-09T12:00:00Z", info.Date)
}

func TestCurrentBuildInfoRejectsVCSTimeForMismatchedExplicitRevision(t *testing.T) {
	oldVersion, oldCommit, oldDate, oldRevisionTime := version, commit, date, revisionTime
	oldReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version, commit, date, revisionTime = oldVersion, oldCommit, oldDate, oldRevisionTime
		readBuildInfo = oldReadBuildInfo
	})
	version, commit, date, revisionTime = "sha-build", "explicit-revision", "unknown", ""
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "ambient-revision"},
			{Key: "vcs.time", Value: "2026-08-09T12:00:00Z"},
		}}, true
	}

	info := currentBuildInfo()
	assert.Equal(t, "explicit-revision", info.Revision)
	assert.Empty(t, info.RevisionTime)
	assert.Equal(t, "2026-08-09T12:00:00Z", info.Date)
}

func TestCurrentBuildInfoRejectsOrderingIdentityForModifiedSource(t *testing.T) {
	oldVersion, oldCommit, oldDate, oldRevisionTime := version, commit, date, revisionTime
	oldReadBuildInfo := readBuildInfo
	t.Cleanup(func() {
		version, commit, date, revisionTime = oldVersion, oldCommit, oldDate, oldRevisionTime
		readBuildInfo = oldReadBuildInfo
	})
	version = "sha-build"
	commit = "0123456789abcdef0123456789abcdef01234567"
	date = "unknown"
	revisionTime = "2026-08-09T12:00:00Z"
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: commit},
			{Key: "vcs.time", Value: revisionTime},
			{Key: "vcs.modified", Value: "true"},
		}}, true
	}

	info := currentBuildInfo()
	assert.Equal(t, commit+"-dirty", info.Revision)
	assert.Empty(t, info.RevisionTime)
	assert.Equal(t, "2026-08-09T12:00:00Z", info.Date)
}
