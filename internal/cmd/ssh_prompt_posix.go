//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package cmd

import (
	"errors"
	"os"

	"github.com/muesli/cancelreader"
	"golang.org/x/sys/unix"
)

func newTerminalPasswordReader(terminal *os.File) (terminalPasswordReader, error) {
	original, err := getTerminalState(terminal)
	if err != nil {
		return nil, err
	}
	passwordState := *original
	passwordState.Lflag &^= unix.ECHO
	passwordState.Lflag |= unix.ICANON | unix.ISIG
	passwordState.Iflag |= unix.ICRNL
	if err := setTerminalState(terminal, &passwordState); err != nil {
		return nil, err
	}
	reader, err := cancelreader.NewReader(terminal)
	if err != nil {
		return nil, errors.Join(err, setTerminalState(terminal, original))
	}
	return &restoringTerminalPasswordReader{
		CancelReader: reader,
		restore: func() error {
			return setTerminalState(terminal, original)
		},
	}, nil
}

func terminalEchoEnabled(terminal *os.File) (bool, error) {
	state, err := getTerminalState(terminal)
	if err != nil {
		return false, err
	}
	return state.Lflag&unix.ECHO != 0, nil
}

type restoringTerminalPasswordReader struct {
	cancelreader.CancelReader
	restore func() error
}

func (r *restoringTerminalPasswordReader) Close() error {
	return errors.Join(r.CancelReader.Close(), r.restore())
}
