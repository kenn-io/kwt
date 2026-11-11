//go:build !windows

package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/kwt/internal/rex"
	"golang.org/x/term"
)

// watchTerminalSize reports the terminal's size after each SIGWINCH.
func watchTerminalSize(ctx context.Context, fd int) (<-chan rex.Size, func()) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	sizes := make(chan rex.Size, 1)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				columns, rows, err := term.GetSize(fd)
				if err != nil {
					continue
				}
				select {
				case sizes <- rex.Size{Columns: columns, Rows: rows}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return sizes, func() {
		signal.Stop(signals)
		cancel()
	}
}
