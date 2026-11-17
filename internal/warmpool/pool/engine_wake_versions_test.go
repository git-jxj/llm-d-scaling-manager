package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWakeReadsEachReleasesAnswer covers what /wake_up answers across vLLM
// releases. Up to v0.31 a successful wake is an empty 200. From v0.32 it is a
// JSON status, and a 200 that says "sleeping" is a wake that left part of the
// engine asleep: /health still answers, so without reading the body the pool
// would put an engine that answers 503 into service.
func TestWakeReadsEachReleasesAnswer(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "v0.26 empty body", body: ""},
		{name: "v0.27.1 empty body", body: ""},
		{name: "v0.32 awake", body: `{"status":"awake","tags":null}`},
		{name: "v0.34 awake", body: `{"status":"awake","tags":null}`},
		{name: "v0.34 still sleeping", body: `{"status":"sleeping","tags":["weights"]}`, wantErr: `status "sleeping"`},
		{name: "unknown release, text body", body: "OK"},
		{name: "unknown release, JSON without status", body: `{"ok":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == wakeUpPath {
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			e := &Engine{client: srv.Client(), baseURL: srv.URL, pollWait: 1}

			err := e.Wake(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Wake: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Wake accepted %s; a partial wake must be an error", tc.body)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Wake error %q does not name the status %q", err, tc.wantErr)
			}
		})
	}
}
