package rex

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// A terminal data stream starts with a Ghostty snapshot, then carries the
// program's output. Rex adds its own messages in band as
// ESC _ rex1;<nonce>;<json> ESC \, using the nonce from the stream's
// WELCOME. The same sequence with another nonce was printed by a program
// and stays in the output.

type pieceKind int

const (
	pieceSnapshot pieceKind = iota
	pieceOutput
	pieceMessage
)

type piece struct {
	kind    pieceKind
	bytes   []byte
	message message
}

// message is one of Rex's in-band stream messages.
type message struct {
	Type     string `json:"type"`
	ExitCode *int   `json:"exit_code"`
}

var messageEnd = []byte("\x1b\\")

type streamFilter struct {
	marker     []byte
	pending    []byte
	inSnapshot bool
}

func newStreamFilter(nonce uint64) *streamFilter {
	return &streamFilter{marker: fmt.Appendf(nil, "\x1b_rex1;%d;", nonce), inSnapshot: true}
}

// push returns the pieces that the bytes read so far complete. It holds back
// a tail that could begin a Rex message.
func (f *streamFilter) push(data []byte) []piece {
	f.pending = append(f.pending, data...)
	var pieces []piece
	for {
		at := bytes.Index(f.pending, f.marker)
		if at < 0 {
			ready := len(f.pending) - partialTail(f.pending, f.marker)
			pieces = f.emit(pieces, f.pending[:ready])
			f.pending = append([]byte(nil), f.pending[ready:]...)
			return pieces
		}
		bodyStart := at + len(f.marker)
		length := bytes.Index(f.pending[bodyStart:], messageEnd)
		if length < 0 {
			pieces = f.emit(pieces, f.pending[:at])
			f.pending = append([]byte(nil), f.pending[at:]...)
			return pieces
		}
		pieces = f.emit(pieces, f.pending[:at])
		var m message
		// An unreadable message still ends the snapshot.
		_ = json.Unmarshal(f.pending[bodyStart:bodyStart+length], &m)
		f.inSnapshot = false
		pieces = append(pieces, piece{kind: pieceMessage, message: m})
		f.pending = append([]byte(nil), f.pending[bodyStart+length+len(messageEnd):]...)
	}
}

func (f *streamFilter) emit(pieces []piece, data []byte) []piece {
	if len(data) == 0 {
		return pieces
	}
	kind := pieceOutput
	if f.inSnapshot {
		kind = pieceSnapshot
	}
	return append(pieces, piece{kind: kind, bytes: append([]byte(nil), data...)})
}

// partialTail reports how many bytes at the end of data begin marker.
func partialTail(data, marker []byte) int {
	for n := min(len(marker)-1, len(data)); n > 0; n-- {
		if bytes.HasSuffix(data, marker[:n]) {
			return n
		}
	}
	return 0
}
