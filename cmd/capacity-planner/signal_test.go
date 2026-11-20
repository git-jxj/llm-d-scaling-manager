package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a bytes.Buffer safe to write from the command's goroutine
// and read from the test's.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// withCommand adds a command under name for the test.
func withCommand(t *testing.T, name string, cmd command) {
	t.Helper()
	commands[name] = cmd
	t.Cleanup(func() { delete(commands, name) })
}

// executed is execute run in the background: its exit code arrives on code.
type executed struct {
	code     chan int
	out, err *lockedBuffer
	sigs     chan os.Signal
}

func startExecute(args []string, grace time.Duration) *executed {
	e := &executed{code: make(chan int, 1), out: &lockedBuffer{}, err: &lockedBuffer{}, sigs: make(chan os.Signal, 2)}
	go func() { e.code <- execute(args, e.out, e.err, e.sigs, grace) }()
	return e
}

// exitWithin is the exit code execute returned within d, or a test failure.
func (e *executed) exitWithin(t *testing.T, d time.Duration) int {
	t.Helper()
	select {
	case c := <-e.code:
		return c
	case <-time.After(d):
		t.Fatalf("execute did not return within %s; stderr: %s", d, e.err.String())
		return 0
	}
}

// A command that never reads its context, the way a long figure did, is cut
// off grace after SIGTERM: exit 143 and a line that says so, not a run that
// goes on until kill -9.
func TestExecute_ACommandThatIgnoresItsContextIsCutOff(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	withCommand(t, "test-ignore-context", func(context.Context, []string, io.Writer, io.Writer) error {
		close(started)
		<-release
		return nil
	})
	e := startExecute([]string{"test-ignore-context"}, 200*time.Millisecond)
	<-started
	sent := time.Now()
	e.sigs <- syscall.SIGTERM
	if code := e.exitWithin(t, 5*time.Second); code != 143 {
		t.Fatalf("exit %d, want 143 (128 + SIGTERM)", code)
	}
	if el := time.Since(sent); el > 2*time.Second {
		t.Fatalf("returned %s after SIGTERM, want about the 200ms grace", el)
	}
	if got := e.err.String(); !strings.Contains(got, "stopped by SIGTERM: the command did not stop within 200ms, so it was cut off") {
		t.Fatalf("stderr %q does not say it was cut off", got)
	}
}

// A second signal ends the wait for the command at once.
func TestExecute_ASecondSignalStopsAtOnce(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	withCommand(t, "test-ignore-context", func(context.Context, []string, io.Writer, io.Writer) error {
		close(started)
		<-release
		return nil
	})
	e := startExecute([]string{"test-ignore-context"}, time.Hour)
	<-started
	e.sigs <- os.Interrupt
	e.sigs <- os.Interrupt
	if code := e.exitWithin(t, 5*time.Second); code != 130 {
		t.Fatalf("exit %d, want 130 (128 + SIGINT)", code)
	}
	if got := e.err.String(); !strings.Contains(got, "stopped by a second signal (SIGINT)") {
		t.Fatalf("stderr %q does not name the second signal", got)
	}
}

// A real verb stops on its own once the signal ends its context: at-replicas
// on the staircase, interrupted as it starts (it takes seconds to answer),
// returns well inside a grace of a minute, exits 130 and says it was
// stopped, and writes no JSON error object for an answer it never reached.
func TestExecute_AtReplicasStopsOnSIGINT(t *testing.T) {
	started := make(chan struct{})
	withCommand(t, "test-at-replicas", func(ctx context.Context, args []string, out, warn io.Writer) error {
		close(started)
		return runAtReplicas(ctx, args, out, warn)
	})
	e := startExecute(append([]string{"test-at-replicas"}, atStair("-o", "json")[1:]...), time.Minute)
	<-started
	sent := time.Now()
	e.sigs <- os.Interrupt
	if code := e.exitWithin(t, 45*time.Second); code != 130 {
		t.Fatalf("exit %d, want 130; stderr: %s", code, e.err.String())
	}
	if el := time.Since(sent); el > 30*time.Second {
		t.Fatalf("stopped %s after SIGINT, want promptly", el)
	}
	if got := e.err.String(); got != "capacity-planner: stopped by SIGINT\n" {
		t.Fatalf("stderr %q, want only the stopped line", got)
	}
	if got := e.out.String(); got != "" {
		t.Fatalf("stdout %q, want nothing for a stopped run", got)
	}
}

