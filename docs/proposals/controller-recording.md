# Record for the capacity planner from the controller

**Status:** proposal, 2026-11-15. Nothing here is built. The open questions
are at the end.

**Audience:** contributors. Operators record today from Prometheus:
[Record a window for the capacity planner](../guides/record-for-capacity-planner/).

**Builds on:** the learner recorder (`internal/signals/learnrec`,
[the learner proposal](analyzer-learner.md), sections 2.4 and 2.5), the
[capacity planner](capacity-planner.md), the
[settings ConfigMap](../reference/configuration.md#the-settings-configmap) and
[Install in two commands](two-command-install.md) (`smt install`).

## The problem

The planner answers from a learner recording. An operator gets one in one of
two ways today:

| way | what it needs | what it misses |
| --- | --- | --- |
| `capacity-planner capture` + `generate` from Prometheus | a Prometheus that keeps the histograms and still holds the window; Go and a checkout; a heavy range query per input | what Prometheus does not hold: rollouts, the engine arguments at the time, Pod roles without kube-state-metrics. Each is a `caveat:` line |
| the controller's own recorder | a strategic-merge patch, a busybox sidecar, `kubectl cp` | a supported way to turn it on, keep it, and get it out |

The controller's recorder already writes exactly what the planner reads, with
none of the caveats: it records the inputs the analyzers read and the results
they return, as the controller saw them. But it is a test aid:

- it is turned on with a Deployment patch (`config/components/learner-recorder`),
  which replaces the Pod;
- one file per controller start, and the file stops at its cap (256 MiB),
  keeping its head: a busy fleet fills it in hours, and nothing is recorded
  after that until the next restart;
- getting it out needs `kubectl cp`, so a busybox sidecar (the manager image
  has no `tar`) and `pods/exec`;
- nothing says what it costs, what it holds, or which planner can read it.

This proposal makes it a supported operator option. It complements the
Prometheus capture and does not replace it: capture answers about a past
window you did not record; the recorder answers about the days since you
turned it on.

## What the operator runs

```bash
# On: at install, or later with a live settings key.
smt install -n team-a --prometheus-url https://prometheus.example:9090 --record
kubectl patch configmap scaling-manager-settings -n team-a --type merge \
  -p '{"data":{"plannerRecording":"on"}}'

# Days later: copy the finished files out, and plan from them.
smt recording fetch -n team-a --out ./recordings
capacity-planner serve --recordings ./recordings

# Off, within a cycle.
kubectl patch configmap scaling-manager-settings -n team-a --type merge \
  -p '{"data":{"plannerRecording":"off"}}'
```

`capacity-planner` is the binary of the same release tag as the controller
([Format and version](#format-and-version)).

## Decisions

### The switch: a live settings key

| key | values | unset means | applies |
| --- | --- | --- | --- |
| `plannerRecording` | `on`, `off` | `off` | live |
| `plannerRecordingVolume` | `emptyDir`, `pvc` | `emptyDir` | install (read by `smt install`) |
| `plannerRecordingSize` | a quantity, such as `20Gi` | `1Gi` (emptyDir), `10Gi` (pvc) | install |
| `plannerRecordingSegment` | a duration, `10m` to `24h` | `24h` | live, from the next segment |

`plannerRecording` is live both ways. Off closes the open file within a cycle.
On opens a new one within a cycle: possible because each file now starts with
a checkpoint of what the analyzers have learned
([Files and rotation](#files-and-rotation)), so it need not start at a
controller start. `smt install --record` is `--set plannerRecording=on`.

The existing flags and `LEARNER_RECORDING_*` variables stay, for tests. When
both are set, the flags win and the key is reported as overridden in
`settingsSources`.

### A settings key cannot add a volume

The controller cannot change its own Pod. So:

- **The volume is always mounted.** The release Deployment mounts an
  `emptyDir` with `sizeLimit: 1Gi` at `/var/lib/scaling-manager/recordings`,
  recording or not. An `emptyDir` reserves nothing: while recording is off it
  holds nothing and costs nothing. The key only enables writing. Turning
  recording on or off never replaces the Pod, and never loses files.
- **A PVC is an install choice.** `smt install` reads
  `plannerRecordingVolume` and `plannerRecordingSize` from the settings and
  renders the Deployment with a PersistentVolumeClaim instead of the
  `emptyDir`. Changing either is an `smt install` re-run (an upgrade), which
  replaces the Pod; `smt` says so and offers a fetch first. The install also
  sets `LLMSM_RECORDING_VOLUME` on the container, so the controller can report
  a key that the Deployment does not match (`plannerRecordingVolume says pvc,
  the Deployment mounts an emptyDir: re-run smt install`).

Rejected: install patching the Deployment on every switch. It replaces the
Pod on each on/off, which with an `emptyDir` deletes what was recorded, and it
needs the install's rights for what should be a one-line patch.

The `config/components/learner-recorder*` components and the busybox sidecar
are removed: the volume is in the base, and files leave over HTTP, not
`kubectl cp`.

### Storage

| volume | survives | holds by default | for |
| --- | --- | --- | --- |
| `emptyDir`, 1Gi limit | a container restart (crash, OOM kill) | about a week of one model, compressed (to be measured) | a few days of history; the default |
| PVC, ReadWriteOnce, `plannerRecordingSize` | a Pod replacement: upgrade, rollout restart, drain | `plannerRecordingSize` | weeks of history |

An `emptyDir` is lost with the Pod: upgrade, `rollout restart`, eviction,
drain, uninstall. `smt install` (upgrade) fetches first when recording is on
and an `emptyDir` is in use, unless `--no-fetch`.

**PVC and replicas.** A ReadWriteOnce volume attaches to one node. The
controller Deployment runs one replica by default, and a rolling update of one
replica with an RWO volume deadlocks when the new Pod lands on another node:
it waits for the volume, and the old Pod waits for the new one to be Ready.
So with `pvc` the install sets `strategy: Recreate` and refuses more than one
replica. A cluster that runs several controller replicas for availability
uses the `emptyDir`; ReadWriteMany is not offered (one directory written by
two Pods across a failover, and a ceiling counted over both, is more than this
needs).

### Files and rotation

Today a file starts at a controller start and stops at its cap. For days of
history that fails twice: the file stops after hours, and a later file cannot
replay without the history before it. The change:

- **Segments.** A recording is a run of segments. A segment closes when it
  reaches its cap (`--learner-recording-cap-bytes`, 256 MiB) or its age
  (`plannerRecordingSegment`, 24h), whichever comes first; the next segment
  opens on the next cycle. A closed segment is gzipped and fsynced, then the
  raw file is removed.
- **Each segment starts with a checkpoint.** Its first lines are the start
  line (as today, plus the segment number and the release tag) and one
  `Checkpoint` event: the learned state of every analyzer and the capacity
  store, in the form each package already writes for the
  [learned-state snapshot](learned-state-snapshot.md) and for a restore event.
  Replay restores it and goes on. A segment therefore replays alone, and a
  planner question about any cycle in it is answered with all the history
  learned before it. This is the one change to replay
  (`internal/engines/analyzers/learnreplay`), and its test is exact: replaying
  segments 1 and 2 joined, and segment 2 alone, gives byte-identical analyzer
  output for every cycle of segment 2. An analyzer whose state the checkpoint
  cannot carry yet makes the segment's start carry a caveat naming it; the
  first phase does not ship until saturation, slo, throughput and the store
  are all carried.
- **Rotation keeps the newest.** The directory ceiling
  (`--learner-recording-dir-bytes`) counts every segment, compressed or open.
  When the next segment would not fit, the oldest closed segments go first,
  as `makeRoom` does today; the open one is never deleted. With segments,
  "oldest" is always the oldest days.
- **Names.** `learnrec-<run start>-<pod suffix>-s<NNNN>.jsonl` while open,
  `.jsonl.gz` once closed. Run start is today's fixed-width UTC time, so names
  still sort oldest first; the pod suffix (the generated part of the Pod name)
  tells two writers apart after a failover; `NNNN` orders segments of one run.

**A window across files.** The planner reads one file. Learned state crosses a
segment boundary through the checkpoint, so answers do. The observed points the
page plots do not: a graph shows only the chosen segment's cycles. For a
window across segments, `capacity-planner join SEG... -o FILE` joins
consecutive segments of one run (it drops each later segment's start and
checkpoint and renumbers), and `serve` reads the result unchanged. It refuses
segments of two runs: across a restart the learned state restarted too, and a
file that hides that would mislead.

### Restart and leader election

Only the leader writes: the recorder lives in the steady-state engine's
optimize loop, which runs on the leader alone.

| event | what happens to recording |
| --- | --- |
| container restart, same Pod | the `emptyDir` stays. The new process finishes the open segment left by the old one (keeps its whole lines, gzips it) and starts a new run |
| graceful leader change (`LeaderElectionReleaseOnCancel`) | the old leader closes and gzips its segment as it stops. The new leader, in another Pod, starts a new run in its own volume |
| leader crash | the old segment has a torn last line; replay drops it (`TornTail`), and the next start finishes the file |
| leadership regained in the same process | today: not recorded. With checkpoints: a new run starting with a checkpoint |
| Pod replaced, `emptyDir` | files lost; fetch first |

With several replicas, each Pod's volume holds the runs it wrote while it led.
`smt recording fetch` reads every controller Pod, so a failover loses nothing
that is still on a running Pod.

### Getting files out: `smt recording fetch`

```bash
smt recording fetch -n NS --out ./recordings [--since 72h] [--include-open] [--pseudonymise]
```

It copies finished segments from every controller Pod, checks each against
the size and SHA-256 the controller lists, skips files already present with
the same digest, and prints, per file, the run, the segment, the time range
and the release that wrote it. `--include-open` also copies the open segment
up to its last whole line. `--pseudonymise` runs the existing export
(`learnreplay` Export, as `learnergolden export` does) on each file, for a file
that leaves the organisation: names are replaced, figures are kept, and the
planner answers from it the same.

How files leave the Pod:

| way | rights the caller needs | what those rights also give | verdict |
| --- | --- | --- | --- |
| `kubectl cp` | `pods/exec` | a shell in the controller's Pod, with its ServiceAccount token: every right the controller has, in every namespace it manages. Also needs `tar` in the image (a sidecar) | rejected |
| `pods/log` | `pods/log` get | nothing more | cannot carry a file |
| API-server pod proxy (`pods/proxy`) | `pods/proxy` get | any port of the Pod. The API server does not pass the caller's token on, so the controller cannot tell who asks | rejected |
| **an endpoint on the controller, reached by port-forward** | `pods/portforward` create, and the endpoint's own check | a TCP connection to the Pod's ports, nothing on its filesystem or token | **chosen** |

The controller serves `GET /recordings/` (a list: name, size, SHA-256, open or
closed) and `GET /recordings/<name>` on `127.0.0.1:8444`. Loopback only: no
Service, no NetworkPolicy change, unreachable from the Pod network; the only
way in is a port-forward. Every request carries the caller's bearer token,
checked with TokenReview, and a SubjectAccessReview for **`get pods/log` on
that controller Pod**. The rule is "who may read the controller's logs may read
its recordings": the logs already carry the same names (models, namespaces,
Pods) and the same decisions. The controller already has the review rights
(the metrics-auth ClusterRole). The stock `admin` role in the controller's
namespace grants everything fetch needs: list pods, `pods/portforward`,
`pods/log`. No ClusterRole is needed: a non-resource URL would need one, bound
by a cluster admin. The tools image already requires a token kubeconfig, so a
bearer token is always there.

The endpoint serves only names it lists, opens files read-only, refuses paths
with a separator, and serves an open segment only up to its last whole line.
It is plain HTTP inside the port-forward, which the API server and kubelet
already carry over TLS.

### Format and version

The decoder is strict: a field it does not know is an error, so a recording
read with the wrong schema fails rather than replays half an input. Fields are
added, never renamed, but a newer controller can write a field an older planner
does not know.

**Rule: read a recording with the `capacity-planner` of the same release
tag.** The start line gains `Release` (the tag; `Commit` is already there).
`serve` and every answer command check it: the same tag, or a dev build, read
it; any other is refused with both tags and the line to fetch the right
binary, unless `--allow-version-skew`, which tries and prints `UNVALIDATED`.
`fetch` prints the tag of each file, so a mix is seen before it is served. A
release that changes the format says so in its notes; a release that cannot
read the previous one's files is a breaking change.

### What a file holds

Only what the controller already reads or decides. No prompt, no completion,
no request header or body: the controller never sees them. The full list of
string fields is the export's classification table
(`learnreplay/classify.go`): a field not in it stops the export, so the table
is complete by construction, and a test fails when a recorded field has no
class.

| kind | examples | where from |
| --- | --- | --- |
| figures | KV-cache use, queue lengths, token rates, latency histograms, replica counts, start times, the analyzers' fits and decisions | Prometheus, the controller's own computation |
| names | namespaces, model names and paths, variant and Pod names, node-selector labels, priority classes, limiter names | Kubernetes objects the controller reads |
| workload facts | container image and revision, parsed engine parameters (KV dtype, parallelism, speculative config) | the model Deployments |
| configuration | policy values, analyzer settings, the controller's commit and release | the policy and settings |
| text | error messages, which can name hosts or URLs (the Prometheus URL) | the controller's own errors |

Not in a file: Secrets, tokens, environment values, raw container arguments
(only the parsed parameters the capacity store keeps), PromQL text in
start/config lines (redacted by the export only; see the open questions).

The review this proposal asks for before phase 1 ships: one test that runs a
recording with planted values (an API key in the engine arguments, a token in
an env var, a URL with credentials in an error) and asserts none reaches the
file. The reference page lists the table above, and the security model page
says who may read files and how.

### Cost and the performance budget

What recording costs, per model, is stated in the docs as measured numbers,
not estimates. Today's only figure is an estimate in `learnrec/files.go`:
about 5.6 KB per replica per cycle per analyzer, uncompressed. For one model
with 8 replicas, saturation only and a 15 s cycle, that is about 260 MB a day
raw; gzip of JSON lines of figures is expected near 10x, so about a week in
the 1Gi default. That claim is what the measurement must confirm or correct,
and the defaults change with it.

| cost | how it is measured |
| --- | --- |
| controller CPU | `process_cpu_seconds_total` of the controller, recording on vs off, same load |
| cycle time | `llmsm_optimization_duration_seconds`, p50 and p99, on vs off, and the new `llmsm_planner_recording_cycle_seconds` |
| memory | `go_memstats_heap_inuse_bytes`, on vs off |
| disk per day per model | `llmsm_planner_recording_bytes_written_total` (raw) and the closed segments' sizes (gzipped), per day, divided by models |
| Prometheus | the record-only queries recording turns on (`QueryTypeLearnerRecording`: router TPOT and flow-control histograms per Pod), counted per cycle |
| gzip on close | wall time and CPU of compressing one 256 MiB segment |
| planner | `serve` time and memory to replay one 24h segment to its last cycle |

Runs: the kind e2e with the simulator at 1, 8 and 32 replicas, each with
saturation alone and with saturation, slo and throughput, two hours on and two
off; and one 24 h run on a GPU cluster with one real model. The results go in
a table in the reference page, with the date and release.

**Budget.** Recording may add at most 5% to the cycle's p99 duration and 50m
CPU on average, for one model at 8 replicas (to be confirmed by the
measurement, then enforced).

**Hard off-switch.** Three, from fastest to strongest:

1. `plannerRecording: "off"`: within a cycle, closes the segment and stops
   the record-only queries.
2. The recorder stops itself, and never blocks a cycle, when a write fails,
   the volume is full, or recording took more than the budget for 10 cycles
   in a row. The state is `stopped` with a reason; setting the key `off` and
   `on` again restarts it.
3. `LLMSM_PLANNER_RECORDING=disabled` on the Deployment: wins over the key, so
   a cluster admin can forbid recording where a namespace owner can edit the
   settings.

### Metrics and alerts

| metric | type | meaning |
| --- | --- | --- |
| `llmsm_planner_recording_state` | gauge, label `state` (`off`, `on`, `stopped`) | 1 for the current state |
| `llmsm_planner_recording_stop_reason` | gauge, label `reason` (`write_error`, `volume_full`, `over_budget`, `disabled`) | 1 for why it stopped |
| `llmsm_planner_recording_bytes_written_total` | counter | raw bytes written |
| `llmsm_planner_recording_dir_bytes` | gauge | bytes held, all segments |
| `llmsm_planner_recording_oldest_timestamp_seconds` | gauge | start of the oldest segment held: how many days you can plan from |
| `llmsm_planner_recording_events_dropped_total` | counter, label `reason` | events not written |
| `llmsm_planner_recording_segments_deleted_total` | counter | segments rotated out |
| `llmsm_planner_recording_cycle_seconds` | histogram | time recording added to a cycle |

The controller status ConfigMap gains `plannerRecording` (`off`, `on`,
`stopped: <reason>`) and `plannerRecordingOldest`.

Alerts, in `config/components/prometheus-alerts`:

- `LLMSMPlannerRecordingStopped`: state `stopped` for 15 minutes.
- `LLMSMPlannerRecordingDropping`: events dropped for 15 minutes while on.

### Test gate

Phase 1 merges when the kind e2e does this, in CI, under a new label in the
existing full suite:

1. Install with `--record`; assert the status says `on` and a segment exists.
2. Drive the simulator through a load ramp for 20 minutes, with
   `plannerRecordingSegment: 10m`, so at least one segment closes and one
   rotates out under a small dir ceiling.
3. `smt recording fetch` as a ServiceAccount bound to the stock `admin`
   role only; assert it got every closed segment and their digests match.
   As a ServiceAccount without `pods/log`, assert 403.
4. `capacity-planner serve --recordings` on the fetched directory; ask
   `replicas-to-slo` on the last segment and assert an answer, not a refusal.
5. Join two segments; assert the replay of the joined file and of the later
   segment alone agree on every cycle of the later one.
6. Patch the key `off`; assert the segment closes within two cycles and the
   record-only queries stop.
7. Restart the controller container; assert the torn segment is finished and
   fetched.

Unit tests: the checkpoint equivalence above for each analyzer; rotation
order; the version check; the planted-secrets test; the endpoint's path
checks and SubjectAccessReview attributes.

## Phases

1. Segments, checkpoints, gzip on close, the version field, the replay
   change, `capacity-planner join`. The test aid uses them too.
2. The settings keys, the always-mounted `emptyDir`, the loopback endpoint,
   `smt recording fetch`, metrics, alerts, status. Remove the kustomize
   components and the sidecar. The e2e gate.
3. The cost measurement; set the defaults from it; publish the table.
4. PVC: `plannerRecordingVolume`, `Recreate`, the replica check, upgrade
   fetch.

## Docs to update

| page | change |
| --- | --- |
| [Record a window for the capacity planner](../guides/record-for-capacity-planner/) | a new first path: from the controller. When to take it, and when Prometheus capture is still the answer |
| [Plan a P/D fleet](../guides/capacity-planner/) | serving fetched files; the same-tag rule |
| [Configuration](../reference/configuration.md) | the four keys |
| [Metrics](../reference/metrics.md), [Monitoring](../reference/monitoring.md) | the new metrics and alerts |
| [Security model](../reference/security-model.md) | what a file holds; who may fetch it; the endpoint |
| [Upgrade and uninstall](../reference/upgrade-and-uninstall.md) | fetch before an upgrade with an `emptyDir` |
| [Troubleshooting](../reference/troubleshooting.md) | `stopped` and its reasons; a refused version |
| [Testing](../developer-guide/testing.md), "Learner Recordings" | the key replaces the patch and `kubectl cp` |
| [Capacity planner serve](../developer-guide/capacity-planner-serve.md) | the version check, `join` |
| a new reference page, planner recordings | file format, segment layout, the content table, the measured cost |

## Out of scope

- Shipping recordings anywhere off the cluster (object storage, a remote
  sink). Fetch is pull only.
- Recording from the scale-from-zero engine or the warm pool.
- Reading recordings in the controller, or serving the planner from it.
- A recording format readable across releases without the same-tag rule.
- ReadWriteMany volumes and several writers on one volume.
- Retention by age alone; the ceiling decides, the segment age only closes
  files.

## Open questions

1. Should the tools image carry the `capacity-planner` binary, so `smt
   recording fetch` and `serve` come from one image of one tag? Today there is
   no released planner binary; the same-tag rule then means "build from the
   release checkout".
2. Is 24h the right default segment? A smaller one bounds the planner's replay
   time and what a crash loses, but adds a checkpoint per segment. The planner
   measurement in phase 3 decides unless you prefer to fix it now.
3. The `get pods/log` rule lets anyone who reads the controller's logs read
   every namespace it manages in a cluster-scoped install. Same as the logs
   today; acceptable, or should cluster scope require a dedicated grant?
4. PromQL text in start and config lines is kept in a raw file (the export
   redacts it). Keep it, since it helps debugging, or drop it at the source?
