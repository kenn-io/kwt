//go:build windows

package cmd

import (
	"errors"
	"os"

	"github.com/muesli/cancelreader"
	"golang.org/x/sys/windows"
)

func newTerminalPasswordReader(terminal *os.File) (terminalPasswordReader, error) {
	handle := windows.Handle(terminal.Fd())
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		return nil, err
	}
	if err := windows.SetConsoleMode(handle, passwordConsoleMode(original)); err != nil {
		return nil, err
	}
	reader, err := cancelreader.NewReader(terminal)
	if err != nil {
		return nil, errors.Join(err, windows.SetConsoleMode(handle, original))
	}
	return &windowsTerminalPasswordReader{
		CancelReader: reader,
		handle:       handle,
		originalMode: original,
	}, nil
}

func passwordConsoleMode(mode uint32) uint32 {
	return mode &^ windows.ENABLE_ECHO_INPUT
}

type windowsTerminalPasswordReader struct {
	cancelreader.CancelReader
	handle       windows.Handle
	originalMode uint32
}

func (r *windowsTerminalPasswordReader) Close() error {
	return errors.Join(
		r.CancelReader.Close(),
		windows.SetConsoleMode(r.handle, r.originalMode),
	)
}
