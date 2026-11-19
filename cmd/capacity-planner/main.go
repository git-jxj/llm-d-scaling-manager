// Command capacity-planner is the capacity planner's command line
// (docs/proposals/slo-capacity-planner.md, "CLI reference"). It captures a
// past window from Prometheus, turns a capture into a learner recording, and
// answers the planner's two questions from a recording, replayed through the
// analyzers to the cycle asked for:
//
//	capacity-planner capture         --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...)
//	                                 --from T0 --to T1 [--step 15s] [--policy FILE] -o run.capture.jsonl.gz
//	capacity-planner generate        -in run.capture.jsonl.gz -o run.jsonl
//	capacity-planner freeze          --recording FILE --spec FILE [-o FILE] [--targets FILE] [--ca2 BUCKET=CA2]
//	capacity-planner replicas-to-slo --recording FILE [--cycle N|last] [--model M] [--decode N]
//	                                 [--headroom H | --rate R] [--tpot-percentile P [--assume-tpot-ratio R]]
//	                                 [--ttft-percentile P] [--prefill N] [--prefill-variant V] [--assume-vp TOKENS]
//	                                 [--assume-a D] [--assume-prompt-cv CV] [--arrival-cv2 C] [--flow-control on|off] [--assume-router-hop D] [-o json]
//	capacity-planner slo-to-rate     --recording FILE [--cycle N|last] [--model M] [--decode N]
//	                                 --tpot 4ms[@p90] [--assume-tpot-ratio R] [--headroom H]
//	                                 [--ttft 200ms[@p95]] [--prefill N] [--prefill-variant V] [--assume-vp TOKENS]
//	                                 [--assume-a D] [--assume-prompt-cv CV] [--arrival-cv2 C] [--flow-control on|off] [--assume-router-hop D] [-o json]
//	capacity-planner joint-ttft      --recording FILE [--cycle N|last] [--model M] [--prefill N] [--decode N]
//	                                 [--rate R] [--prompt I] [--hop D] [-o json]
//	capacity-planner replicas-to-slo|slo-to-rate --prometheus URL --namespace NS (--kube-state-metrics | --role-of ROLE=SELECTOR ...)
//	                                 --from T0 --to T1 [capture's flags] [the question's flags]
//	capacity-planner at-replicas     --recording FILE [--cycle N|last] [--model M] [--prefill N] [--decode N]
//	                                 [--shape S ...] [--load L ...] [--load-statistic mean|p95|peak | --load-seen R] [--load-seen-x X]
//	                                 [--ttft 2s@p90] [--tpot 30ms] [--headroom H] [--ttft-percentile 50,90] [--tpot-percentile 90]
//	                                 [--arrival-cv2 C] [the --assume-* flags] [-o json]
//	capacity-planner day-plan        (--recording FILE | --prometheus URL capture's flags --namespace NS [--from T0 --to T1])
//	                                 [--bucket 5m] [--size-for mean|p95|max] [--ttft 500ms@p95] [--tpot 4ms] [-o json]
//	capacity-planner serve           [--recordings DIR] [--prometheus URL capture's flags --namespace NS [--allowed-namespaces NS,...]]
//	                                 [--listen 127.0.0.1:8080] [--auth none|kubernetes --tls-cert-file F --tls-key-file F [--token-audiences A,...]]
//	                                 [--listen-all-in-container (in a container: --listen 0.0.0.0:PORT, no auth)]
//	capacity-planner version         (the release and commit; the controller of the recordings is released under the same tag)
//
// capture writes the window's cycle inputs (internal/whatifsource);
// generate plays them through the analyzers in the engine's order
// (learnreplay, Generate mode) and writes the recording, which
// learnergolden export pseudonymises and learnergolden add accepts at the
// commit that generated it. replicas-to-slo and slo-to-rate read a
// recording (learnreplay.StateAt): a file, or with --prometheus the
// recording capture and generate would write for the window, made in
// memory along the same path, so that the answer is the files' answer.
//
// A TTFT is asked for by a TTFT flag; with none, the answers are decode's
// alone. It prices a P/D fleet (ttft.go), and prints UNVALIDATED. joint-ttft
// prices the joint prefill and decode model's mean at a fleet (joint.go). Aggregated
// TTFT and per-bucket answers are not built (the planner's M1).
//
// at-replicas judges one fleet at the load the window saw: the utilization
// there, the knees, the SLO against load and a verdict (report.go); POST
// /v1/report answers the same.
//
// serve runs the capacity planner's HTTP API on the same code paths
// (serve.go, internal/whatifapi; docs/developer-guide/capacity-planner-serve.md).
//
// Exit codes: 0 done, and every role answered; 3 a role refused (the
// output is still printed); 2 a usage or input error (a bad flag, an
// unreadable or malformed capture or recording, overlapping --role-of
// selectors, a window too large without --force); 1 anything else (a failed
// Prometheus query, a Generate or a replay that fails inside the analyzers).
//
// The command was named whatif before. Run under that name (make
// capacity-planner also builds bin/whatif), it works the same and first
// prints a one-line deprecation note to stderr. whatif will be removed in
// v0.13.0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// progName is the command's name in its usage, errors and warnings.
const progName = "capacity-planner"

