package whatifapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// UploadPath is the path a recording is uploaded to. With Kubernetes
// authentication it is the one path authorized as post, not get: the
// read-only grant does not let a user upload.
const UploadPath = "/v1/recordings"

// DefaultUploadBytes is the largest upload a server takes when its Config
// names none: a day-long recording of a 16-replica P/D fleet is about 95 MB
// compressed.
const DefaultUploadBytes = 256 << 20

// uploadDeadline is how long an upload may take to arrive and be checked: the
// server's 30 s read timeout is for a request of a few hundred bytes, and a
// recording of a hundred megabytes through a port-forward takes minutes.
const uploadDeadline = 15 * time.Minute

// uploadIdle is how long an upload may send nothing before it is stopped: a
// client that stalls does not hold the one upload slot for uploadDeadline.
const uploadIdle = time.Minute

// uploadTypes are the media types an upload may be sent as. Neither is one
// a page on another site may post without asking first (a CORS preflight,
// which this server never answers), so no other page can upload through a
// user's browser.
var uploadTypes = []string{"application/octet-stream", "application/gzip"}

// Uploader keeps a recording uploaded with POST /v1/recordings
// (cmd/capacity-planner's).
type Uploader interface {
	// Upload reads body, the recording as sent, and keeps it when it reads
	// as a recording. created is false when the same bytes were kept
	// before. An error reading body is returned wrapped, so that a body past
	// the server's bound is told apart (http.MaxBytesError).
	Upload(ctx context.Context, body io.Reader) (out UploadResponse, created bool, err error)
}

// TooLargeError is an upload past one of the server's bounds: 413, with its
// message.
type TooLargeError struct{ Err error }

// Error is the bound the upload is past.
func (e TooLargeError) Error() string {
	if e.Err == nil {
		return "too large"
	}
	return e.Err.Error()
}

// Unwrap is the bound's error.
func (e TooLargeError) Unwrap() error { return e.Err }

// errUploadOff is an upload to a server that takes none.
var errUploadOff = errors.New("this server takes no uploads: whoever runs it starts it with --allow-upload")

// Quantity is n bytes as a flag gives them (256Mi).
func Quantity(n int64) string { return resource.NewQuantity(n, resource.BinarySI).String() }

// handleUpload is POST /v1/recordings: the body, a recording as a file holds
// it, kept by the Uploader under a name the server gives it. It is refused
// when the server takes no uploads (403), sent as another media type (415),
// or longer than the server's bound (413, before a byte of it is read when
// its length is declared). One upload is read at a time.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if s.uploader == nil {
		writeError(w, http.StatusForbidden, errUploadOff)
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (mt != uploadTypes[0] && mt != uploadTypes[1]) {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("content type %q, want %s or %s: the file's bytes as they are",
			r.Header.Get("Content-Type"), uploadTypes[0], uploadTypes[1]))
		return
	}
	tooLarge := TooLargeError{Err: fmt.Errorf("the upload is larger than this server takes, %s (--upload-max-size)", Quantity(s.uploadMax))}
	if r.ContentLength > s.uploadMax {
		writeErr(w, tooLarge)
		return
	}
	if !acquire(s.uploadSlots) {
		writeErr(w, errBusy)
		return
	}
	defer func() { <-s.uploadSlots }()
	// A recorder in a test has no connection to set deadlines on.
	rc := http.NewResponseController(w)
	end := time.Now().Add(s.uploadDeadline)
	_ = rc.SetReadDeadline(end)
	_ = rc.SetWriteDeadline(end.Add(time.Minute))
	// The check stops at end too, not only the body's reads.
	ctx, cancel := context.WithDeadline(r.Context(), end)
	defer cancel()
	in := &idleReader{r: http.MaxBytesReader(w, r.Body, s.uploadMax), rc: rc, idle: s.uploadIdle, end: end}
	out, created, err := s.uploader.Upload(ctx, in)
	// A context ended once end has passed is the cap, not a client gone: as
	// a read cut off, 408.
	pastEnd := !time.Now().Before(end) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
	switch {
	case errors.As(err, new(*http.MaxBytesError)):
		err = tooLarge
	case errors.Is(err, os.ErrDeadlineExceeded), pastEnd:
		// The rest of the body is never read: the connection is not reused.
		w.Header().Set("Connection", "close")
		writeError(w, http.StatusRequestTimeout, fmt.Errorf("the upload was stopped: nothing arrived for %s, or it took longer than %s", s.uploadIdle, s.uploadDeadline))
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	out.SchemaVersion = SchemaVersion
	body, err := marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	if created {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
		return
	}
	writeBody(w, body)
}

// idleReader reads an upload's body, moving the connection's read deadline
// to idle from each read, never past end: a client that stops sending is cut
// off after idle, not end. Once the body is read the deadline is end again,
// so that the server notices a client that leaves while the upload is
// checked (its context ends) and no sooner.
type idleReader struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
	end  time.Time
}

func (b *idleReader) Read(p []byte) (int, error) {
	next := time.Now().Add(b.idle)
	if next.After(b.end) {
		next = b.end
	}
	// A recorder in a test has no connection to set deadlines on.
	_ = b.rc.SetReadDeadline(next)
	n, err := b.r.Read(p)
	// Any other error leaves the deadline passed: the server then gives up
	// on the rest of the body at once, rather than wait for it before it
	// answers.
	if errors.Is(err, io.EOF) {
		_ = b.rc.SetReadDeadline(b.end)
	}
	return n, err
}
