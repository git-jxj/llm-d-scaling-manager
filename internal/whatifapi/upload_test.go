package whatifapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
}

func (u *stubUploader) Upload(_ context.Context, body io.Reader) (UploadResponse, bool, error) {
	u.calls++
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
