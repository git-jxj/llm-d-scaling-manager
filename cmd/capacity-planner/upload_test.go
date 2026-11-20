package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// uploadServer is serve's handler as the flags build it, on a recordings
// directory holding the ols-sweep recording, and the flags.
func uploadServer(t *testing.T, f serveFlags) (http.Handler, *serveFlags) {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(olsSweep)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ols-sweep.jsonl.gz"), b, 0o600))
	f.recordings, f.listen, f.auth = dir, "127.0.0.1:0", whatifapi.AuthNone
	h, err := f.handler(newFlags("serve", io.Discard), io.Discard)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.uploads.Close() })
	return h, &f
}

// send sends one request to h on a loopback name, as a browser through a
// port-forward does.
func send(t *testing.T, h http.Handler, method, path, typ string, body []byte) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewReader(body))
	if typ != "" {
		req.Header.Set("Content-Type", typ)
	}
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// uploaded uploads body and decodes the answer, which must be want.
func uploaded(t *testing.T, h http.Handler, body []byte, want int) whatifapi.UploadResponse {
	t.Helper()
	code, out := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/octet-stream", body)
	require.Equal(t, want, code, "%s", out)
	var up whatifapi.UploadResponse
	require.NoError(t, json.Unmarshal(out, &up))
	return up
}

// uploadInputs resolves a recording for ols-sweep's model, and answers the
// status and the inputs.
func uploadInputs(t *testing.T, h http.Handler, name string) (int, whatifapi.InputsResponse, []byte) {
	t.Helper()
	req, err := json.Marshal(whatifapi.InputsRequest{Model: "model-02", Source: &whatifapi.SourceRequest{Kind: whatifapi.SourceRecording, Name: name}})
	require.NoError(t, err)
	code, body := send(t, h, http.MethodPost, "/v1/inputs", "application/json", req)
	var in whatifapi.InputsResponse
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal(body, &in))
	}
	return code, in, body
}

// sourcesOf is GET /v1/sources.
func sourcesOf(t *testing.T, h http.Handler) whatifapi.SourcesResponse {
	t.Helper()
	code, body := send(t, h, http.MethodGet, "/v1/sources", "", nil)
	require.Equal(t, http.StatusOK, code, "%s", body)
	var s whatifapi.SourcesResponse
	require.NoError(t, json.Unmarshal(body, &s))
	return s
}

// uploadFiles is the files in the store's directory.
func uploadFiles(t *testing.T, f *serveFlags) []string {
	t.Helper()
	entries, err := os.ReadDir(f.uploads.dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func olsSweepBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(olsSweep)
	require.NoError(t, err)
	return b
}

// olsSweepLines is the ols-sweep recording's lines.
func olsSweepLines(t *testing.T) []byte {
	t.Helper()
	z, err := gzip.NewReader(bytes.NewReader(olsSweepBytes(t)))
	require.NoError(t, err)
	b, err := io.ReadAll(z)
	require.NoError(t, err)
	return b
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	z, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	require.NoError(t, err)
	_, err = z.Write(b)
	require.NoError(t, err)
	require.NoError(t, z.Close())
	return out.Bytes()
}

// Without --allow-upload, serve takes no upload: a good recording is 403,
// nothing is stored, and the page is not offered the control.
func TestServeUploadOffByDefault(t *testing.T) {
	h, f := uploadServer(t, serveFlags{})
	code, body := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/octet-stream", olsSweepBytes(t))
	assert.Equal(t, http.StatusForbidden, code, "%s", body)
	assert.Contains(t, string(body), "--allow-upload")
	assert.Nil(t, f.uploads)
	assert.Nil(t, sourcesOf(t, h).Upload)
	// The Host check holds for an upload as for every request.
	h, _ = uploadServer(t, serveFlags{allowUpload: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://rebound.example:8080"+whatifapi.UploadPath, bytes.NewReader(olsSweepBytes(t)))
	req.Header.Set("Content-Type", "application/octet-stream")
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// --upload-max-size needs --allow-upload and a whole number of bytes;
// --allow-upload alone is a source.
func TestServeUploadFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"--allow-upload"}, true},
		{[]string{"--allow-upload", "--upload-max-size", "1Gi"}, true},
		{[]string{"--upload-max-size", "1Gi", "--recordings", t.TempDir()}, false},
		{[]string{"--allow-upload", "--upload-max-size", "0"}, false},
		{[]string{"--allow-upload", "--upload-max-size", "lots"}, false},
	} {
		fs := newFlags("serve", io.Discard)
		f := serveFlags{listen: "127.0.0.1:0", auth: whatifapi.AuthNone}
		fs.StringVar(&f.recordings, "recordings", "", "")
		fs.BoolVar(&f.allowUpload, "allow-upload", false, "")
		fs.StringVar(&f.uploadMax, "upload-max-size", "", "")
		require.NoError(t, fs.Parse(c.args))
		_, err := f.handler(fs, io.Discard)
		if f.uploads != nil {
			dir := f.uploads.dir
			require.NoError(t, f.uploads.Close())
			assert.NoDirExists(t, dir, "Close removes the uploads")
		}
		if c.ok {
			assert.NoError(t, err, "%v", c.args)
		} else {
			assert.ErrorAs(t, err, new(usageError), "%v", c.args)
		}
	}
}

