package main

import (
	"bytes"
	"context"
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
// on the staircase, interrupted during its replay or its pricing, returns
// well inside a grace of a minute, exits 130 and says it was stopped, and
// writes no JSON error object for an answer it never reached.
func TestExecute_AtReplicasStopsOnSIGINT(t *testing.T) {
	for _, after := range []time.Duration{100 * time.Millisecond, 1500 * time.Millisecond} {
		t.Run(after.String(), func(t *testing.T) {
			e := startExecute(atStair("-o", "json"), time.Minute)
			time.Sleep(after)
			sent := time.Now()
			e.sigs <- os.Interrupt
			code := e.exitWithin(t, 45*time.Second)
			if code == 0 {
				t.Skipf("the report finished within %s, before the signal", after)
			}
			if code != 130 {
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
		})
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
