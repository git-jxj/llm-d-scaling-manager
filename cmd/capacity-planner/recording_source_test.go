package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// gzipOf is b gzipped.
func gzipOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	_, err := z.Write(b)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	return buf.Bytes()
}

// resolveRecording resolves the recording name in src's directory, for
// model-02, the model the test recordings are asked for.
func resolveRecording(src *servedSources, name string) (*whatifapi.Resolved, error) {
	return src.Resolve(context.Background(), whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: name}})
}

// A recording rewritten while it is resolved is refused, never answered
// from passes over two versions under the first pass's content id. Rewritten
// in place shorter, the state was the half recording's, with no error;
// rewritten with the same bytes but a new modification time, or replaced by
// a rename with the same size and time, it is the file's identity that
// moved. Through the API, nothing is cached under the stale id: the next
// request reads the file as it now is, the id a new server gives it. Each
// rewrite lands as the first pass (the check) opens, and as the second: one
// under the check reads short or mixed, and the error is the change, not the
// bytes it left (before, the check's own error, such as "not a learner
// recording", for a file overwritten with gzip bytes).
func TestResolveRefusesARecordingChangedWhileRead(t *testing.T) {
	gz, err := os.ReadFile(olsSweep)
	require.NoError(t, err)
	plain, err := gunzipped(gz)
	require.NoError(t, err)
	for name, rewrite := range map[string]func(t *testing.T, path string){
		"shorter in place": func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, plain[:len(plain)/2+17], 0o644))
		},
		"same bytes, new time": func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, plain, 0o644))
			later := time.Now().Add(time.Hour)
			require.NoError(t, os.Chtimes(path, later, later))
		},
		"replaced by a rename": func(t *testing.T, path string) {
			fi, err := os.Stat(path)
			require.NoError(t, err)
			next := path + ".next"
			require.NoError(t, os.WriteFile(next, plain, 0o644))
			require.NoError(t, os.Chtimes(next, fi.ModTime(), fi.ModTime()))
			require.NoError(t, os.Rename(next, path))
		},
		"overwritten with gzip": func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, gz[:len(gz)/2], 0o644))
		},
	} {
		for _, at := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s at pass %d", name, at), func(t *testing.T) {
				// Overwritten with gzip, the replay fails on the pass that
				// meets it: the change is named then, not past the last pass.
				changedAt(t, plain, rewrite, at, name != "overwritten with gzip")
			})
		}
	}
}

// changedAt is TestResolveRefusesARecordingChangedWhileRead for one rewrite,
// made as pass at opens. readable is a rewrite every later pass reads to its
// end: the change is then met only after the last.
func changedAt(t *testing.T, plain []byte, rewrite func(t *testing.T, path string), at int, readable bool) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rec.jsonl")
	require.NoError(t, os.WriteFile(path, plain, 0o644))
	passes := 0
	src := &servedSources{recordings: dir, warn: io.Discard}
	src.onPass = func(pass int) {
		passes = pass
		if pass == at {
			rewrite(t, path)
		}
	}
	_, err := resolveRecording(src, "rec.jsonl")
	require.Error(t, err, "a recording changed at pass %d resolved", at)
	assert.Equal(t, "rec.jsonl: the file changed while it was read: read it again once it is written", err.Error())
	assert.True(t, whatifapi.IsInputError(err), "%T", err)
	if at > 1 && readable {
		assert.Greater(t, passes, at, "the change was met before the last pass")
	}

	// Through the API: a 400, then the file as it now is.
	require.NoError(t, os.WriteFile(path, plain, 0o644))
	src.onPass = func(pass int) {
		if pass == at {
			rewrite(t, path)
		}
	}
	h := whatifapi.New(whatifapi.Config{Resolver: src, WhatifSchema: "schema-v3"}).Handler()
	code, body := apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "rec.jsonl"}})
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	assert.Contains(t, string(body), "changed while it was read")
	assert.NotContains(t, string(body), dir, "the server's path in a response")
	src.onPass = nil
	code, body = apiPost(t, h, "/v1/inputs", whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "rec.jsonl"}})
	fresh := whatifapi.New(whatifapi.Config{Resolver: &servedSources{recordings: dir, warn: io.Discard},
		WhatifSchema: "schema-v3"}).Handler()
	freshCode, freshBody := apiPost(t, fresh, "/v1/inputs", whatifapi.InputsRequest{Model: "model-02",
		Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: "rec.jsonl"}})
	assert.Equal(t, freshCode, code)
	assert.Equal(t, string(freshBody), string(body), "the file as it now is, not a version cached mid-read")
}