// An upload past --upload-max-size is 413, and nothing of it is kept.
func TestServeUploadOverTheLimit(t *testing.T) {
	h, f := uploadServer(t, serveFlags{allowUpload: true, uploadMax: "1Ki"})
	code, body := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/gzip", olsSweepBytes(t))
	assert.Equal(t, http.StatusRequestEntityTooLarge, code, "%s", body)
	assert.Contains(t, string(body), "1Ki (--upload-max-size)")
	assert.Empty(t, uploadFiles(t, f))
	assert.Equal(t, []string{"ols-sweep.jsonl.gz"}, sourcesOf(t, h).Recordings)
}

// A file that is not a learner recording is 400, and nothing of it is kept:
// text, a gzip cut short, JSON lines with no start line, and nothing.
func TestServeUploadRefusesANonRecording(t *testing.T) {
	h, f := uploadServer(t, serveFlags{allowUpload: true})
	rec := olsSweepBytes(t)
	for name, body := range map[string][]byte{
		"text":          []byte("hello, planner\n"),
		"cut gzip":      rec[:len(rec)/2],
		"no start line": []byte(`{"kind":"cycle"}` + "\n"),
		"empty":         {},
	} {
		code, out := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/octet-stream", body)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, out)
		assert.NotContains(t, string(out), f.uploads.dir, "%s: an error names no server path", name)
	}
	assert.Empty(t, uploadFiles(t, f))
	assert.Equal(t, []string{"ols-sweep.jsonl.gz"}, sourcesOf(t, h).Recordings)
}

// A recording uploaded is listed, and answers as the same file in
// --recordings does: the same inputs, under the same inputsId. The same
// bytes again are answered with the name they have, not kept twice.
func TestServeUploadAnswersAsTheRecording(t *testing.T) {
	h, f := uploadServer(t, serveFlags{allowUpload: true})
	up := uploaded(t, h, olsSweepBytes(t), http.StatusCreated)
	sum := sha256.Sum256(olsSweepBytes(t))
	assert.Equal(t, uploadPrefix+hex.EncodeToString(sum[:])+".jsonl.gz", up.Name, "named by the whole SHA-256 of the bytes sent")
	assert.Equal(t, len(olsSweepBytes(t)), up.Bytes)
	assert.Equal(t, up, uploaded(t, h, olsSweepBytes(t), http.StatusOK))
	assert.Len(t, uploadFiles(t, f), 1)

	s := sourcesOf(t, h)
	assert.Equal(t, []string{up.Name, "ols-sweep.jsonl.gz"}, s.Recordings)
	require.NotNil(t, s.Upload)
	assert.Equal(t, whatifapi.DefaultUploadBytes, s.Upload.MaxBytes)

	code, fromFile, body := uploadInputs(t, h, "ols-sweep.jsonl.gz")
	require.Equal(t, http.StatusOK, code, "%s", body)
	code, fromUpload, body := uploadInputs(t, h, up.Name)
	require.Equal(t, http.StatusOK, code, "%s", body)
	assert.Equal(t, up.Name, fromUpload.Source.Name)
	assert.NotEmpty(t, fromUpload.Terms)
	fromUpload.Source.Name = fromFile.Source.Name
	assert.Equal(t, fromFile, fromUpload)
}

