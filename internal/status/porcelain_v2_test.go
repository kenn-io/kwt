package status

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePorcelainV2OrdinaryAndUntrackedRecords(t *testing.T) {
	input := []byte(
		"1 M. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb staged.txt\x00" +
			"1 .M N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb worktree.txt\x00" +
			"1 AM N... 000000 100644 100644 0000000000000000000000000000000000000000 bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb added-and-edited.txt\x00" +
			"1 .D N... 100644 100644 000000 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb deleted.txt\x00" +
			"? untracked.txt\x00",
	)

	got, err := parseChangePorcelainV2(input)

	require.NoError(t, err)
	assert.Equal(t, []FileChange{
		{Path: "staged.txt", Index: FileStateModified},
		{Path: "worktree.txt", Worktree: FileStateModified},
		{
			Path:     "added-and-edited.txt",
			Index:    FileStateAdded,
			Worktree: FileStateModified,
		},
		{Path: "deleted.txt", Worktree: FileStateDeleted},
		{Path: "untracked.txt", Worktree: FileStateUntracked},
	}, got)
}

func TestParsePorcelainV2RenameAndCopyRecords(t *testing.T) {
	input := []byte(
		"2 R. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb R100 renamed.txt\x00original.txt\x00" +
			"2 C. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb C075 copied.txt\x00source.txt\x00",
	)

	got, err := parseChangePorcelainV2(input)

	require.NoError(t, err)
	assert.Equal(t, []FileChange{
		{
			Path:         "renamed.txt",
			OriginalPath: "original.txt",
			Index:        FileStateRenamed,
		},
		{
			Path:         "copied.txt",
			OriginalPath: "source.txt",
			Index:        FileStateCopied,
		},
	}, got)
}

func TestParsePorcelainV2AllUnmergedCodes(t *testing.T) {
	for _, code := range []string{"DD", "AU", "UD", "UA", "DU", "AA", "UU"} {
		t.Run(code, func(t *testing.T) {
			input := []byte(fmt.Sprintf(
				"u %s N... 100644 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccc conflict-%s.txt\x00",
				code,
				code,
			))

			got, err := parseChangePorcelainV2(input)

			require.NoError(t, err)
			assert.Equal(t, []FileChange{{
				Path:     "conflict-" + code + ".txt",
				Index:    FileStateConflicted,
				Worktree: FileStateConflicted,
			}}, got)
		})
	}
}

func TestParsePorcelainV2PreservesUnusualPathBytes(t *testing.T) {
	path := []byte{'o', 'd', 'd', '\t', 'l', 'i', 'n', 'e', '\n', 0xff, '.', 't', 'x', 't'}
	input := append(
		[]byte("1 .M N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb "),
		path...,
	)
	input = append(input, 0)

	got, err := parseChangePorcelainV2(input)

	require.NoError(t, err)
	assert.Equal(t, string(path), got[0].Path)
	assert.Equal(t, path, []byte(got[0].Path))
}

func TestParsePorcelainV2RejectsMalformedOrUnknownRecords(t *testing.T) {
	ordinaryPrefix := "1 M. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb "
	renamePrefix := "2 R. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb "
	unmergedPrefix := "u MM N... 100644 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb cccccccccccccccccccccccccccccccccccccccc conflict.txt\x00"
	tests := []struct {
		name  string
		input []byte
	}{
		{name: "missing NUL terminator", input: []byte(ordinaryPrefix + "file.txt")},
		{name: "empty record", input: []byte{0}},
		{name: "truncated ordinary", input: []byte("1 M. file.txt\x00")},
		{name: "corrupt ordinary tag", input: []byte("1x M. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb file.txt\x00")},
		{name: "unknown ordinary state", input: []byte("1 Z. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb file.txt\x00")},
		{name: "invalid submodule field", input: []byte("1 M. BAD. 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb file.txt\x00")},
		{name: "invalid mode", input: []byte("1 M. N... 10064x 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb file.txt\x00")},
		{name: "invalid object ID", input: []byte("1 M. N... 100644 100644 100644 not-an-object-id bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb file.txt\x00")},
		{name: "unknown unmerged code", input: []byte(unmergedPrefix)},
		{name: "rename missing original path", input: []byte(renamePrefix + "R100 renamed.txt\x00")},
		{name: "invalid rename score", input: []byte(renamePrefix + "X100 renamed.txt\x00original.txt\x00")},
		{name: "mismatched copy score", input: []byte(renamePrefix + "C100 renamed.txt\x00original.txt\x00")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChangePorcelainV2(tt.input)

			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

func TestParsePorcelainV2SkipsUnknownFramedRecords(t *testing.T) {
	input := []byte(
		"1 M. N... 100644 100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb staged.txt\x00" +
			"x future fields remain opaque\x00" +
			"! ignored-by-current-command.txt\x00" +
			"? untracked.txt\x00",
	)

	got, err := parseChangePorcelainV2(input)

	require.NoError(t, err)
	assert.Equal(t, []FileChange{
		{Path: "staged.txt", Index: FileStateModified},
		{Path: "untracked.txt", Worktree: FileStateUntracked},
	}, got)
}
