//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// mainEnv, set to 1, makes the test binary run main with its arguments: the
// signal tests start the command as a process and signal it.
const mainEnv = "CAPACITY_PLANNER_TEST_MAIN"

// startedLine is what test-spin writes to stderr once it runs.
const startedLine = "test-spin: started"

func TestMain(m *testing.M) {
	if os.Getenv(mainEnv) == "1" {
		// test-spin is a command that never reads its context, as a figure
		// that runs for minutes does not.
		commands["test-spin"] = func(_ context.Context, _ []string, _, warn io.Writer) error {
			fmt.Fprintln(warn, startedLine)
			for {
				time.Sleep(time.Second)
			}
		}
		main()
		return
	}
	os.Exit(m.Run())
}

// startMain starts the test binary as the command with args, and returns it
// once it has written ready to stderr, with stderr so far and the rest.
func startMain(t *testing.T, ready string, args ...string) (*exec.Cmd, *bytes.Buffer, chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), mainEnv+"=1")
	cmd.Stdout = io.Discard
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	stderr := &bytes.Buffer{}
	seen := make(chan struct{})
	waited := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(pipe)
		once := false
		for sc.Scan() {
			stderr.WriteString(sc.Text() + "\n")
			if !once && strings.Contains(sc.Text(), ready) {
				once = true
				close(seen)
			}
		}
		waited <- cmd.Wait()
	}()
	if ready == "" {
		return cmd, stderr, waited
	}
	select {
	case <-seen:
	case <-time.After(30 * time.Second):
		t.Fatalf("no %q on stderr within 30s", ready)
	}
	return cmd, stderr, waited
}

// exitAfterSignal sends sig and returns the exit code and how long the
// process took to exit, failing the test past limit.
func exitAfterSignal(t *testing.T, cmd *exec.Cmd, waited chan error, sig os.Signal, limit time.Duration, stderr *bytes.Buffer) (int, time.Duration) {
	t.Helper()
	sent := time.Now()
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), time.Since(sent)
	case <-time.After(limit):
		t.Fatalf("still running %s after %s; stderr: %s", limit, sig, stderr.String())
		return 0, 0
	}
}

// SIGTERM stops a command that never reads its context: the process exits
// 143 within the grace and says it cut the command off. On v0.12.1 the
// signal was caught and ignored, and at-replicas needed kill -9.
func TestSignal_SIGTERMStopsACommandThatIgnoresIt(t *testing.T) {
	cmd, stderr, waited := startMain(t, startedLine, "test-spin")
	code, took := exitAfterSignal(t, cmd, waited, syscall.SIGTERM, 20*time.Second, stderr)
	if code != 143 {
		t.Fatalf("exit %d, want 143; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "capacity-planner: stopped by SIGTERM") {
		t.Fatalf("stderr %q does not say what stopped it", stderr.String())
	}
	t.Logf("exited %s after SIGTERM", took)
}

// SIGTERM stops at-replicas itself during its replay, before the grace:
// exit 143, and the stopped line alone.
func TestSignal_SIGTERMStopsAtReplicas(t *testing.T) {
	cmd, stderr, waited := startMain(t, "", atStair()...)
	time.Sleep(300 * time.Millisecond)
	code, took := exitAfterSignal(t, cmd, waited, syscall.SIGTERM, 20*time.Second, stderr)
	if code == 0 {
		t.Skip("the report finished before the signal")
	}
	if code != 143 || stderr.String() != "capacity-planner: stopped by SIGTERM\n" {
		t.Fatalf("exit %d, stderr %q; want 143 and the stopped line alone", code, stderr.String())
	}
	t.Logf("exited %s after SIGTERM", took)
}
