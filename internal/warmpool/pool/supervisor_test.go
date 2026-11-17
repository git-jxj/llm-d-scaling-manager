package pool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSupervisor stands in for the launcher, recording what it was asked.
type fakeSupervisor struct {
	server    *httptest.Server
	lastPath  string
	lastBody  string
	respond   func(w http.ResponseWriter, r *http.Request)
	callCount int
}

func newFakeSupervisor(t *testing.T) *fakeSupervisor {
	t.Helper()
	f := &fakeSupervisor{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.callCount++
		f.lastPath = r.Method + " " + r.URL.Path
		if r.Body != nil {
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			f.lastBody = string(buf[:n])
		}
		f.respond(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

// client points a Supervisor at the fake, bypassing the podIP:port construction.
func (f *fakeSupervisor) client() *Supervisor {
	return &Supervisor{client: f.server.Client(), baseURL: f.server.URL}
}

func TestListAcceptsTheLauncherEnvelopeShape(t *testing.T) {
	// The shape the pinned launcher actually returns, copied from a live Pod.
	// Without this case the parser passed its tests and could not read a single
	// real Pod: the pool reported itself empty on every reconcile.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"revision":1,"total_instances":1,"running_instances":1,` +
			`"instances":[{"instance_id":"qwen-a","status":"running",` +
			`"options":"--model /model-cache/models/Qwen/Qwen3-0.6B --port 9001"}]}`))
	}
	got, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "qwen-a" {
		t.Fatalf("envelope not unwrapped: %+v", got)
	}
	if got[0].Status != "running" || !strings.Contains(got[0].Options, "Qwen3-0.6B") {
		t.Errorf("fields lost: %+v", got[0])
	}
}

func TestListReportsAnEmptyPoolRatherThanFailing(t *testing.T) {
	// An empty pool is the ordinary state of a Pod that holds nothing yet, and
	// it must not read as an error -- "could not be read" makes the reconciler
	// drop the Pod from the observation entirely.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"revision":0,"total_instances":0,"running_instances":0,"instances":[]}`))
	}
	got, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("an empty pool must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want no instances, got %+v", got)
	}
}

func TestListAcceptsTheLauncherMapShape(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"qwen":{"status":"running","options":"--model Qwen/Qwen3-0.6B --port 9001"}}`))
	}
	got, err := f.client().List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "qwen" {
		t.Fatalf("id must be filled in from the map key: %+v", got)
	}
	if !strings.Contains(got[0].Options, "Qwen3-0.6B") {
		t.Errorf("options lost: %+v", got[0])
	}
}

func TestListAlsoAcceptsAList(t *testing.T) {
	// Tolerated on purpose: the launcher is copied code that may change shape,
	// and a read that breaks on it would take the whole pool down.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"instance_id":"a","status":"running"}]`))
	}
	got, err := f.client().List(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("List = %+v, err %v", got, err)
	}
}

func TestCreateSendsTheCallersIdAndOptions(t *testing.T) {
	// Identity keyed on the model, and the port inside the options, are the two
	// decisions that let the copied launcher serve a pool unmodified.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"my-variant","status":"running"}`))
	}
	spec := InstanceSpec{
		Options: "--model Qwen/Qwen3-0.6B --port 9001",
		EnvVars: map[string]string{"VLLM_SERVER_DEV_MODE": "1"},
	}
	inst, err := f.client().Create(context.Background(), "my-variant", spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ID != "my-variant" {
		t.Errorf("instance id = %q", inst.ID)
	}
	if f.lastPath != "PUT /v2/vllm/instances/my-variant" {
		t.Errorf("wrong request: %s", f.lastPath)
	}
	var sent InstanceSpec
	if err := json.Unmarshal([]byte(f.lastBody), &sent); err != nil {
		t.Fatalf("body was not the spec: %q", f.lastBody)
	}
	if sent.EnvVars["VLLM_SERVER_DEV_MODE"] != "1" {
		t.Error("dev mode must be passed: /sleep and /wake_up do not exist without it")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	// A controller that must remember whether it already evicted something will
	// be wrong about it after a restart.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such instance", http.StatusNotFound)
	}
	if err := f.client().Delete(context.Background(), "gone"); err != nil {
		t.Fatalf("deleting an absent instance must succeed: %v", err)
	}
}

// failedDeleteThenList answers a DELETE with the 500 launcher v0.8.0 gives when
// psutil's wait raises EINVAL, and a list with whatever instances are given.
func failedDeleteThenList(listBody string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, "[Errno 22] Invalid argument", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(listBody))
	}
}

func TestDeleteTreatsA500AsDoneWhenTheInstanceIsGone(t *testing.T) {
	// Measured: v0.8.0 answers 500 after escalating to SIGKILL with the engine
	// and its GPU memory already freed. Failing the eviction then makes the next
	// pass count a dead copy as resident and evict it again.
	f := newFakeSupervisor(t)
	f.respond = failedDeleteThenList(`{"revision":3,"total_instances":0,"running_instances":0,"instances":[]}`)
	if err := f.client().Delete(context.Background(), "evicted"); err != nil {
		t.Fatalf("a 500 for an instance that is gone must be success: %v", err)
	}
	if f.callCount != 2 || f.lastPath != "GET "+instancesPath {
		t.Errorf("want the DELETE then one list, got %d calls, last %q", f.callCount, f.lastPath)
	}
}

func TestDeleteTreatsA500AsDoneWhenTheInstanceIsStopped(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = failedDeleteThenList(`{"revision":3,"total_instances":1,"running_instances":0,` +
		`"instances":[{"instance_id":"evicted","status":"stopped","options":"--port 9001"}]}`)
	if err := f.client().Delete(context.Background(), "evicted"); err != nil {
		t.Fatalf("a 500 for a stopped instance must be success: %v", err)
	}
}

func TestDeleteKeepsA500WhenTheInstanceStillRuns(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = failedDeleteThenList(`{"revision":3,"total_instances":1,"running_instances":1,` +
		`"instances":[{"instance_id":"evicted","status":"running","options":"--port 9001"}]}`)
	err := f.client().Delete(context.Background(), "evicted")
	if err == nil {
		t.Fatal("a 500 for an instance still running must stay an error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "Invalid argument") {
		t.Errorf("the supervisor's own words must survive: %v", err)
	}
}

func TestDeleteKeepsA500WhenTheListFails(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "[Errno 22] Invalid argument", http.StatusInternalServerError)
	}
	if err := f.client().Delete(context.Background(), "evicted"); err == nil {
		t.Fatal("an unreadable list proves nothing; the 500 must stay an error")
	}
}

func TestDeleteDoesNotListAfterA4xx(t *testing.T) {
	// Only a server failure is re-checked. A refusal is the supervisor's answer.
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad id", http.StatusBadRequest)
	}
	if err := f.client().Delete(context.Background(), "evicted"); err == nil {
		t.Fatal("a 400 must stay an error")
	}
	if f.callCount != 1 {
		t.Errorf("want only the DELETE, got %d calls", f.callCount)
	}
}

func TestErrorsCarryTheSupervisorsOwnWords(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "GPU 0 already has 2 sleepers", http.StatusConflict)
	}
	_, err := f.client().Create(context.Background(), "x", InstanceSpec{Options: "--model m"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "already has 2 sleepers") {
		t.Errorf("the reason must survive: %v", err)
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("the status must survive: %v", err)
	}
}

func TestRequestsHonourContextCancellation(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) { time.Sleep(2 * time.Second) }
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.client().List(ctx); err == nil {
		t.Fatal("a hung supervisor must not hang the loop")
	}
}
