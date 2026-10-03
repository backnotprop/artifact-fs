//go:build !windows

package main

import (
	"context"
	"os"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/cli"
)

// TestMain lets this test binary stand in for artifact-fs where git runs it.
// The fsmonitor hook ArtifactFS installs execs os.Executable(), which is the
// test binary here, so answer the hook as the real CLI would.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fsmonitor-hook" {
		os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}
