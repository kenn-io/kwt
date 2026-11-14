package rex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type split struct {
	snapshot []byte
	output   []byte
	messages []message
}

func collect(into *split, pieces []piece) {
	for _, p := range pieces {
		switch p.kind {
		case pieceSnapshot:
			into.snapshot = append(into.snapshot, p.bytes...)
		case pieceOutput:
			into.output = append(into.output, p.bytes...)
		case pieceMessage:
			into.messages = append(into.messages, p.message)
		}
	}
}

func TestStreamFilterSeparatesSnapshotOutputAndRexMessages(t *testing.T) {
	stream := []byte("GHOSTSNP\x01\x00\x1b" +
		"\x1b_rex1;42;{\"type\":\"resize\",\"cols\":80}\x1b\\" +
		"hello\x1b_rex1;7;{}\x1b\\ there" +
		"\x1b_rex1;42;{\"type\":\"child_exited\",\"exit_code\":3}\x1b\\")

	var whole split
	collect(&whole, newStreamFilter(42).push(stream))

	// A marker split across reads must still parse.
	var bytewise split
	filter := newStreamFilter(42)
	for _, b := range stream {
		collect(&bytewise, filter.push([]byte{b}))
	}

	for _, got := range []split{whole, bytewise} {
		assert.Equal(t, "GHOSTSNP\x01\x00\x1b", string(got.snapshot))
		assert.Equal(t, "hello\x1b_rex1;7;{}\x1b\\ there", string(got.output),
			"a message with another nonce is program output")
		require.Len(t, got.messages, 2)
		assert.Equal(t, "resize", got.messages[0].Type)
		assert.Equal(t, "child_exited", got.messages[1].Type)
		require.NotNil(t, got.messages[1].ExitCode)
		assert.Equal(t, 3, *got.messages[1].ExitCode)
	}
}
