// Command testharness runs the repository's Go tests inside the test isolation
// boundary.
package main

import (
	"context"
	"os"

	"go.kenn.io/kwt/internal/testharness"
)

func main() {
	os.Exit(testharness.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
