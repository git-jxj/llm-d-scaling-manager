package whatifapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authzv1 "k8s.io/api/authorization/v1"
)

// stubUploader reads the whole body, as the server's store does, and keeps
// nothing: created the first time it sees a body, not after.
type stubUploader struct {
	calls int
	seen  map[string]bool
	err   error
	// fileName is the one the last upload was given.
	fileName string
}

func (u *stubUploader) Upload(_ context.Context, body io.Reader, fileName string) (UploadResponse, bool, error) {
	u.calls++
	u.fileName = fileName
	b, err := io.ReadAll(body)
	if err != nil {
		return UploadResponse{}, false, fmt.Errorf("reading the upload: %w", err)
	}
	if u.err != nil {
		return UploadResponse{}, false, u.err
	}
	if u.seen == nil {
		u.seen = map[string]bool{}
	}
	created := !u.seen[string(b)]
	u.seen[string(b)] = true
	return UploadResponse{Name: "upload:x.jsonl", Bytes: len(b)}, created, nil
}

// upload posts body to h as typ; a negative length hides it, as a chunked
// request does.
func upload(h http.Handler, body, typ string, length int64) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, UploadPath, io.MultiReader(strings.NewReader(body)))
	req.ContentLength = length
	req.Header.Set("Content-Type", typ)
	h.ServeHTTP(rec, req)
	return rec
}

// A server with no Uploader refuses an upload, whatever it holds, and its
// sources say it takes none.
func TestUploadOffByDefault(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	rec := upload(h, "{}\n", "application/octet-stream", 3)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "--allow-upload")
	src := httptest.NewRecorder()
	h.ServeHTTP(src, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	require.Equal(t, http.StatusOK, src.Code)
	assert.NotContains(t, src.Body.String(), `"upload"`)
}

// An upload is refused, before the Uploader reads it, as a type a page on
// another site may post without a preflight, and past the bound when its
// length is declared; past the bound when it is not, as it is read.
func TestUploadBounds(t *testing.T) {
	u := &stubUploader{}
	h := New(Config{Resolver: &stubResolver{}, Uploader: u, UploadMaxBytes: 8}).Handler()
	for _, typ := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/json", ""} {
		rec := upload(h, "{}\n", typ, 3)
		assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code, "%q: %s", typ, rec.Body.String())
	}
	rec := upload(h, "0123456789", "application/octet-stream", 10)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "8 (--upload-max-size)")
	assert.Zero(t, u.calls, "a body declared too long is not read")
	rec = upload(h, "0123456789", "application/octet-stream", -1)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Equal(t, 1, u.calls)

	// The first upload of a body is created, the second answered with it.
	for _, c := range []struct {
		typ  string
		want int
	}{{"application/gzip", http.StatusCreated}, {"application/octet-stream", http.StatusOK}} {
		rec = upload(h, "01234567", c.typ, -1)
		require.Equal(t, c.want, rec.Code, rec.Body.String())
		var out UploadResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		assert.Equal(t, UploadResponse{SchemaVersion: SchemaVersion, Name: "upload:x.jsonl", Bytes: 8}, out)
	}

	u.err = TooLargeError{Err: errors.New("it gunzips to more")}
	rec = upload(h, "abc", "application/gzip", 3)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	u.err = InputErrorf("not a learner recording")
	rec = upload(h, "abd", "application/gzip", 3)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	src := httptest.NewRecorder()
	h.ServeHTTP(src, httptest.NewRequest(http.MethodGet, "/v1/sources", nil))
	assert.Contains(t, src.Body.String(), `"maxBytes": 8`)
}