// An upload is refused, 413 with the bound named, where its replay would
// hold too much: it gunzips past 16 times --upload-max-size, a line is
// longer than a recording's, or loaded it weighs more than the inputs cache
// keeps. The same file in --recordings still loads.
func TestServeUploadBoundsTheReplay(t *testing.T) {
	t.Run("gunzipped", func(t *testing.T) {
		// 116 KB that gunzip to 3.6 MB, past 16 times 128Ki.
		h, f := uploadServer(t, serveFlags{allowUpload: true, uploadMax: "128Ki"})
		code, body := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/gzip", olsSweepBytes(t))
		assert.Equal(t, http.StatusRequestEntityTooLarge, code, "%s", body)
		assert.Contains(t, string(body), "gunzips to more than 2Mi")
		assert.Empty(t, uploadFiles(t, f))
	})
	t.Run("line", func(t *testing.T) {
		h, f := uploadServer(t, serveFlags{allowUpload: true, uploadMax: "8Mi"})
		long := append(olsSweepLines(t), bytes.Repeat([]byte("a"), maxUploadLine+1)...)
		code, body := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/gzip", gzipBytes(t, long))
		assert.Equal(t, http.StatusRequestEntityTooLarge, code, "%s", body)
		assert.Contains(t, string(body), "a line longer than 64Mi")
		assert.Empty(t, uploadFiles(t, f))
	})
	t.Run("weight", func(t *testing.T) {
		h, _ := uploadServer(t, serveFlags{allowUpload: true, inputsCache: "1Ki"})
		up := uploaded(t, h, olsSweepBytes(t), http.StatusCreated)
		code, _, body := uploadInputs(t, h, up.Name)
		assert.Equal(t, http.StatusRequestEntityTooLarge, code, "%s", body)
		assert.Contains(t, string(body), "--inputs-cache-size 1Ki")
		code, _, body = uploadInputs(t, h, "ols-sweep.jsonl.gz")
		assert.Equal(t, http.StatusOK, code, "%s", body)
	})
}

// The server keeps the last maxKeptUploads uploads: an older one is removed,
// no longer listed, and refused by name.
func TestServeUploadKeepsTheLast(t *testing.T) {
	h, f := uploadServer(t, serveFlags{allowUpload: true})
	lines := olsSweepLines(t)
	names := make([]string, 0, maxKeptUploads+1)
	for i := range maxKeptUploads + 1 {
		// Each a different file: the recording and i blank lines.
		up := uploaded(t, h, append(bytes.Clone(lines), bytes.Repeat([]byte("\n"), i)...), http.StatusCreated)
		assert.True(t, strings.HasSuffix(up.Name, ".jsonl"), up.Name)
		names = append(names, up.Name)
	}
	assert.Len(t, uploadFiles(t, f), maxKeptUploads)
	listed := sourcesOf(t, h).Recordings
	assert.NotContains(t, listed, names[0])
	assert.Equal(t, names[maxKeptUploads], listed[0], "the newest first")
	code, _, body := uploadInputs(t, h, names[0])
	assert.Equal(t, http.StatusBadRequest, code, "%s", body)
	assert.Contains(t, string(body), "no such upload")
	code, _, body = uploadInputs(t, h, names[maxKeptUploads])
	assert.Equal(t, http.StatusOK, code, "%s", body)
}

