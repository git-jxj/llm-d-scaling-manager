package whatifapi

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stalledUpload opens a connection to url and sends an upload's headers and
// the first bytes of a body it says is longer, then sends nothing more. It
// answers the response the server sends, read within wait.
func stalledUpload(t *testing.T, url string, wait time.Duration) (*http.Response, time.Duration) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = io.WriteString(conn, "POST "+UploadPath+" HTTP/1.1\r\nHost: localhost\r\n"+
		"Content-Type: application/octet-stream\r\nContent-Length: 1000\r\n\r\nabc")
	require.NoError(t, err)
	start := time.Now()
	require.NoError(t, conn.SetReadDeadline(start.Add(wait)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "no answer within %s: the stalled upload still holds its slot", wait)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, time.Since(start)
}

// An upload that stops sending is stopped once nothing has arrived for the
// idle time, 408, and releases the one upload slot: the next upload is
// read. Before, a stalled client held the slot for the whole 15-minute
// deadline, and every other upload was 503.
func TestUploadStalledClientIsStopped(t *testing.T) {
	s := New(Config{Resolver: &stubResolver{}, Uploader: &stubUploader{}})
	s.uploadIdle = 300 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, took := stalledUpload(t, srv.URL, 10*time.Second)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusRequestTimeout, resp.StatusCode, "%s", body)
	assert.Contains(t, string(body), "nothing arrived for 300ms")
	assert.Less(t, took, 5*time.Second, "stopped after the idle time, not the deadline")
	t.Logf("stopped after %s (idle %s)", took, s.uploadIdle)

	// A client that keeps sending is not stopped, however long it takes
	// past the idle time in all.
	pr, pw := io.Pipe()
	go func() {
		for range 4 {
			_, _ = pw.Write([]byte("abcd"))
			time.Sleep(s.uploadIdle / 2)
		}
		_ = pw.Close()
	}()
	req, err := http.NewRequest(http.MethodPost, srv.URL+UploadPath, pr)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/octet-stream")
	next, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = next.Body.Close() }()
	out, _ := io.ReadAll(next.Body)
	assert.Equal(t, http.StatusCreated, next.StatusCode, "the slot was released: %s", out)
}

// checkingUploader reads the body, then checks it for check, or until the
// request's context ends, and says which on done.
type checkingUploader struct {
	check time.Duration
	done  chan error
}

func (u *checkingUploader) Upload(ctx context.Context, body io.Reader) (UploadResponse, bool, error) {
	if _, err := io.ReadAll(body); err != nil {
		u.done <- err
		return UploadResponse{}, false, err
	}
	select {
	case <-ctx.Done():
		u.done <- ctx.Err()
		return UploadResponse{}, false, ctx.Err()
	case <-time.After(u.check):
		u.done <- nil
		return UploadResponse{Name: "upload:x.jsonl", Bytes: 1}, true, nil
	}
}

// Once the body has arrived, the idle time no longer applies: a check that
// takes longer is not cut off. A client that leaves during the check ends
// the request's context, which the check reads between lines.
func TestUploadCheckOutlivesTheIdleTime(t *testing.T) {
	u := &checkingUploader{check: time.Second, done: make(chan error, 1)}
	s := New(Config{Resolver: &stubResolver{}, Uploader: u})
	s.uploadIdle = 200 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+UploadPath, "application/octet-stream", strings.NewReader("abc"))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NoError(t, <-u.done, "a check five times the idle time ran to its end")

	u.check = time.Minute
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err)
	_, err = io.WriteString(conn, "POST "+UploadPath+" HTTP/1.1\r\nHost: localhost\r\n"+
		"Content-Type: application/octet-stream\r\nContent-Length: 3\r\n\r\nabc")
	require.NoError(t, err)
	time.Sleep(2 * s.uploadIdle)
	require.NoError(t, conn.Close())
	select {
	case err := <-u.done:
		assert.ErrorIs(t, err, context.Canceled, "the client left during the check")
	case <-time.After(10 * time.Second):
		t.Fatal("the check did not see the client leave")
	}
}
