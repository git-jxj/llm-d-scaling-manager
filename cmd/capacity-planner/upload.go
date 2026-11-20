package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// Bounds on what an upload may hold. A recording is read a line at a time,
// so its replay's memory follows its longest line and its weight, not its
// length; its time follows the bytes gunzipped.
const (
	// maxKeptUploads is how many uploads are kept; the oldest is removed
	// past it.
	maxKeptUploads = 4
	// maxUploadExpansion bounds the bytes an upload gunzips to, as a multiple
	// of --upload-max-size: a day-long recording gunzips about 11 times.
	maxUploadExpansion = 16
	// maxUploadLine is the longest line an upload may hold: a line is held
	// whole while it is read.
	maxUploadLine = 64 << 20
)

// uploadPrefix starts the name of every upload. recordingName refuses a ':',
// so no file in --recordings has a name an upload could take.
const uploadPrefix = "upload:"

// uploadDirPrefix starts the name of every store's directory in TMPDIR.
const uploadDirPrefix = "capacity-planner-uploads-"

// errNoUploadStore is a disk error storing an upload, as the client is told
// it: the error itself names the server's paths, and goes to the server's
// log.
var errNoUploadStore = errors.New("the server could not store the upload")

// uploadStore keeps the recordings uploaded to serve (--allow-upload) in a
// directory of its own, readable by the server's user alone and removed when
// the server stops. An upload is kept under a name the server gives it, from
// its bytes; nothing the client sends names a file.
type uploadStore struct {
	dir string
	// lock is the directory held open, and locked where the system can
	// (lockUploadDir), for as long as the server runs: a server starting
	// later tells a live store's directory from one a stopped server left.
	lock *os.File
	// maxBytes is --upload-max-size.
	maxBytes int64
	warn     io.Writer

	mu sync.Mutex
	// paths is each upload's file by its name, and order its names, oldest
	// first.
	paths map[string]string
	order []string
	// loading counts the loads reading each file, by path; a file pushed
	// out by newer uploads while it loads is removed when its last load
	// ends (evicted).
	loading map[string]int
	evicted map[string]bool
}

// newUploadStore makes the store's directory, in the system's temporary
// directory (TMPDIR), mode 0700. It first removes the directories a server
// that stopped without removing its own left there (a container restarted
// keeps its emptyDir), and none a running server holds.
func newUploadStore(maxBytes int64, warn io.Writer) (*uploadStore, error) {
	sweepUploadDirs(os.TempDir(), warn)
	for range 3 {
		dir, err := os.MkdirTemp("", uploadDirPrefix)
		if err != nil {
			return nil, err
		}
		lock, err := lockUploadDir(dir)
		if err == nil {
			// A server starting at the same moment may have swept the
			// directory between its making and its lock.
			if _, err = os.Stat(dir); err == nil {
				return &uploadStore{dir: dir, lock: lock, maxBytes: maxBytes, warn: warn,
					paths: map[string]string{}, loading: map[string]int{}, evicted: map[string]bool{}}, nil
			}
			_ = lock.Close()
		}
		_ = os.RemoveAll(dir)
	}
	return nil, errors.New("the upload directory was removed as it was made")
}

// sweepUploadDirs removes the store directories in tmp that no running
// server holds (lockUploadDir), so that a server that restarts in the same
// TMPDIR does not keep the uploads of the one before it. A directory it
// cannot open, another user's, is left.
func sweepUploadDirs(tmp string, warn io.Writer) {
	dirs, _ := filepath.Glob(filepath.Join(tmp, uploadDirPrefix+"*"))
	for _, dir := range dirs {
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			continue
		}
		lock, err := lockUploadDir(dir)
		if err != nil || !uploadDirLocks {
			if lock != nil {
				_ = lock.Close()
			}
			continue
		}
		if err := os.RemoveAll(dir); err != nil && warn != nil {
			fmt.Fprintf(warn, "capacity-planner: removing a stopped server's uploads: %v\n", err)
		}
		_ = lock.Close()
	}
}

// Close removes the store's directory and every upload in it.
func (u *uploadStore) Close() error {
	if u == nil {
		return nil
	}
	err := os.RemoveAll(u.dir)
	if u.lock != nil {
		_ = u.lock.Close()
	}
	return err
}

// path is the file of the upload named name.
func (u *uploadStore) path(name string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.paths[name]
	return p, ok
}

// load is the file of the upload named name, held until release is called:
// newer uploads pushing it out while it loads leave the file in place until
// then, so that the load reads it to the end.
func (u *uploadStore) load(name string) (path string, release func(), ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	path, ok = u.paths[name]
	if !ok {
		return "", func() {}, false
	}
	u.loading[path]++
	var once sync.Once
	return path, func() { once.Do(func() { u.release(path) }) }, true
}

// release ends one load of path, and removes the file when it was the last
// load of an upload pushed out meanwhile.
func (u *uploadStore) release(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.loading[path]--; u.loading[path] > 0 {
		return
	}
	delete(u.loading, path)
	if u.evicted[path] {
		delete(u.evicted, path)
		u.remove(path)
	}
}

// remove removes an upload's file, saying so on the server's log when it
// cannot.
func (u *uploadStore) remove(path string) {
	if err := os.Remove(path); err != nil && u.warn != nil {
		fmt.Fprintf(u.warn, "capacity-planner: removing an upload: %v\n", err)
	}
}