// newUploads is a store of the default bound whose log is warn, removed
// with the test.
func newUploads(t *testing.T, warn io.Writer) *uploadStore {
	t.Helper()
	up, err := newUploadStore(whatifapi.DefaultUploadBytes, warn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = up.Close() })
	return up
}

// An upload pushed out by newer ones while it loads is kept until its load
// ends: the load succeeds, and only then is its file removed. Before, the
// file was removed under the load, which failed with "the file changed
// while it was read".
func TestServeUploadKeptWhileItLoads(t *testing.T) {
	up := newUploads(t, io.Discard)
	lines := olsSweepLines(t)
	first, created, err := up.Upload(t.Context(), bytes.NewReader(lines), "")
	require.NoError(t, err)
	require.True(t, created)
	firstPath, ok := up.path(first.Name)
	require.True(t, ok)
	src := &servedSources{uploads: up, warn: io.Discard}
	src.onPass = func(pass int) {
		if pass != 1 {
			return
		}
		for i := range maxKeptUploads {
			_, created, err := up.Upload(t.Context(), bytes.NewReader(append(bytes.Clone(lines), bytes.Repeat([]byte("\n"), i+1)...)), "")
			require.NoError(t, err)
			require.True(t, created)
		}
		_, kept := up.path(first.Name)
		require.False(t, kept, "pushed out of the store while it loads")
	}
	res, err := resolveRecording(src, first.Name)
	require.NoError(t, err)
	assert.Equal(t, first.Name, res.Inputs.Source.Name)
	assert.NoFileExists(t, firstPath, "removed once its load ended")
	assert.Len(t, up.names(), maxKeptUploads)
	entries, err := os.ReadDir(up.dir)
	require.NoError(t, err)
	assert.Len(t, entries, maxKeptUploads)
}

// A disk error storing an upload is a 500 that names no server path; the
// error itself goes to the server's log.
func TestServeUploadDiskErrorNamesNoPath(t *testing.T) {
	h, f := uploadServer(t, serveFlags{allowUpload: true})
	var log bytes.Buffer
	f.uploads.warn = &log
	require.NoError(t, os.RemoveAll(f.uploads.dir))
	code, body := send(t, h, http.MethodPost, whatifapi.UploadPath, "application/gzip", olsSweepBytes(t))
	assert.Equal(t, http.StatusInternalServerError, code, "%s", body)
	assert.Contains(t, string(body), "the server could not store the upload")
	assert.NotContains(t, string(body), f.uploads.dir)
	assert.NotContains(t, string(body), os.TempDir())
	assert.Contains(t, log.String(), f.uploads.dir, "the server's log has the detail")
}