// The order recordingSource.check names a recording's errors in: a gzip
// stream that does not read is that error, before any line's (a bad line
// then a cut stream reads as the cut), and a plain file's bad line is the
// line's. A torn tail, a missing final newline, CRLF lines and two gzip
// members read; gzip and plain of the same lines share a content id.
func TestCheckErrorOrder(t *testing.T) {
	G, err := os.ReadFile(olsSweep)
	require.NoError(t, err)
	R, err := gunzipped(G)
	require.NoError(t, err)
	lines := bytes.SplitAfter(R, []byte("\n"))
	withBad := bytes.Join([][]byte{bytes.Join(lines[:3], nil), []byte("{broken\n"), bytes.Join(lines[3:], nil)}, nil)
	badGz := gzipOf(t, withBad)
	half, rest := bytes.Join(lines[:len(lines)/2], nil), bytes.Join(lines[len(lines)/2:], nil)
	crc := bytes.Clone(G)
	crc[len(crc)-8] ^= 1
	badSeq := bytes.Join([][]byte{bytes.Join(lines[:3], nil), bytes.Join(lines[4:], nil)}, nil)
	const (
		sameAsGz = "same content as gz"
		read     = "read"
	)
	cases := []struct {
		name string
		data []byte
		want string // the error, or read / sameAsGz
	}{
		{"gz", G, read},
		{"plain", R, sameAsGz},
		{"plain-no-final-newline", bytes.TrimRight(R, "\n"), read},
		{"plain-crlf", bytes.ReplaceAll(R, []byte("\n"), []byte("\r\n")), read},
		{"plain-torn-tail", append(bytes.Clone(R), []byte(`{"Seq":99999,"Now":"20`)...), read},
		{"gz-truncated", G[:len(G)/2], "gz-truncated: unexpected EOF"},
		{"gz-bad-crc", crc, "gz-bad-crc: gzip: invalid checksum"},
		{"gz-trailing-garbage", append(bytes.Clone(G), []byte("garbage")...), "gz-trailing-garbage: unexpected EOF"},
		{"plain-bad-line", withBad, "plain-bad-line: not a learner recording: learnrec: line 4: invalid character 'b' looking for beginning of object key string"},
		{"gz-bad-line-then-truncated", badGz[:len(badGz)*8/10], "gz-bad-line-then-truncated: unexpected EOF"},
		{"empty", nil, "empty: not a learner recording: no start line"},
		{"gz-empty", gzipOf(t, nil), "gz-empty: not a learner recording: no start line"},
		{"plain-bad-seq", badSeq, "plain-bad-seq: not a learner recording: learnrec: line 4: sequence number 5, want 4"},
		{"gz-two-members", append(gzipOf(t, half), gzipOf(t, rest)...), sameAsGz},
		{"one-byte", []byte{0x1f}, "one-byte: not a learner recording: no start line"},
		{"gz-header-only", G[:10], "gz-header-only: unexpected EOF"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		require.NoError(t, os.WriteFile(filepath.Join(dir, c.name), c.data, 0o644))
	}
	src := &servedSources{recordings: dir, warn: io.Discard}
	gzRes, err := resolveRecording(src, "gz")
	require.NoError(t, err)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := resolveRecording(src, c.name)
			switch c.want {
			case read, sameAsGz:
				require.NoError(t, err)
				if c.want == sameAsGz {
					assert.Equal(t, gzRes.Content, res.Content)
				}
			default:
				require.Error(t, err)
				assert.Equal(t, c.want, err.Error())
				assert.True(t, whatifapi.IsInputError(err), "%T", err)
			}
		})
	}
}