// The report reads its context at every fleet it prices, so a report whose
// context ends while it prices (the serve path, POST /v1/report, runs the
// same buildReport) stops with the context's error, not an answer. The
// staircase report prices for seconds; the context ends 200 ms in.
func TestBuildReport_StopsWhenItsContextEnds(t *testing.T) {
	st := stairReplay.state(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	rep, err := buildReport(ctx, st, &servedState{st: st, decodeVariant: "variant-01"}, stairQuery(2, 2, 0))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("report %v, error %v; want context.Canceled", rep != nil, err)
	}
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("stopped %s in, want promptly", el)
	}
	// A context already ended prices nothing.
	done, stop := context.WithCancel(t.Context())
	stop()
	if _, err := buildReport(done, st, &servedState{st: st, decodeVariant: "variant-01"}, stairQuery(2, 2, 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v on an ended context, want context.Canceled", err)
	}
}

// A command that still returns its own error after the signal exits as it
// would have: the error printed and its code, not the signal's.
func TestExecute_AnErrorAfterTheSignalIsTheCommands(t *testing.T) {
	withCommand(t, "test-own-error", func(ctx context.Context, _ []string, _, _ io.Writer) error {
		<-ctx.Done()
		return usagef("refused for its own reason")
	})
	e := startExecute([]string{"test-own-error"}, time.Minute)
	e.sigs <- syscall.SIGTERM
	if code := e.exitWithin(t, 10*time.Second); code != 2 {
		t.Fatalf("exit %d, want the usage error's 2", code)
	}
	if got := e.err.String(); got != "capacity-planner: refused for its own reason\n" {
		t.Fatalf("stderr %q, want the command's error alone", got)
	}
}

// With -o json a stopped run writes no JSON error object, however its
// context error is wrapped; any other error still does.
func TestAsJSONError_PassesAStoppedRunThrough(t *testing.T) {
	args := []string{"-o", "json"}
	for _, err := range []error{context.Canceled, fmt.Errorf("pricing 2 + 2: %w", context.Canceled)} {
		var out bytes.Buffer
		if got := asJSONError(reportQuestion, args, &out, err); got != err || out.Len() != 0 {
			t.Errorf("%v: returned %v and wrote %q; want it back and nothing written", err, got, out.String())
		}
	}
	var out bytes.Buffer
	got := asJSONError(reportQuestion, args, &out, context.DeadlineExceeded)
	var s exitStatus
	if !errors.As(got, &s) || !strings.Contains(out.String(), `"exitCode": 1`) {
		t.Errorf("DeadlineExceeded: returned %v and wrote %q; want a JSON error object, exit 1", got, out.String())
	}
}

// serve is not cut off at the grace: it stops on its own once its listener
// is shut down, and a clean shutdown exits 0.
func TestExecute_ServeShutsDownCleanlyOnSIGTERM(t *testing.T) {
	up := make(chan net.Addr, 1)
	listening = func(a net.Addr) { up <- a }
	t.Cleanup(func() { listening = nil })
	e := startExecute([]string{"serve", "--listen", "127.0.0.1:0", "--recordings", t.TempDir(), "--shutdown-timeout", "1s"}, time.Nanosecond)
	select {
	case <-up:
	case code := <-e.code:
		t.Fatalf("serve exited %d before it listened; stderr: %s", code, e.err.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("serve did not listen; stderr: %s", e.err.String())
	}
	e.sigs <- syscall.SIGTERM
	if code := e.exitWithin(t, 10*time.Second); code != 0 {
		t.Fatalf("exit %d, want 0 for a clean shutdown; stderr: %s", code, e.err.String())
	}
	if got := e.err.String(); strings.Contains(got, "stopped by") {
		t.Fatalf("stderr %q: serve was cut off, want it left to shut down", got)
	}
}

// With no signal the exit code is the command's own, as before.
func TestExecute_WithNoSignalExitsAsTheCommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"version"}, 0},
		{[]string{"no-such-command"}, 2},
	} {
		if code := startExecute(c.args, time.Minute).exitWithin(t, 10*time.Second); code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
	}
}
