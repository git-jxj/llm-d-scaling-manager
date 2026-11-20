package learnrec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// RawEvent is an Event as read back, its Data still undecoded.
type RawEvent struct {
	Seq  uint64
	Now  time.Time
	Kind string
	// Data is the event's data as written; DecodeData turns it into the
	// kind's own type.
	Data json.RawMessage
	// Line is the whole line as written, for a byte-for-byte comparison.
	Line string
}

// Recording is a recording as read back.
type Recording struct {
	Events []RawEvent
	// TornTail is a last line that was cut off mid-write (no newline, and it
	// does not parse): the process died, or the volume filled, while writing
	// it. It is dropped; empty when the recording ends cleanly.
	TornTail string
}

// Read parses a recording into its events. Every line must parse and the
// sequence numbers must run from 1 with no gap, except a last line cut off
// mid-write, which is reported in TornTail and dropped. Anything else is an
// error: a recording with a hole in it does not replay.
func Read(data []byte) (Recording, error) {
	var out Recording
	s := NewScanner(bytes.NewReader(data))
	for s.Scan() {
		out.Events = append(out.Events, s.Event())
	}
	if err := s.Err(); err != nil {
		return Recording{}, err
	}
	out.TornTail = s.TornTail()
	return out, nil
}

// Scanner reads a recording one event at a time, by Read's rules, so that a
// recording is read without being held: a day-long one is a gigabyte of
// lines.
type Scanner struct {
	r *bufio.Reader
	// buf is the line being read, reused from line to line.
	buf []byte
	ev  RawEvent
	err error
	// line is the lines read, from 1; events the events read; eof that the
	// last line was read.
	line   int
	events uint64
	eof    bool
	torn   string
}

// NewScanner is a scanner of the recording r reads.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewReaderSize(r, 64<<10)}
}

// Scan reads the next event, and reports whether there was one: false at
// the end of the recording, a last line cut off mid-write (TornTail), or an
// error (Err).
func (s *Scanner) Scan() bool {
	for s.err == nil && !s.eof {
		line, err := s.next()
		if err != nil {
			s.err = err
			return false
		}
		s.line++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev, err := readLine(line)
		if err != nil {
			if s.eof {
				// No newline after it: the write that would have ended it
				// never happened.
				s.torn = string(line)
				return false
			}
			s.err = fmt.Errorf("learnrec: line %d: %w", s.line, err)
			return false
		}
		if want := s.events + 1; ev.Seq != want {
			s.err = fmt.Errorf("learnrec: line %d: sequence number %d, want %d", s.line, ev.Seq, want)
			return false
		}
		s.events++
		s.ev = ev
		return true
	}
	return false
}

// next is the next line, without its newline; at the end of the input it is
// what follows the last newline, and eof is set.
func (s *Scanner) next() ([]byte, error) {
	s.buf = s.buf[:0]
	for {
		chunk, err := s.r.ReadSlice('\n')
		s.buf = append(s.buf, chunk...)
		switch {
		case err == nil:
			return s.buf[:len(s.buf)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			s.eof = true
			return s.buf, nil
		default:
			return nil, err
		}
	}
}

// Event is the event Scan read.
func (s *Scanner) Event() RawEvent { return s.ev }

// Err is the error that stopped the scan, nil at the end of a recording.
func (s *Scanner) Err() error { return s.err }

// TornTail is the last line, cut off mid-write, that the scan dropped; empty
// when the recording ended cleanly or the scan has not reached its end.
func (s *Scanner) TornTail() string { return s.torn }

// envelope is a line's fields, as encoding/json reads them.
type envelope struct {
	Seq  uint64
	Now  string
	Kind string
	Data json.RawMessage
}

func readLine(line []byte) (RawEvent, error) {
	env, ok := canonicalEnvelope(line)
	if !ok {
		if err := json.Unmarshal(line, &env); err != nil {
			return RawEvent{}, err
		}
	}
	now, err := time.Parse(time.RFC3339Nano, env.Now)
	if err != nil {
		return RawEvent{}, err
	}
	return RawEvent{Seq: env.Seq, Now: now.UTC(), Kind: env.Kind, Data: env.Data, Line: string(line)}, nil
}

// canonicalEnvelope is a line as Marshal writes it, read without
// encoding/json's decoder: valid JSON, an object of exactly the four
// fields, by their names, Seq a plain integer, Now and Kind plain strings and
// Data no null. It is what encoding/json reads such a line as; false for any
// other line, which encoding/json reads.
func canonicalEnvelope(line []byte) (envelope, bool) {
	var env envelope
	if !json.Valid(line) {
		return env, false
	}
	ms, ok := splitObject(bytes.TrimSpace(line))
	if !ok || len(ms) != 4 {
		return env, false
	}
	var have [4]bool
	for _, m := range ms {
		switch m.key {
		case "Seq":
			n, err := strconv.ParseUint(string(m.raw), 10, 64)
			if err != nil {
				return env, false
			}
			env.Seq, have[0] = n, true
		case "Now", "Kind":
			if !plainString(m.raw) {
				return env, false
			}
			if m.key == "Now" {
				env.Now, have[1] = string(m.raw[1:len(m.raw)-1]), true
			} else {
				env.Kind, have[2] = string(m.raw[1:len(m.raw)-1]), true
			}
		case "Data":
			if len(m.raw) == 0 || m.raw[0] == 'n' {
				return env, false
			}
			env.Data, have[3] = bytes.Clone(m.raw), true
		default:
			return env, false
		}
	}
	return env, have == [4]bool{true, true, true, true}
}

// DecodeData decodes the event's data into the value v points to.
func (e RawEvent) DecodeData(v any) error {
	return Unmarshal(e.Data, v)
}
