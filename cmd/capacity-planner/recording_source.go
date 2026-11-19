package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"io"
	"os"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/learnreplay"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// recordingSource is a recording read in passes, never held whole: a file,
// opened once so that every pass reads the same bytes and gunzipped as it is
// read when it is gzip, or a recording made in memory (--prometheus). A
// day-long recording is a gigabyte of lines; each pass holds one.
type recordingSource struct {
	open learnreplay.Source
	// file is the file under open; nil for a recording in memory.
	file *os.File
	// sum is the SHA-256 of the gunzipped recording, set by check.
	sum [sha256.Size]byte
}

// memoryRecording is the source of a recording held in memory.
func memoryRecording(data []byte) *recordingSource {
	return &recordingSource{open: learnreplay.BytesSource(data)}
}

// openRecording is the source of the recording file at path.
func openRecording(path string) (*recordingSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	size := fi.Size()
	var magic [2]byte
	n, _ := f.ReadAt(magic[:], 0)
	gz := n == len(magic) && bytes.Equal(magic[:], []byte{0x1f, 0x8b})
	open := func() (io.ReadCloser, error) {
		r := io.NewSectionReader(f, 0, size)
		if !gz {
			return io.NopCloser(readErrors{r}), nil
		}
		z, err := gzip.NewReader(bufio.NewReaderSize(r, 1<<20))
		if err != nil {
			return nil, readError{err}
		}
		return struct {
			io.Reader
			io.Closer
		}{readErrors{z}, z}, nil
	}
	return &recordingSource{open: open, file: f}, nil
}

// Close closes the file under the source, if any.
func (s *recordingSource) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

// readError is an error reading the recording's bytes (a file, or its gzip),
// not a line of it: the file's error, as reading it whole gave it.
type readError struct{ err error }

func (e readError) Error() string { return e.err.Error() }
func (e readError) Unwrap() error { return e.err }

// readErrors is a reader whose errors, but io.EOF, are readErrors.
type readErrors struct{ r io.Reader }

func (r readErrors) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = readError{err}
	}
	return n, err
}

// check reads every line of the recording, as learnrec.Read would, and sums
// its bytes: a file that does not read is a usage error naming path, a
// recording whose lines do not read or that has no start line one naming
// name, as answers read them.
func (s *recordingSource) check(name, path string) error {
	rc, err := s.open()
	if err != nil {
		return usagef("%s: %v", path, err)
	}
	defer func() { _ = rc.Close() }()
	h := sha256.New()
	tee := io.TeeReader(rc, h)
	sc := learnrec.NewScanner(tee)
	events, start := 0, false
	for sc.Scan() {
		if events == 0 {
			start = sc.Event().Kind == learnrec.KindStart
		}
		events++
	}
	// The rest is read whatever stopped the scan: what follows a torn tail
	// is summed too, and a file that does not read is that error, before
	// any of its lines', as reading it whole gave it.
	_, rest := io.Copy(io.Discard, tee)
	var re readError
	switch err := sc.Err(); {
	case errors.As(err, &re):
		return usagef("%s: %v", path, re.err)
	case errors.As(rest, &re):
		return usagef("%s: %v", path, re.err)
	case err != nil:
		return usagef("%s: not a learner recording: %v", name, err)
	case rest != nil:
		return usagef("%s: %v", path, rest)
	case !start:
		return usagef("%s: not a learner recording: no start line", name)
	}
	copy(s.sum[:], h.Sum(nil))
	return nil
}
