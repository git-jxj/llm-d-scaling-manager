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
	return &Supervisor{client: f.server.Client(), baseURL: f.server.URL, deleteRetryDelay: time.Millisecond}
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

// deleteScript answers DELETEs with the given status codes in turn (the last
// one repeats), and every list with listBody. It counts both.
type deleteScript struct {
	codes    []int
	listBody string
	deletes  int
	lists    int
}

func (d *deleteScript) respond(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		code := d.codes[min(d.deletes, len(d.codes)-1)]
		d.deletes++
		if code == http.StatusOK {
			_, _ = w.Write([]byte(`{"status":"terminated","instance_id":"evicted"}`))
			return
		}
		// The body launcher v0.8.0 sends with its 500.
		http.Error(w, "[Errno 22] Invalid argument", code)
		return
	}
	d.lists++
	_, _ = w.Write([]byte(d.listBody))
}

const (
	listEmpty   = `{"revision":3,"total_instances":0,"running_instances":0,"instances":[]}`
	listStopped = `{"revision":3,"total_instances":1,"running_instances":0,` +
		`"instances":[{"instance_id":"evicted","status":"stopped","options":"--port 9001"}]}`
)

func listWithStatus(status string) string {
	return `{"revision":3,"total_instances":1,"running_instances":0,` +
		`"instances":[{"instance_id":"evicted","status":"` + status + `","options":"--port 9001"}]}`
}

func runDelete(t *testing.T, d *deleteScript) error {
	t.Helper()
	f := newFakeSupervisor(t)
	f.respond = d.respond
	return f.client().Delete(context.Background(), "evicted")
}

func TestDeleteRetriesA500AndTakesTheRetrysSuccess(t *testing.T) {
	// Measured on launcher v0.8.0: a 500 after the SIGKILL escalation, then 200
	// for the same DELETE sent again, which also removes the stale entry.
	d := &deleteScript{codes: []int{500, 200}, listBody: listStopped}
	if err := runDelete(t, d); err != nil {
		t.Fatalf("a retry that succeeds must make the delete succeed: %v", err)
	}
	if d.deletes != 2 || d.lists != 0 {
		t.Errorf("want 2 deletes and no list, got %d deletes and %d lists", d.deletes, d.lists)
	}
}

func TestDeleteTakesA404OnTheRetryAsSuccess(t *testing.T) {
	d := &deleteScript{codes: []int{500, 404}, listBody: listStopped}
	if err := runDelete(t, d); err != nil {
		t.Fatalf("an instance gone by the retry is deleted: %v", err)
	}
}

func TestDeleteKeepsTheErrorWhenBothFailAndTheInstanceIsListed(t *testing.T) {
	// "stopped" says only that the API server is dead. EngineCore or a worker
	// may still hold the GPU, so a listed instance is never proof of a delete.
	d := &deleteScript{codes: []int{500, 500}, listBody: listStopped}
	err := runDelete(t, d)
	if err == nil {
		t.Fatal("two failed deletes of a listed instance must stay an error")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "Invalid argument") {
		t.Errorf("the supervisor's own words must survive: %v", err)
	}
	if d.deletes != 2 || d.lists != 1 {
		t.Errorf("want 2 deletes and 1 list, got %d and %d", d.deletes, d.lists)
	}
}

func TestDeleteSucceedsWhenBothFailButTheInstanceIsGone(t *testing.T) {
	d := &deleteScript{codes: []int{500, 500}, listBody: listEmpty}
	if err := runDelete(t, d); err != nil {
		t.Fatalf("an instance no longer listed is deleted: %v", err)
	}
}

func TestDeleteRetriesEvery5xx(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		d := &deleteScript{codes: []int{code, 200}, listBody: listStopped}
		if err := runDelete(t, d); err != nil {
			t.Errorf("%d then 200 must succeed: %v", code, err)
		}
		if d.deletes != 2 {
			t.Errorf("%d: want the delete retried, got %d deletes", code, d.deletes)
		}
		d = &deleteScript{codes: []int{code, code}, listBody: listStopped}
		if err := runDelete(t, d); err == nil {
			t.Errorf("%d twice with the instance listed must stay an error", code)
		}
	}
}

func TestDeleteNeverTakesAListedStatusAsProof(t *testing.T) {
	for _, status := range []string{"", "stopping", "not_running", "running"} {
		d := &deleteScript{codes: []int{500, 500}, listBody: listWithStatus(status)}
		if err := runDelete(t, d); err == nil {
			t.Errorf("status %q: a listed instance must keep the error", status)
		}
	}
}

func TestDeleteKeepsTheErrorWhenTheListFails(t *testing.T) {
	f := newFakeSupervisor(t)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "[Errno 22] Invalid argument", http.StatusInternalServerError)
	}
	if err := f.client().Delete(context.Background(), "evicted"); err == nil {
		t.Fatal("an unreadable list proves nothing; the 500 must stay an error")
	}
}

func TestDeleteDoesNotRetryA4xx(t *testing.T) {
	// Only a server failure is retried. A refusal is the supervisor's answer.
	d := &deleteScript{codes: []int{http.StatusBadRequest}, listBody: listEmpty}
	if err := runDelete(t, d); err == nil {
		t.Fatal("a 400 must stay an error")
	}
	if d.deletes != 1 || d.lists != 0 {
		t.Errorf("want only the DELETE, got %d deletes and %d lists", d.deletes, d.lists)
	}
}

func TestDeleteRetryHonoursContextCancellation(t *testing.T) {
	f := newFakeSupervisor(t)
	d := &deleteScript{codes: []int{500, 200}, listBody: listEmpty}
	f.respond = d.respond
	s := f.client()
	s.deleteRetryDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Delete(ctx, "evicted"); err == nil {
		t.Fatal("a cancelled wait must return the first error")
	}
	if d.deletes != 1 {
		t.Errorf("want no retry after cancellation, got %d deletes", d.deletes)
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