// A client that leaves while its upload is checked stops the check: the
// rest is not read, and nothing is kept. The file here would read as no
// recording; read to its end, that was the answer.
func TestServeUploadCheckStopsWhenTheClientLeaves(t *testing.T) {
	up := newUploads(t, io.Discard)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := up.Upload(ctx, strings.NewReader("hello, planner\n"), "")
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, whatifapi.IsInputError(err), "%v", err)
	entries, err := os.ReadDir(up.dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// removedAtEOF is data whose reader removes dir once it is read, as Close
// does to the store's directory when the server stops mid-upload.
type removedAtEOF struct {
	data []byte
	dir  string
}

func (r *removedAtEOF) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		_ = os.RemoveAll(r.dir)
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// An error checking an upload names the upload, never the server's file:
// here the store's directory goes between the write and the check.
func TestServeUploadCheckErrorNamesNoPath(t *testing.T) {
	var log bytes.Buffer
	up := newUploads(t, &log)
	_, _, err := up.Upload(t.Context(), &removedAtEOF{data: olsSweepLines(t), dir: up.dir}, "")
	require.Error(t, err)
	t.Logf("err: %v", err)
	assert.NotContains(t, err.Error(), up.dir)
	assert.NotContains(t, err.Error(), os.TempDir())
}

// The same bytes uploaded again move that upload to the newest place: it is
// not the next pushed out.
func TestServeUploadAgainIsNewest(t *testing.T) {
	up := newUploads(t, io.Discard)
	lines := olsSweepLines(t)
	names := make([]string, 0, maxKeptUploads+1)
	for i := range maxKeptUploads + 1 {
		if i == maxKeptUploads {
			// The oldest again, before one more arrives.
			again, created, err := up.Upload(t.Context(), bytes.NewReader(lines), "")
			require.NoError(t, err)
			require.False(t, created)
			require.Equal(t, names[0], again.Name)
		}
		out, created, err := up.Upload(t.Context(), bytes.NewReader(append(bytes.Clone(lines), bytes.Repeat([]byte("\n"), i)...)), "")
		require.NoError(t, err)
		require.True(t, created)
		names = append(names, out.Name)
	}
	kept := up.names()
	assert.Contains(t, kept, names[0], "uploaded again, it was kept")
	assert.NotContains(t, kept, names[1], "the oldest not uploaded again went")
	assert.Equal(t, []string{names[maxKeptUploads], names[0]}, kept[:2], "newest first")
}

// An upload is listed with the file name it was sent under, its size and
// when, beside its name, which stays the hash: the store keeps the file name
// in memory and names no file by it. The same bytes sent again under another
// name are relabelled, and under none keep their label. Before, uploads were
// listed by hash alone.
func TestServeUploadListsItsFileName(t *testing.T) {
	h, f := uploadServer(t, serveFlags{allowUpload: true})
	at := time.Date(2026, 11, 20, 14, 5, 0, 0, time.FixedZone("IST", 2*3600))
	f.uploads.now = func() time.Time { return at }
	post := func(target string, body []byte) whatifapi.UploadResponse {
		t.Helper()
		code, out := send(t, h, http.MethodPost, target, "application/gzip", body)
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, code, "%s", out)
		var up whatifapi.UploadResponse
		require.NoError(t, json.Unmarshal(out, &up))
		return up
	}
	sweep := olsSweepBytes(t)
	first := post(whatifapi.UploadPath+"?filename="+url.QueryEscape(`C:\runs\..\ols <sweep>.jsonl.gz`), sweep)
	assert.True(t, strings.HasPrefix(first.Name, uploadPrefix), first.Name)
	assert.Equal(t, "ols _sweep_.jsonl.gz", first.FileName)
	assert.Equal(t, "2026-11-20T12:05:00Z", first.UploadedAt)
	assert.Equal(t, len(sweep), first.Bytes)
	for _, file := range uploadFiles(t, f) {
		assert.NotContains(t, file, "sweep", "no file is named by the client's name")
	}

	second := post(whatifapi.UploadPath, olsSweepLines(t))
	s := sourcesOf(t, h)
	assert.Equal(t, []string{second.Name, first.Name}, s.Recordings[:2])
	assert.Equal(t, []whatifapi.UploadedRecording{
		{Name: second.Name, Bytes: second.Bytes, UploadedAt: "2026-11-20T12:05:00Z"},
		{Name: first.Name, FileName: "ols _sweep_.jsonl.gz", Bytes: len(sweep), UploadedAt: "2026-11-20T12:05:00Z"},
	}, s.Uploads)

	at = at.Add(time.Hour)
	again := post(whatifapi.UploadPath+"?filename=renamed.jsonl.gz", sweep)
	assert.Equal(t, first.Name, again.Name)
	assert.Equal(t, "renamed.jsonl.gz", again.FileName)
	assert.Equal(t, "2026-11-20T13:05:00Z", again.UploadedAt)
	post(whatifapi.UploadPath, sweep)
	s = sourcesOf(t, h)
	require.Len(t, s.Uploads, 2)
	assert.Equal(t, whatifapi.UploadedRecording{Name: first.Name, FileName: "renamed.jsonl.gz", Bytes: len(sweep), UploadedAt: "2026-11-20T13:05:00Z"},
		s.Uploads[0], "sent again, newest first, its label kept when the upload gives none")
}
