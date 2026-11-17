package main

import (
	"context"
	"fmt"
	"io"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// version is the release this binary was built for, set at link time by the
// release (-X main.version=vX.Y.Z); a build from a checkout is "unreleased".
// It must match the controller whose recordings the planner replays: both
// are released under one tag.
var version = "unreleased"

// runVersion prints the release and the commit the binary was built from.
func runVersion(_ context.Context, args []string, out, _ io.Writer) error {
	fs := newFlags("version", out)
	if ok, err := parse(fs, args); !ok {
		return err
	}
	_, err := fmt.Fprintf(out, "%s %s (commit %s)\n", progName, version, learnrec.Commit())
	return err
}
