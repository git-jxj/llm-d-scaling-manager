//go:build unix

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makePipe is a named pipe that gives data once to the one reader that opens
// it, as <(cat x.gz) gives a recording.
func makePipe(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.pipe")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	done := make(chan struct{})
	go func() {
		defer close(done)
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = w.Write(data)
		_ = w.Close()
	}()
	t.Cleanup(func() {
		// A pipe no reader opened: open it once so that the writer ends.
		if r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = r.Close()
		}
		<-done
	})
	return path
}

// A --recording that is no regular file, a pipe, is read whole, as before
// passes, and answers as the file does; a directory is named once, as its
// read's error.
func TestRecordingFromAPipeOrADirectory(t *testing.T) {
	gz, err := os.ReadFile(olsSweep)
	require.NoError(t, err)
	args := func(recording string) []string {
		return []string{"slo-to-rate", "--recording", recording, "--model", "model-02", "--tpot", "50ms", "-o", "json"}
	}
	fileOut, fileCode := runCmd(t, args(olsSweep)...)
	require.Equal(t, 0, fileCode, fileOut)

	pipe := makePipe(t, gz)
	pipeOut, pipeCode := runCmd(t, args(pipe)...)
	assert.Equal(t, fileCode, pipeCode, "%s", pipeOut)
	assert.Equal(t, strings.ReplaceAll(fileOut, olsSweep, "REC"), strings.ReplaceAll(pipeOut, pipe, "REC"), "a pipe answered other than its file")

	dir := t.TempDir()
	// The error is the JSON output's, or the returned error's.
	var out bytes.Buffer
	err = run(context.Background(), args(dir), &out, io.Discard)
	require.Error(t, err)
	said := out.String() + err.Error()
	assert.Contains(t, said, "read "+dir+": is a directory")
	assert.Equal(t, 1, strings.Count(said, dir), "the directory named more than once: %s", said)
}
