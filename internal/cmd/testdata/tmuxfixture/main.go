package main

import (
	"fmt"
	"os"
)

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "-L" {
		args = args[2:]
	}
	if len(args) > 0 && args[0] == "list-sessions" {
		_, _ = fmt.Fprintln(os.Stderr, "no server running on test socket")
		os.Exit(1)
	}
	_, _ = fmt.Fprintf(os.Stderr, "unexpected tmux fixture arguments: %q\n", args)
	os.Exit(2)
}