// deprecatedName is the command's old name, still accepted until deprecatedNameRemovedIn.
const deprecatedName = "whatif"

// deprecatedNameRemovedIn is the release that drops deprecatedName.
const deprecatedNameRemovedIn = "v0.13.0"

// deprecationNote is printed to stderr when the command runs as
// deprecatedName.
const deprecationNote = deprecatedName + ": deprecated: the command is now " + progName +
	" (make " + progName + ", bin/" + progName + "); " + deprecatedName + " will be removed in " + deprecatedNameRemovedIn

// invokedAsDeprecated reports whether argv0, the path the command was run
// as, names the old command: whatif, or whatif.exe on Windows, in any case.
func invokedAsDeprecated(argv0 string) bool {
	base := filepath.Base(argv0)
	if len(base) > len(".exe") && strings.EqualFold(base[len(base)-len(".exe"):], ".exe") {
		base = base[:len(base)-len(".exe")]
	}
	return strings.EqualFold(base, deprecatedName)
}

// warnIfDeprecated writes deprecationNote to stderr when argv0 names the
// old command, and nothing otherwise.
func warnIfDeprecated(argv0 string, stderr io.Writer) {
	if invokedAsDeprecated(argv0) {
		fmt.Fprintln(stderr, deprecationNote)
	}
}

func main() {
	warnIfDeprecated(os.Args[0], os.Stderr)
	// The analyzers the replay runs log through controller-runtime: with no
	// logger set it prints a stack trace on a run of 30 s or more.
	ctrllog.SetLogger(logr.Discard())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		var s exitStatus
		if !errors.As(err, &s) {
			fmt.Fprintln(os.Stderr, progName+":", err)
		}
		os.Exit(exitCode(err))
	}
}

// usageError is a usage or input error: exit 2.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// exitStatus ends a command whose output is already printed with a code
// other than 0 (whatif.ExitCode): nothing more is printed.
type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit %d", int(s)) }

// exitCode is the exit code for err: an exitStatus's own, 2 for a usage or
// input error, 1 for anything else.
func exitCode(err error) int {
	var s exitStatus
	if errors.As(err, &s) {
		return int(s)
	}
	var u usageError
	if errors.As(err, &u) {
		return 2
	}
	return 1
}

const usage = "usage: capacity-planner at-replicas|capture|day-plan|freeze|generate|joint-ttft|replicas-to-slo|serve|slo-to-rate|version [flags]; -h after a command lists its flags"

// command is one subcommand: it parses its own flags from args and writes
// what it reports to out, and its warnings to warn (stderr).
type command func(ctx context.Context, args []string, out, warn io.Writer) error

// commands is every subcommand, by name.
var commands = map[string]command{
	"at-replicas":     runAtReplicas,
	"capture":         runCapture,
	"day-plan":        runDayPlan,
	"freeze":          runFreeze,
	"generate":        runGenerate,
	"joint-ttft":      runJointTTFT,
	"replicas-to-slo": runReplicasToSLO,
	"serve":           runServe,
	"slo-to-rate":     runSLOToRate,
	"version":         runVersion,
}

// run runs one command. Asking for help is not an error. The analyzers a
// replay runs log through controller-runtime, which the CLI never shows; a
// replay past 30 s with no logger set says so on stderr, with a stack.
func run(ctx context.Context, args []string, out, warn io.Writer) error {
	ctrllog.SetLogger(logr.Discard())
	if len(args) == 0 {
		return usageError{errors.New(usage)}
	}
	cmd, ok := commands[args[0]]
	if !ok {
		return usagef("unknown command %q; %s", args[0], usage)
	}
	return cmd(ctx, args[1:], out, warn)
}

// newFlags is a flag set for one command that reports its errors as usage
// errors and writes its help to out.
func newFlags(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(progName+" "+name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// parse parses args; ok is false when the caller should stop: help was
// asked for (err nil) or the flags were wrong (a usage error).
func parse(fs *flag.FlagSet, args []string) (bool, error) {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return false, nil
	case err != nil:
		return false, usageError{err}
	case fs.NArg() > 0:
		return false, usagef("unexpected argument %q", fs.Arg(0))
	}
	return true, nil
}