// names is the uploads kept, newest first.
func (u *uploadStore) names() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := slices.Clone(u.order)
	slices.Reverse(out)
	return out
}

// Upload writes body to a new file in the store, then reads it as a
// recording (check, every line, with the bounds above). A file that is not a
// recording, or is past a bound, is removed and refused; one whose bytes were
// kept before is removed and answered with the name they have.
func (u *uploadStore) Upload(ctx context.Context, body io.Reader) (whatifapi.UploadResponse, bool, error) {
	var none whatifapi.UploadResponse
	f, err := os.CreateTemp(u.dir, "upload-*")
	if err != nil {
		return none, false, u.storeError(err)
	}
	path, kept := f.Name(), false
	defer func() {
		if !kept {
			_ = os.Remove(path)
		}
	}()
	h := sha256.New()
	// A disk error is the server's, and names its paths; an error reading
	// body is the client's connection, or its bound.
	file := &writeErrors{w: f}
	n, err := io.Copy(io.MultiWriter(file, h), body)
	var magic [2]byte
	m, _ := f.ReadAt(magic[:], 0)
	cerr := f.Close()
	switch {
	case file.err != nil:
		return none, false, u.storeError(file.err)
	case err != nil:
		return none, false, fmt.Errorf("reading the upload: %w", err)
	case cerr != nil:
		return none, false, u.storeError(cerr)
	}
	if n == 0 {
		return none, false, whatifapi.InputErrorf("the upload is empty")
	}
	ext := ".jsonl"
	if m == len(magic) && bytes.Equal(magic[:], []byte{0x1f, 0x8b}) {
		ext = ".jsonl.gz"
	}
	name := uploadPrefix + hex.EncodeToString(h.Sum(nil)) + ext
	out := whatifapi.UploadResponse{Name: name, Bytes: int(n)}
	if _, ok := u.path(name); ok {
		return out, false, nil
	}
	if err := u.check(ctx, name, path); err != nil {
		return none, false, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.paths[name]; ok {
		return out, false, nil
	}
	kept = true
	u.paths[name] = path
	u.order = append(u.order, name)
	for len(u.order) > maxKeptUploads {
		old := u.order[0]
		u.order = u.order[1:]
		// A file being loaded is removed when its loads end (release).
		if p := u.paths[old]; u.loading[p] > 0 {
			u.evicted[p] = true
		} else {
			u.remove(p)
		}
		delete(u.paths, old)
	}
	return out, true, nil
}

// storeError is errNoUploadStore, the disk error err said on the server's
// log alone.
func (u *uploadStore) storeError(err error) error {
	if u.warn != nil {
		fmt.Fprintf(u.warn, "capacity-planner: storing an upload: %v\n", err)
	}
	return errNoUploadStore
}

// writeErrors is a writer that keeps the first error writing to w.
type writeErrors struct {
	w   io.Writer
	err error
}

func (w *writeErrors) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

// errUploadBound is a read stopped at one of an upload's bounds.
var errUploadBound = errors.New("past the upload's bound")

// check reads the upload at path as serve reads a recording, under the
// upload's bounds: past one is a TooLargeError, a file that is no recording
// an InputError naming the upload by name.
func (u *uploadStore) check(ctx context.Context, name, path string) error {
	rs, err := openRecording(path, true)
	if err != nil {
		return err
	}
	defer func() { _ = rs.Close() }()
	b := &boundedReader{ctx: ctx, left: u.maxBytes * maxUploadExpansion}
	open := rs.open
	rs.open = func() (io.ReadCloser, error) {
		rc, err := open()
		if err != nil {
			return nil, err
		}
		b.r = rc
		return struct {
			io.Reader
			io.Closer
		}{b, rc}, nil
	}
	err = rs.check(name, name)
	switch {
	case ctx.Err() != nil:
		// The client left, or the deadline passed: the rest is not read.
		return ctx.Err()
	case b.long:
		return whatifapi.TooLargeError{Err: fmt.Errorf("the upload was not kept: it has a line longer than %s, "+
			"more than a recording holds", whatifapi.Quantity(maxUploadLine))}
	case b.left < 0:
		return whatifapi.TooLargeError{Err: fmt.Errorf("the upload was not kept: it gunzips to more than %s "+
			"(%d times --upload-max-size), more than this server replays", whatifapi.Quantity(u.maxBytes*maxUploadExpansion), maxUploadExpansion)}
	case err != nil:
		return whatifapi.InputError{Err: fmt.Errorf("the upload was not kept: %w", err)}
	}
	return ctx.Err()
}

// boundedReader reads r until it has read left bytes, or a line longer than
// maxUploadLine: past either it stops with a readError, and left is below 0
// or long is set. It stops too once ctx is done, between two reads.
type boundedReader struct {
	ctx  context.Context
	r    io.Reader
	left int64
	line int64
	long bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, readError{err}
	}
	if b.left < 0 || b.long {
		return 0, readError{errUploadBound}
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	// The line read on into this chunk ends at its first newline; the
	// lines whole within it are shorter than the chunk, which is far
	// shorter than the bound.
	chunk := p[:n]
	if first := bytes.IndexByte(chunk, '\n'); first >= 0 {
		b.long = b.line+int64(first) > maxUploadLine
		b.line = int64(n - bytes.LastIndexByte(chunk, '\n') - 1)
	} else {
		b.line += int64(n)
	}
	b.long = b.long || b.line > maxUploadLine
	if b.left < 0 || b.long {
		return n, readError{errUploadBound}
	}
	return n, err
}
