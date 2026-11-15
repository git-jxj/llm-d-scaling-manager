# Record for the capacity planner from the controller

**Status:** proposal, 2026-11-15; no open questions
([decided on review](#decided-on-review-2026-11-15)). Nothing here is built.

**Audience:** contributors. Operators record today from Prometheus:
[Record a window for the capacity planner](../guides/record-for-capacity-planner/).

**Builds on:** the learner recorder (`internal/signals/learnrec`,
[the learner proposal](analyzer-learner.md), sections 2.4 and 2.5), the
[capacity planner](capacity-planner.md), and the
[settings ConfigMap](../reference/configuration.md#the-settings-configmap).
Phase 2 also depends on `smt install` from
[Install in two commands](two-command-install.md), which is being built and
is not released: `smt` has no `install` or `recording` command yet.

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
# On. Either at install:
smt install -n team-a --prometheus-url https://prometheus.example:9090 --record
# or, on an installed controller, with the live settings key:
kubectl patch configmap scaling-manager-settings -n team-a --type merge \
  -p '{"data":{"plannerRecording":"on"}}'

# Days later: copy the finished files out, and plan from them.
smt recording fetch -n team-a --out ./recordings
capacity-planner serve --recordings ./recordings

# Off, by the end of the next cycle.
kubectl patch configmap scaling-manager-settings -n team-a --type merge \
  -p '{"data":{"plannerRecording":"off"}}'
```

`smt` fetches; it does not plan. `capacity-planner` is the planner image or
binary of the same release tag as the controller
([Format and version](#format-and-version)); the tools image does not carry
it.

## Decisions

### The switch: a live settings key

| key | values | unset means | applies |
| --- | --- | --- | --- |
| `plannerRecording` | `on`, `off` | `off` | live |
| `plannerRecordingVolume` | `emptyDir`, `pvc` | `emptyDir` | install (read by `smt install`) |
| `plannerRecordingSize` | a quantity, such as `20Gi` | `1Gi` (emptyDir), `10Gi` (pvc) | install |
| `plannerRecordingPart` | a duration, `10m` to `24h` | `24h` | live, from the next part |

`plannerRecording` is live both ways. Off closes the open part by the end of
the next cycle. On opens a new run by the end of the next cycle. A run opened
after the controller started has no record of what the analyzers learned
before it, so it is read in cold mode
([Reading a run](#reading-a-run)) and says so. For an exact replay from the
first cycle, turn it on and restart the controller.

`smt install --record` is `--set plannerRecording=on`.

### Where each value comes from

| source | what it sets | precedence |
| --- | --- | --- |
| `LLMSM_PLANNER_RECORDING=disabled` on the Deployment | nothing records, whatever else says | 1, highest. For a cluster admin who must forbid recording where a namespace owner can edit the settings |
| `--learner-recording-dir`, `--learner-recording-cap-bytes`, `--learner-recording-dir-bytes` flags, or their `LEARNER_RECORDING_*` variables | the test aid: records to that directory with those caps | 2. When the directory is set, the key and the `plannerRecording*` sizes are ignored, and `settingsSources` reports the key as overridden by `flag` or `env`. A flag and its variable that disagree: the flag wins, because the variable is only the flag's default (as in `cmd/main.go` today) |
| `plannerRecording` settings key | on or off | 3 |
| `LLMSM_PLANNER_RECORDING_VOLUME`, `LLMSM_PLANNER_RECORDING_BYTES` on the Deployment | the volume kind and size, written by `smt install` from `plannerRecordingVolume` and `plannerRecordingSize`; the ceiling derives from the size ([Ceiling and caps](#ceiling-and-caps)) | set by install only. When they disagree with the keys, the status says so and names `smt install` |
| none | off | 4 |

New variables use the `LLMSM_` prefix. The test aid's `LEARNER_RECORDING_*`
keep their names and stay out of the operator docs.

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
  `emptyDir`, and the `LLMSM_PLANNER_RECORDING_*` variables to match. Changing
  either is an `smt install` re-run (an upgrade), which replaces the Pod;
  `smt` says so and offers a fetch first.

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

**A full `emptyDir` evicts the Pod.** A disk-backed `emptyDir` past its
`sizeLimit` is not a write error: the kubelet evicts the Pod, every file goes
with it, and the controller restarts (the reason `learnrec` refuses `/tmp`
today). So the recorder never relies on the volume filling: it counts its own
bytes and keeps them well under the limit ([Ceiling and caps](#ceiling-and-caps)).
On an `emptyDir` there is no "volume full" stop; on a PVC, a write error from
a full volume stops recording (`write_error`) and nothing is evicted.

**PVC: what it costs.** Say it plainly in the docs:

- A ReadWriteOnce volume attaches to one node. A rolling update of one
  replica deadlocks when the new Pod lands on another node: it waits for the
  volume, the old Pod waits for the new one to be Ready. So with `pvc` the
  install sets `strategy: Recreate` and refuses more than one replica.
- With `Recreate`, every upgrade and every `rollout restart` stops the
  controller and the KEDA external scaler it serves before the new Pod starts:
  no scaling decisions, and KEDA's calls fail, until it is Ready.
- An RWO volume on zonal storage pins the controller to that zone.
- After a node failure, the volume may not detach for minutes (until the node
  is declared lost and the attachment times out); the controller Pod cannot
  start elsewhere until it does.

A cluster that runs several controller replicas for availability uses the
`emptyDir`. ReadWriteMany is not offered.

### Files and rotation

Today a file starts at a controller start and stops at its cap. For days of
history that fails: the file stops after hours.

**Considered: a checkpoint at the head of every file.** Each file would start
with the full learned state of every learner, so it replays alone and old
files can be deleted freely. Rejected for now, because no learner can write
that state today:

| learner | today | a checkpoint would have to carry at least |
| --- | --- | --- |
| saturation | `ExportLearnedState` / `RestoreLearnedState` (`saturation/recording.go`), for the learned-state snapshot. A cut-down copy: k2 and throughput averages, ITL baselines and slopes, start estimates | the k2 and throughput windows themselves, the ITL windows, episodes and bucket state, lower bounds, hold and borrow timers, accelerator memos, forget marks, the window arguments |
| slo | none | its per-model judging state, slope holds and timers, the TTFT inputs' windows |
| throughput | none | its arrival-rate windows and demand state |
| `capacity.Store` | none | every record |
| settle tracker, composite holds | none | their open holds and grace timers |

A replay from such a checkpoint is exact only if every field is carried, and
the proof is a byte-for-byte replay test per learner. That is a project of its
own, larger than the rest of this proposal.

**Chosen: a run split into parts, read in order.**

- **A run** is what one leader writes for one model namespace from when
  recording starts to when it stops: one stream, one start line, one sequence
  of event numbers, as today.
- **Parts.** A run is written as parts. A part closes when it reaches its cap
  or its age (`plannerRecordingPart`, 24 h to start with), whichever comes
  first; the next part continues the same stream on the next event. Parts
  are never split mid-line.
  - **Part 1** begins with the run's start line (event 1, kind `start`), as
    a recording does today. The same line, byte for byte, is also written to
    `...-start.json`, kept until the run's last part is deleted.
  - **Every part after the first** begins with one part line:
    `{"Part":{"Run":"<run id>","N":2,"FirstSeq":48113}}`. It is not an event:
    no `Seq`, no `Kind`, so it takes no event number.
  - **The run** is its parts put end to end with their part lines removed:
    exactly the lines the recorder wrote, events numbered from 1 with no gap,
    which is what `learnrec.Read` requires. The reader strips part lines, and
    checks each one's `FirstSeq` against the next event's number before it
    does.
- **Cold runs are marked.** The start line gains `Cold` (true when recording
  began after the controller started: the analyzers already held state the
  run does not have) and `ControllerStart`. A run whose part 1 was rotated out
  is cold too; the reader sees that from the gap ([Reading a run](#reading-a-run)).
- **Gzip off the cycle.** A closed part is fsynced and put on a queue. One
  background goroutine gzips one part at a time, oldest first, at low
  priority, never on the cycle's goroutine.
  The SHA-256 is computed once, when a file is final (gzipped, or closed raw
  and not yet gzipped), and kept beside it. At shutdown the controller only
  closes and fsyncs the open part: the manager has 25 s to stop
  (`managerGracefulShutdownTimeout`) on a 500m CPU limit, too little to gzip
  256 MiB. A raw closed part left behind is gzipped by the next start, in the
  background.
- **Rotation keeps the newest.** When the next part would not fit under the
  ceiling, the oldest closed parts go first, across all runs; the open parts
  are never deleted. Each run's start line is also kept in a small file of its
  own, deleted only with the run's last part.
- **One run per model namespace.** A cluster-scoped controller writes one
  run per model namespace ([why](#access-per-model-namespace)). A
  namespace-scoped install has one namespace, so one run.
- **Names.** `learnrec-<model namespace>-<run start>-<pod suffix>-p<NNNN>.jsonl`
  while open, `.jsonl.gz` once closed, and `...-start.json` for the start
  line. Run start is today's fixed-width UTC time; the pod suffix (the
  generated part of the Pod name) tells two writers apart after a failover;
  `NNNN` orders parts. Rotation orders by the run start and part parsed from
  the name, not by the name, so the oldest days go first across all
  namespaces: one ceiling for the volume, and a busy namespace can push out a
  quiet one's history (the reference page says so).

#### Reading a run

`smt recording fetch` writes **one file per run**, always gzip:

- When part 1 is held: the run's parts in order. When it was rotated out:
  the `...-start.json` line first, then the parts held.
- **Encoding.** The listing reports each file's encoding (`gzip` for a
  closed, compressed part; `identity` for an open part or one still queued
  for gzip). fetch gzips every `identity` file on the caller's machine before
  it joins anything, so the output is only gzip members, never plain lines
  next to gzip members. Gzip members put end to end are one valid gzip file,
  and Go's reader reads them all (`compress/gzip`, multistream by default),
  so joining is concatenation, not parsing.
- **Where files go.** Downloaded parts are kept in `<out>/.parts/<pod>/`,
  under their own names; a part already there with the listed digest is not
  downloaded again. The output, one file per run, is
  `<out>/<namespace>-<run start>-<pod suffix>.jsonl.gz`, rebuilt from
  `.parts` on every fetch. `serve` lists neither directories nor names
  starting with a dot (`cmd/capacity-planner/serve_price.go`), so the cache
  is invisible to it.
- **`--since` never cuts a run.** It selects the runs with any part newer
  than the time, and fetches each as held. A gap is only ever left by
  rotation, at the head.

`capacity-planner serve --recordings` reads the output. What it does with a
run:

| run as fetched | how the planner reads it |
| --- | --- |
| whole: part 1 held, start line not `Cold` | exactly as today (Fidelity): every answer from all the run's history |
| cold: start line `Cold` (recording turned on after the controller started), or a gap right after the start line (part 1 rotated out) | **cold mode**: the planner plays the kept inputs through fresh analyzers from the first kept cycle, as `generate` plays a capture |
| a gap anywhere else: a missing middle part | refused, naming the missing event numbers: a hole in the middle of a run does not replay, and cold mode would hide it |

**What a cold answer carries.** It is flagged, not refused: a caveat line in
the text output (`cold: learned from <first kept time>, not from the
controller's start`), the same in `caveats` with `-o json`, and on the page.
Not refused, because the Prometheus capture is cold in the same way and is
served with the same kind of caveat; refusing the first hours of a recording
would refuse what a capture of the same window gets. Answers from late in a
cold file are as good as a capture of that window; those from its first hours
are from a cold start, and the caveat says so.

The reader changes in `internal/engines/analyzers/learnreplay` and
`learnrec.Read` are: strip part lines, checking their `FirstSeq`; read `Cold`;
accept one gap, only directly after the start line, and only into cold mode;
play cold from recorded inputs instead of comparing recorded outputs.

A window across a restart is two runs, two files. They are never joined: the
learned state restarted too, and one file that hid it would mislead.

#### Ceiling and caps

The ceiling is what all files on the volume may hold together; the cap is
what one part may hold before it closes.

- **Ceiling** = 80% of `LLMSM_PLANNER_RECORDING_BYTES` (from
  `plannerRecordingSize`). The 20% left covers what is not counted exactly:
  a gzip output in progress is counted at its raw part's size, but the
  filesystem's own overhead is not. 1Gi gives a ceiling of about 819 MiB.
- **Cap per part** = the smaller of 256 MiB and ceiling / (N + 2), where N is
  the number of runs open (model namespaces being recorded). The 2 is room for
  one gzip in progress and one closed part kept. This keeps `Check`'s rule
  (ceiling at least twice the cap) for any N. One namespace on 1Gi: about
  256 MiB per part; ten namespaces: about 68 MiB.
- When a namespace starts recording, N grows; parts already open keep their
  cap, and if the open parts together would pass the ceiling, the largest
  closes early.
- Below the 64 KiB minimum per part (`MinCapBytes`), a further namespace is
  not recorded; the status and a metric name it.

### Restart and leader election

Only the leader writes: the recorder lives in the steady-state engine's
optimize loop, which runs on the leader alone. Losing the lease ends the
manager, and the process with it, so a process never leads twice.

| event | what happens to recording |
| --- | --- |
| container restart, same Pod | the `emptyDir` stays. The new process closes what the old one left open (keeps its whole lines) and starts new runs |
| graceful leader change (`LeaderElectionReleaseOnCancel`) | the old leader closes and fsyncs its open parts as it stops. The new leader, in another Pod, starts new runs in its own volume |
| leader crash | the open part has a torn last line; the reader drops it (`TornTail`), and the next start closes the file |
| Pod replaced, `emptyDir` | files lost; fetch first |

With several replicas, each Pod's volume holds the runs it wrote while it led.
`smt recording fetch` reads every controller Pod, so a failover loses nothing
that is still on a running Pod.

### Getting files out: `smt recording fetch`

```bash
smt recording fetch -n NS --out ./recordings [--since 72h] [--include-open] [--pseudonymise]
```

It copies closed parts from every controller Pod, checks each against the size
and SHA-256 the controller lists, skips parts already present with the same
digest, writes one file per run ([Reading a run](#reading-a-run)), and prints,
per run, the namespace, the time range, whether it is whole or cold, and the
release that wrote it. `--include-open` also copies the open part up to its
last whole line. `--pseudonymise` runs the existing export (`learnreplay`
Export, as `learnergolden export` does) on each file, for a file that leaves
the organisation: names are replaced, figures are kept.

How files leave the Pod:

| way | rights the caller needs | what those rights also give | verdict |
| --- | --- | --- | --- |
| `kubectl cp` | `pods/exec` | a shell in the controller's Pod, with its ServiceAccount token: every right the controller has, in every namespace it manages. Also needs `tar` in the image (a sidecar) | rejected |
| `pods/log` | `pods/log` get | nothing more | cannot carry a file |
| API-server pod proxy (`pods/proxy`) | `pods/proxy` get | any port of the Pod. The API server does not pass the caller's token on, so the controller cannot tell who asks | rejected |
| **an endpoint on the controller, reached by port-forward** | `pods/portforward` create, and the endpoint's own check | a TCP connection to the Pod's ports, nothing on its filesystem or token | **chosen** |

The controller serves `GET /recordings/` (a list: name, namespace, run,
part, size, SHA-256, open or closed, encoding `gzip` or `identity`) and
`GET /recordings/<name>` on `127.0.0.1:8444`.

- **Loopback works through a port-forward.** The kubelet's port-forward opens
  the connection from inside the Pod's network namespace, to its loopback
  address. So a listener on `127.0.0.1` is reachable through a port-forward
  and through nothing else: no Service, no NetworkPolicy change, nothing on
  the Pod network.
- **Every replica serves it**, leader or not, so files a former leader wrote
  can be fetched from its Pod.
- **Authentication.** Every request carries a bearer token, checked with
  TokenReview, then SubjectAccessReviews
  ([Access per model namespace](#access-per-model-namespace)). The controller
  has these review rights through `metrics-auth-role`, the same ClusterRole
  the metrics filter uses. An install that adds the
  `config/components/unauthenticated-metrics` component deletes that role;
  there the endpoint refuses every request, and fetch says why ("the
  controller cannot review tokens: this install has no metrics-auth-role").
- **Tokens.** The controller keeps review results in memory for a minute,
  keyed by the SHA-256 of the token, never the token; it never logs a token
  or a header. fetch prefers a short-lived token with its own audience: when
  the caller is a ServiceAccount, it asks TokenRequest for a 10-minute token
  for itself with audience `scaling-manager-recordings`, which the API
  server will not accept for anything else; it falls back to the
  kubeconfig's own token only when TokenRequest is refused.
- **Audiences.** The endpoint's TokenReview sets `spec.audiences` to two
  values: `scaling-manager-recordings`, and the API server's own audience,
  read from the `aud` claim of the controller's own projected ServiceAccount
  token (the API server's `--api-audiences` default, which the controller has
  no other way to read). The first is the preferred token: useless anywhere
  but this endpoint, so a leaked one leaks nothing else. The second is
  accepted only so the fallback works: a kubeconfig token (an OIDC or
  long-lived ServiceAccount token) is minted for the API server. The review's
  `status.audiences` says which matched; the controller logs which (never the
  token), and `LLMSM_PLANNER_RECORDING_STRICT_AUDIENCE=true` drops the second,
  for a cluster that wants no general token sent to a Pod. A
  kubeconfig with a client certificate and no token (kind's default) has no
  bearer token to send: fetch stops and says to use a token kubeconfig, which
  the tools image already requires. The e2e uses ServiceAccount tokens.
- The endpoint serves only names it lists, opens files read-only, refuses
  paths with a separator, and serves an open part only up to its last whole
  line. It is plain HTTP inside the port-forward, which the API server and
  kubelet already carry over TLS.

The rights fetch needs, all in the stock `admin` role of a namespace: in the
controller's namespace, `list` and `get` on `pods` (to find the replicas; a
port-forward also needs `get pods`), `create` on `pods/portforward`, and
`get` on `pods/log`; in each model namespace it fetches, `get` on `pods/log`.
When the caller is a ServiceAccount, `create` on its own
`serviceaccounts/token` for the short-lived token (optional: without it,
the fallback). No ClusterRole: a non-resource
URL would need one, bound by a cluster admin.

#### Access per model namespace

`get pods/log` on the controller Pod is necessary, not sufficient. The
endpoint lists and serves a file only when the caller also has `get pods/log`
in that file's model namespace (one SubjectAccessReview per namespace, cached
as above). A namespace-scoped install checks the one namespace; the stock
`admin` role there grants it. In a cluster-scoped install a namespace owner
fetches their own namespaces' files and no one else's.

Two ways to make that possible:

| way | how | verdict |
| --- | --- | --- |
| filter on serve | one file for all namespaces; the endpoint drops other namespaces' events as it serves | rejected. Every fetch parses and re-encodes up to 256 MiB per part and renumbers it; the result is not the file the controller wrote, so its digest is computed per caller and cannot be checked against a listing; and every event that spans namespaces (the capacity store, quotas, limiters, utilization share) needs a rule for who may see it, written once per event kind and easy to get wrong |
| **split on write** | one run per model namespace; the file is the unit of access | **chosen**. The check is one SAR on a name; files are served byte for byte, with stable digests; a leak needs a writer bug, not a filter bug, and the planted-names test below catches it |

The cost of the split is a few events written more than once: what spans
namespaces is written into each namespace's run that its analyzers read it
in. In those copies, another namespace's names are replaced by the export's
pseudonyms, keyed per controller, so a namespace's file never names another
namespace, its models or its Pods; the figures (a shared quota's use, a
borrowed share) stay, since the planner needs them. The test gate checks a
file of namespace A for every name of namespace B.

### Format and version

The decoder is strict: a field it does not know is an error, so a recording
read with the wrong schema fails rather than replays half an input. Fields are
added, never renamed, but a newer controller can write a field an older planner
does not know.

**Rule: read a recording with the `capacity-planner` of the same release
tag.** The start line gains `Release` (the tag; `Commit` is already there).
`serve` and every answer command check it: the same tag, or a dev build, read
it; any other is refused with both tags and the image or binary to use,
unless `--allow-version-skew`, which tries and prints `UNVALIDATED`. `fetch`
prints the tag of each run, so a mix is seen before it is served. A release
that changes the format says so in its notes; a release that cannot read the
previous one's files is a breaking change.

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
(only the parsed parameters the capacity store keeps).

Kept in a raw file: the PromQL text in start and config lines. It helps
debugging, and holds no more than the policy it came from; `--pseudonymise`
redacts it, as the export does today.

The review this proposal asks for before phase 2 ships: one test that runs a
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

Parts start at 24 h. **Gate before the feature ships:** the final part
length, cap and default size are set from the measured disk per model per
day, and the docs state those numbers.

| cost | how it is measured |
| --- | --- |
| controller CPU | `process_cpu_seconds_total` of the controller, recording on vs off, same load |
| cycle time | `llmsm_optimization_duration_seconds`, p50 and p99, on vs off, and the new `llmsm_planner_recording_cycle_seconds` |
| memory | `go_memstats_heap_inuse_bytes`, on vs off |
| disk per day per model | `llmsm_planner_recording_bytes_written_total` (raw) and the closed parts' sizes (gzipped), per day, divided by models |
| Prometheus | the record-only queries recording turns on (`QueryTypeLearnerRecording`: router TPOT and flow-control histograms per Pod), counted per cycle |
| gzip | wall time and CPU of compressing one 256 MiB part in the background, and the cycle time while it runs |
| planner | `serve` time and memory to read a day of one model, whole and in cold mode |

Runs: the kind e2e with the simulator at 1, 8 and 32 replicas, each with
saturation alone and with saturation, slo and throughput, two hours on and two
off; and one 24 h run on a GPU cluster with one real model. The results go in
a table in the reference page, with the date and release.

**Budget.** Recording may add at most 5% to the cycle's p99 duration and 50m
CPU on average, for one model at 8 replicas (to be confirmed by the
measurement, then enforced).

**Hard off-switch.** Three, from fastest to strongest:

1. `plannerRecording: "off"`: by the end of the next cycle, closes the open
   parts and stops the record-only queries.
2. The recorder stops itself, and never blocks a cycle, when a write fails or
   recording took more than the budget for 10 cycles in a row. The state is
   `stopped` with a reason; setting the key `off` and `on` again restarts it.
3. `LLMSM_PLANNER_RECORDING=disabled` on the Deployment: wins over the key.
   The state is `disabled`, not `stopped`: nothing went wrong.

### Metrics and alerts

| metric | type | meaning |
| --- | --- | --- |
| `llmsm_planner_recording_state` | gauge, label `state` | 1 for the current state: `off` (the key), `disabled` (the Deployment variable), `on`, or `stopped` (the recorder stopped itself) |
| `llmsm_planner_recording_stop_reason` | gauge, label `reason` (`write_error`, `over_budget`) | while `stopped`, 1 for why |
| `llmsm_planner_recording_bytes_written_total` | counter | raw bytes written |
| `llmsm_planner_recording_dir_bytes` | gauge | bytes held, all parts, against the ceiling |
| `llmsm_planner_recording_oldest_timestamp_seconds` | gauge | start of the oldest part held: how many days you can plan from |
| `llmsm_planner_recording_events_dropped_total` | counter, label `reason` | events not written |
| `llmsm_planner_recording_parts_deleted_total` | counter | parts rotated out |
| `llmsm_planner_recording_namespaces_skipped` | gauge | model namespaces not recorded for lack of room |
| `llmsm_planner_recording_cycle_seconds` | histogram | time recording added to a cycle |

The controller status ConfigMap gains `plannerRecording` (`off`,
`disabled`, `on`, `stopped: <reason>`) and `plannerRecordingOldest`.

Alerts, in `config/components/prometheus-alerts`:

- `LLMSMPlannerRecordingStopped`: state `stopped` for 15 minutes.
- `LLMSMPlannerRecordingDropping`: events dropped for 15 minutes while on.

### Test gate

Phase 2 merges when the kind e2e does this, in CI, under a new label in the
existing full suite. Two installs, one after the other: a namespace-scoped
install (steps 1 to 8), then a cluster-scoped install
(`smt install --scope cluster`) with two model namespaces A and B (step 9).
The model Deployments carry a planted environment value
(`PLANTED_SECRET=<random>`) and a planted engine argument
(`--api-key <random>`).

1. Install with `--record`; assert the status says `on` and a part exists.
   The run is not `Cold`.
2. Drive the simulator through a load ramp for 20 minutes, with
   `plannerRecordingPart: 10m` and a small size, so at least one part closes
   and one rotates out.
3. Throughout, assert no eviction: the controller's `restartCount` stays 0
   (until step 7), no `Evicted` event names it, and
   `llmsm_planner_recording_dir_bytes` never exceeds the ceiling.
4. `smt recording fetch` as a ServiceAccount bound to the stock `admin`
   role only. Assert: the number of parts fetched equals the number the
   listing shows, and is more than zero (an empty fetch fails the step);
   every digest matches; the output has one file per run. Grep every fetched
   file for the ServiceAccount's token, `PLANTED_SECRET`'s value and the
   `--api-key` value: none found. As a ServiceAccount without `pods/log`:
   403. With `config/components/unauthenticated-metrics` applied (no
   `metrics-auth-role`): fetch is refused and its message names the role.
5. `capacity-planner serve --recordings` on the fetched directory; ask
   `replicas-to-slo` on the run and assert an answer, not a refusal; on the
   rotated run, assert the cold caveat in text and in `-o json`.
6. Patch the key `off`, then `on`. Assert: the open part closes, and the
   record-only queries stop, by the end of the next cycle; a new run opens by
   the end of the next cycle after `on`, and its start line says `Cold`.
7. Kill the controller container with SIGKILL (not a graceful stop), so the
   open part has a torn last line. Assert the next start closes it, and
   fetch gets it, without the torn line.
8. Fetch again: assert nothing is downloaded twice (`.parts` reused) and the
   outputs are rebuilt identically.
9. Cluster scope. As a ServiceAccount with `pods/log` on the controller and
   in A only: assert it lists and gets A's runs and no run of B. First, as a
   ServiceAccount with `pods/log` in both, fetch B's run and assert B's
   namespace, model and Pod names are present in it (the negative control:
   the grep below can find them). Then assert none of them appears in any
   file of A.

**Negative control for the gate.** Run the e2e once against the parent
commit, before the change: it must fail (no `plannerRecording` key, no
endpoint), and the PR says how it failed.

Unit tests: parts put end to end, part lines stripped, equal the recorder's
stream byte for byte; a part line whose `FirstSeq` does not match is an
error; a whole fetched run replays in Fidelity mode; cold mode on a run with
part 1 rotated out, and on a `Cold` start line; a gap after part 1 is
refused; rotation order and the cap per N; the gzip queue runs one part at a
time; the version check; the planted-secrets and planted-names tests; the
endpoint's path checks, token hashing, audiences and SubjectAccessReview
attributes.

## Risks and gates

| risk | gate |
| --- | --- |
| the recorded inputs are not enough to play a run from cold (cold mode) for some learner | **before the build starts:** a spike that cuts a whole recording at a mid-run cycle and plays the rest from cold for the saturation, slo and throughput analyzers and the capacity store, and shows each answers. The same spike splits a two-namespace recording into one run per namespace, with the other namespace's names in cross-namespace store records pseudonymised, and shows each run replays in Fidelity mode. If either fails, this proposal is revised before phase 1 |
| a checkpoint route is taken later | before that build starts: verify that replay accepts a checkpoint for the saturation, slo and throughput analyzers (each needs a new full-state checkpoint; see [Files and rotation](#files-and-rotation)) |
| the defaults do not hold a week, or hold far more | before the feature ships: part length, cap and default size set from the measured disk per model per day |
| the recorder's files evict the Pod | the ceiling at 80% of the size, counted by the recorder; a unit test that it never writes past it |
| a namespace's file names another namespace | the cluster-scope e2e check and a unit test over every event kind that spans namespaces |
| recording slows the cycle | the budget, enforced by the recorder's own stop |

## Phases

1. Parts, the part line, the `Cold` start field, the gzip queue, digests, the
   ceiling and caps, the version field, one run per model namespace (split on
   write, cross-namespace names pseudonymised), the reader changes and cold
   mode. The test aid uses them too.
2. The settings keys, the always-mounted `emptyDir`, the loopback endpoint,
   `smt recording fetch`, metrics, alerts, status. Remove the kustomize
   components and the sidecar. The e2e gate. Needs `smt install`
   ([Builds on](#record-for-the-capacity-planner-from-the-controller)).
3. The cost measurement; set the defaults from it; publish the table.
4. PVC: `plannerRecordingVolume`, `Recreate`, the replica check, upgrade
   fetch.

## Docs to update

| page | change |
| --- | --- |
| [Record a window for the capacity planner](../guides/record-for-capacity-planner/) | a new first path: from the controller. When to take it, and when Prometheus capture is still the answer |
| [Plan a P/D fleet](../guides/capacity-planner/) | serving fetched runs; cold mode; the same-tag rule |
| [Configuration](../reference/configuration.md) | the four keys and the precedence table |
| [Metrics](../reference/metrics.md), [Monitoring](../reference/monitoring.md) | the new metrics and alerts |
| [Security model](../reference/security-model.md) | what a file holds; who may fetch it; the endpoint and tokens |
| [Upgrade and uninstall](../reference/upgrade-and-uninstall.md) | fetch before an upgrade with an `emptyDir`; `Recreate` downtime with a PVC |
| [Troubleshooting](../reference/troubleshooting.md) | `stopped` and its reasons; a refused version; a refused fetch without `metrics-auth-role` or with a certificate kubeconfig |
| [Testing](../developer-guide/testing.md), "Learner Recordings" | the key replaces the patch and `kubectl cp` |
| [Capacity planner serve](../developer-guide/capacity-planner-serve.md) | the version check, cold mode |
| a new reference page, planner recordings | file format, runs and parts, the content table, the measured cost |

## Out of scope

- Full-state checkpoints for every learner (considered above).
- Shipping recordings anywhere off the cluster (object storage, a remote
  sink). Fetch is pull only.
- Recording from the scale-from-zero engine or the warm pool.
- Reading recordings in the controller, or serving the planner from it.
- A recording format readable across releases without the same-tag rule.
- ReadWriteMany volumes and several writers on one volume.
- Retention by age alone; the ceiling decides, the part age only closes
  files.

## Decided on review (2026-11-15)

1. **The tools image does not ship `capacity-planner`.** `smt` fetches; the
   planner runs from the planner image or binaries of the same release tag.
2. **Parts start at 24 h.** The final value is set from the measured disk per
   model per day before the feature ships ([Risks and gates](#risks-and-gates)).
3. **Cluster scope: access per model namespace.** Fetch needs `get pods/log`
   on the controller, and returns only files of model namespaces where the
   caller also has `get pods/log`. Files are split per model namespace on
   write, not filtered on serve
   ([Access per model namespace](#access-per-model-namespace)).
4. **PromQL text stays in raw files.** `--pseudonymise` redacts it.
5. **No checkpoints in this proposal.** Only saturation exports learned state,
   and a cut-down copy; slo, throughput and the capacity store export none. A
   run is split into parts read in order, and a run whose head is gone is read
   in cold mode ([Files and rotation](#files-and-rotation)).

## Open questions

None.
