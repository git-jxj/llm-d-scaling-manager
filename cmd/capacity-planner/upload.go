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

// uploadStore keeps the recordings uploaded to serve (--allow-upload) in a
// directory of its own, readable by the server's user alone and removed when
// the server stops. An upload is kept under a name the server gives it, from
// its bytes; nothing the client sends names a file.
type uploadStore struct {
	dir string
	// maxBytes is --upload-max-size.
	maxBytes int64
	warn     io.Writer

	mu sync.Mutex
	// paths is each upload's file by its name, and order its names, oldest
	// first.
	paths map[string]string
	order []string
}

// newUploadStore makes the store's directory, in the system's temporary
// directory (TMPDIR), mode 0700.
func newUploadStore(maxBytes int64, warn io.Writer) (*uploadStore, error) {
	dir, err := os.MkdirTemp("", "capacity-planner-uploads-")
	if err != nil {
		return nil, err
	}
	return &uploadStore{dir: dir, maxBytes: maxBytes, warn: warn, paths: map[string]string{}}, nil
}

// Close removes the store's directory and every upload in it.
func (u *uploadStore) Close() error {
	if u == nil {
		return nil
	}
	return os.RemoveAll(u.dir)
}

// path is the file of the upload named name.
func (u *uploadStore) path(name string) (string, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p, ok := u.paths[name]
	return p, ok
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
		return none, false, err
	}
	path, kept := f.Name(), false
	defer func() {
		if !kept {
			_ = os.Remove(path)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), body)
	var magic [2]byte
	m, _ := f.ReadAt(magic[:], 0)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return none, false, fmt.Errorf("reading the upload: %w", err)
	}
	if n == 0 {
		return none, false, whatifapi.InputErrorf("the upload is empty")
	}
	ext := ".jsonl"
	if m == len(magic) && bytes.Equal(magic[:], []byte{0x1f, 0x8b}) {
		ext = ".jsonl.gz"
	}
	name := uploadPrefix + hex.EncodeToString(h.Sum(nil))[:16] + ext
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
		if err := os.Remove(u.paths[old]); err != nil && u.warn != nil {
			fmt.Fprintf(u.warn, "capacity-planner: removing the upload %s: %v\n", old, err)
		}
		delete(u.paths, old)
	}
	return out, true, nil
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
	b := &boundedReader{left: u.maxBytes * maxUploadExpansion}
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
// or long is set.
type boundedReader struct {
	r    io.Reader
	left int64
	line int64
	long bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
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