// With Kubernetes authentication an upload is authorized as post on its
// path: a user granted get on it, the read-only API's verb, may not upload.
func TestUploadIsAuthorizedAsPost(t *testing.T) {
	c := newCluster("/whatif"+UploadPath, "")
	h := NewAuthenticator(c, nil, 0, nil).Wrap(New(Config{Resolver: &stubResolver{}, Uploader: &stubUploader{}}).Handler())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, UploadPath, strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer good")
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "may not post /whatif/v1/recordings")
	require.NotNil(t, c.lastSAR.NonResourceAttributes)
	assert.Equal(t, authzv1.NonResourceAttributes{Path: "/whatif/v1/recordings", Verb: "post"}, *c.lastSAR.NonResourceAttributes)
}

// An upload's file name is a label: the last element of a path, characters
// a name needs kept, every other one replaced, bounded. Before, the name the
// client sent was never read, and uploads were listed by hash alone.
func TestUploadFileName(t *testing.T) {
	for in, want := range map[string]string{
		"sample-staircase-qwen3-0.6b.jsonl.gz": "sample-staircase-qwen3-0.6b.jsonl.gz",
		"C:/Users/me/runs/run (2).jsonl.gz":    "run (2).jsonl.gz",
		`C:\Users\me\runs\run.jsonl.gz`:        "run.jsonl.gz",
		"../../etc/passwd":                     "passwd",
		"..":                                   "..",
		"<script>alert(1)</script>.jsonl":      "script_.jsonl",
		"run\u202egz.lmthjsonl":                "run_gz.lmthjsonl",
		"tab\there\nnewline":                   "tab_here_newline",
		"Qwen3-32B 2026-11-17 15:25Z.jsonl.gz": "Qwen3-32B 2026-11-17 15_25Z.jsonl.gz",
		"  spaced  ":                           "spaced",
		"":                                     "",
		"dir/":                                 "",
		"\u0430\u0431\u0432-\u05e9\u05dc\u05d5\u05dd.jsonl": "\u0430\u0431\u0432-\u05e9\u05dc\u05d5\u05dd.jsonl",
	} {
		assert.Equal(t, want, UploadFileName(in), "%q", in)
	}
	for in, want := range map[string]string{
		"Cafe\u0301.jsonl":                     "Cafe\u0301.jsonl",
		"\u0939\u093f\u0902\u0926\u0940.jsonl": "\u0939\u093f\u0902\u0926\u0940.jsonl",
		"e\u0301\u0301\u0301\u0301x":           "e\u0301\u0301__x",
		"\u0301start":                          "_start",
		"\u3164\u3164.jsonl":                   "__.jsonl",
		"a\u200bb\u2066c":                      "a_b_c",
	} {
		assert.Equal(t, want, UploadFileName(in), "%+q", in)
	}
	long := UploadFileName(strings.Repeat("a", 300))
	assert.Equal(t, strings.Repeat("a", 80), long)
}

// The file name an upload gives, ?filename=, reaches the Uploader cleaned,
// and none is empty; with Kubernetes authentication the query does not
// change the path an upload is authorized on (here as post, which this
// cluster refuses).
func TestUploadPassesTheFileName(t *testing.T) {
	u := &stubUploader{}
	h := New(Config{Resolver: &stubResolver{}, Uploader: u}).Handler()
	for target, want := range map[string]string{
		UploadPath + "?filename=" + url.QueryEscape(`C:\runs\run <1>.jsonl.gz`): "run _1_.jsonl.gz",
		UploadPath + "?filename=": "",
		UploadPath:                "",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(target))
		req.Header.Set("Content-Type", "application/octet-stream")
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, want, u.fileName, target)
	}

	c := newCluster("/whatif"+UploadPath, "")
	ah := NewAuthenticator(c, nil, 0, nil).Wrap(h)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, UploadPath+"?filename=run.jsonl.gz", strings.NewReader("y"))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer good")
	ah.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.NotNil(t, c.lastSAR.NonResourceAttributes)
	assert.Equal(t, authzv1.NonResourceAttributes{Path: "/whatif/v1/recordings", Verb: "post"}, *c.lastSAR.NonResourceAttributes)
}
