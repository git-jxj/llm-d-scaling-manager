package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// version prints the release the binary was linked with and its commit, the
// line the release checks read; it takes no argument.
func TestVersion(t *testing.T) {
	was := version
	t.Cleanup(func() { version = was })
	version = "v9.8.7"
	var out bytes.Buffer
	require.NoError(t, run(context.Background(), []string{"version"}, &out, io.Discard))
	assert.Equal(t, "capacity-planner v9.8.7 (commit "+learnrec.Commit()+")\n", out.String())
	err := run(context.Background(), []string{"version", "extra"}, io.Discard, io.Discard)
	assert.Equal(t, 2, exitCode(err), "%v", err)
}
