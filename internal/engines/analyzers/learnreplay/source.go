package learnreplay

import (
	"bytes"
	"io"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// Source opens a recording, gunzipped, from its first line. A reader that
// passes over a recording more than once (StateFrom, SeriesFrom) opens it
// once a pass, so a day-long recording, a gigabyte of lines, is never held
// whole.
type Source func() (io.ReadCloser, error)

// BytesSource is the source of a recording held in memory.
func BytesSource(recording []byte) Source {
	return func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(recording)), nil }
}

// scan calls fn with each event of the recording src opens, in order, until
// fn returns false or an error. A line that does not read is learnrec.Read's
// error.
func scan(src Source, fn func(learnrec.RawEvent) (bool, error)) error {
	rc, err := src()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	s := learnrec.NewScanner(rc)
	for s.Scan() {
		more, err := fn(s.Event())
		if err != nil || !more {
			return err
		}
	}
	return s.Err()
}

// check reads every line of the recording src opens, as learnrec.Read does,
// holding none: a replay of a recording that does not read fails before any
// event is applied, with Read's error.
func check(src Source) error {
	return scan(src, func(learnrec.RawEvent) (bool, error) { return true, nil })
}
