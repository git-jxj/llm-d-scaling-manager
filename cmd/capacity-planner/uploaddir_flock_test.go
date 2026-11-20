//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A server that starts removes the upload directories a stopped server left
// in TMPDIR (a container restarted keeps its emptyDir), and none a running
// server holds. Before, a stopped server's uploads stayed until the Pod went.
func TestUploadStoreSweepsAStoppedServersUploads(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	// Windows reads TMP.
	t.Setenv("TMP", tmp)
	stale := filepath.Join(tmp, uploadDirPrefix+"stale")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "upload-1"), []byte("left behind"), 0o600))
	other := filepath.Join(tmp, "other")
	require.NoError(t, os.Mkdir(other, 0o700))

	running := newUploads(t, io.Discard)
	assert.Equal(t, tmp, filepath.Dir(running.dir))
	assert.NoDirExists(t, stale, "a stopped server's directory is removed")
	assert.DirExists(t, other, "a directory not a store's is left")

	// A second server in the same TMPDIR leaves the first one's alone.
	require.NoError(t, os.WriteFile(filepath.Join(running.dir, "upload-2"), []byte("live"), 0o600))
	second := newUploads(t, io.Discard)
	assert.NotEqual(t, running.dir, second.dir)
	assert.FileExists(t, filepath.Join(running.dir, "upload-2"), "a running server's directory is kept")

	// Stopped, its directory goes; one whose server died without Close is
	// swept by the next to start.
	require.NoError(t, running.lock.Close())
	third := newUploads(t, io.Discard)
	assert.NoDirExists(t, running.dir, "a dead server's directory is removed")
	assert.DirExists(t, second.dir)
	assert.DirExists(t, third.dir)
}
