//go:build windows

package cmd

import (
	"context"

	"go.kenn.io/kwt/internal/rex"
)

// Windows consoles send no SIGWINCH. Rex servers do not run on Windows yet,
// so an attach there keeps its first size.
func watchTerminalSize(context.Context, int) (<-chan rex.Size, func()) {
	return nil, func() {}
}
