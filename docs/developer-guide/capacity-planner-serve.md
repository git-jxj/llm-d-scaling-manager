# Capacity planner serve: the HTTP API

`capacity-planner serve` runs the capacity planner's HTTP API on your machine. You
resolve a source once (a recording, or a window of Prometheus), then ask for
plans against it: curves, the two points on each graph, the fleet's limits and
what one more replica of each role would change; or ask the other way round,
for the fewest replicas a rate needs. Every figure is one the
`capacity-planner` CLI also prints, from the same code.

This is phases 0 and 1 of [the capacity planner](../proposals/capacity-planner.md),
the API and the page at `/` that draws it, and phase 2's inverse mode
(`POST /v1/size`) and the page's saved scenarios and compare. Operators start from
[Plan a P/D fleet with the capacity planner](../guides/capacity-planner/); this
page is the API's. Like the rest of `capacity-planner`, it is a contributor and
operator tool and is not in the manager image.

## Start it

From a recordings directory:

```
make capacity-planner
bin/capacity-planner serve --recordings ./recordings
```

`make capacity-planner` stamps the commit into the build, as the recordings
it makes carry it.

It listens on `127.0.0.1:8080` and prints the address on stderr. Stop it with
Ctrl-C.

From Prometheus (through a port-forward, as `capacity-planner capture` reads it):

```
bin/capacity-planner serve --prometheus http://localhost:9090 --namespace llm --kube-state-metrics
```

Both sources can be served at once.

## Ask for a plan

Resolve the inputs. A recording is named by its file name in `--recordings`,
never by a path:

```
curl -s localhost:8080/v1/inputs -H 'Content-Type: application/json' -d '{"model": "model-02",
  "source": {"kind": "recording", "name": "ols-sweep.jsonl.gz"}}'
```

With no `source`, the inputs are a Prometheus window of the last hour, ending
now. A window of your own:

```
curl -s localhost:8080/v1/inputs -H 'Content-Type: application/json' -d '{"model": "qwen3-32b",
  "source": {"kind": "prometheus", "namespace": "llm",
             "from": "2026-11-09T10:00:00Z", "to": "2026-11-09T11:00:00Z"}}'
```

`model` may be left out when the source holds one model. A model with more
than one variant of a role names the one to price with `prefillVariant` and
`decodeVariant` (the CLI's `--prefill-variant` and `--variant`), beside
`model`; the error that asks for one lists the names.

The answer carries an `inputsId`. Price a scenario against it:

```
curl -s localhost:8080/v1/plan -H 'Content-Type: application/json' -d '{"inputsId": "sha256:...",
  "fleet": {"prefill": 1, "decode": 1},
  "traffic": {"classes": {"critical": {"rate": 6}}, "mix": {"promptTokens": 1000, "outputTokens": 1000}},
  "slo": {"ttft": {"statistic": "p95", "target": 0.2}, "tpot": {"statistic": "mean", "target": 0.004}},
  "assume": {"vp": 30000}}'
```

Everything but `inputsId` has a default: the recorded replica counts, the
observed mix, the policy's SLO and headroom, and as the rate the operating
point (headroom times the lower knee).

## Flags

| Flag | What it does |
| --- | --- |
| `--recordings DIR` | Recordings a `recording` source names by file name. Serve refuses to start if it is not a directory. |
| `--prometheus URL` | The Prometheus a `prometheus` source reads. With it, every [capture flag](capacity-planner-capture.md) but `--from`, `--to` and `-o` applies: the role map, `--step`, `--policy`, the engine, TLS and the bearer token. The window comes with each request. |
| `--namespace NS` | The namespace a `prometheus` source reads when the request names none. |
| `--allowed-namespaces NS,...` | The namespaces a `prometheus` source may read. Default `--namespace` alone; with `--prometheus` one of the two is required. A request for any other namespace is 403, and a namespace that is not a DNS-1123 label a 400. |
| `--batch-cap N` | The engine's batch cap a window is captured with, as `capture --batch-cap`. |
| `--listen ADDR` | Where to serve. Default `127.0.0.1:8080`. Any address that is not loopback needs `--auth=kubernetes`; serve refuses to start without it. |
| `--auth none\|kubernetes` | `none` (the default) is loopback only. `kubernetes` checks a bearer token with TokenReview, then asks a SubjectAccessReview for `get` on the non-resource URL `/whatif` plus the path (`/whatif/v1/plan`). A missing or unauthenticated token is 401, a user not allowed is 403. A `prometheus` source's namespace is checked too: the user must be allowed `get` on `pods` in it. Reviews are reused for 30 s; a review that fails is a 500 that names no client error (the error goes to stderr). The client comes from `KUBECONFIG`, `~/.kube/config` or the in-cluster config. |
| `--tls-cert-file`, `--tls-key-file` | The server's certificate and key. Required with `--auth=kubernetes`: a token is not sent in the clear. |
| `--token-audiences A,...` | The audiences a TokenReview checks a token for. Default: the API server's own. |
| `--shutdown-timeout` | How long requests in flight may run after Ctrl-C. Default 10s. |

No request can name a host or a file path: the Prometheus URL is a flag, and a
recording is a file name inside `--recordings`. An error names no server path.

## Requests

Every request body is one JSON value of at most 1 MiB, sent as
`Content-Type: application/json`, in every mode: any other media type is 415,
so no cross-site form reaches the server. Every error, whatever its status, is
the same shape:

```json
{"schemaVersion": 1, "error": "slo.ttft.statistic \"p100\", want mean or a percentile from p1 to p99.9 (p90)"}
```

With `--auth=none` the server answers only requests addressed to a loopback
name (`localhost`, `127.0.0.0/8`, `[::1]`, by the same rule `--listen` is
checked by), so a web page that rebinds its own name to 127.0.0.1 cannot read
it.

## Kubernetes authentication

The server reads Prometheus with its own credentials, so a namespace is the
server's to allow (`--allowed-namespaces`), and with `--auth=kubernetes` the
user's too: `get` on `pods` there. Access to the API itself is `get` on the
non-resource URL `/whatif/` plus the path, for every method: a `POST
/v1/plan` is authorized as `get` on `/whatif/v1/plan`, since the API changes
nothing. The path keeps the command's old name, so RBAC written for an earlier
release still applies.

The RBAC for a group `planners`, and the ServiceAccount `planner` the token
example below uses, that read the namespace `llm`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: capacity-planner
rules:
- nonResourceURLs: ["/whatif/*"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: capacity-planner
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: capacity-planner
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: planners
- kind: ServiceAccount
  name: planner
  namespace: llm
---
# One Role and RoleBinding per namespace in --allowed-namespaces.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: capacity-planner-reader
  namespace: llm
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: capacity-planner-reader
  namespace: llm
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: capacity-planner-reader
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: planners
- kind: ServiceAccount
  name: planner
  namespace: llm
```

The server's own identity (its kubeconfig) needs `create` on
`tokenreviews.authentication.k8s.io` and
`subjectaccessreviews.authorization.k8s.io`, as `system:auth-delegator`
grants.

A request with a token, from the ServiceAccount `planner` in `llm` that the
bindings above name. Start the server; it runs in the foreground:

```
bin/capacity-planner serve --auth=kubernetes --listen 0.0.0.0:8443 \
  --tls-cert-file tls.crt --tls-key-file tls.key \
  --prometheus https://prometheus.example:9090 --kube-state-metrics --namespace llm
```

Then, in another terminal:

```
TOKEN=$(kubectl -n llm create token planner)
curl -s --cacert ca.crt https://planner.example:8443/v1/inputs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model": "qwen3-32b", "source": {"kind": "prometheus", "namespace": "llm"}}'
```

A token minted for another audience (`kubectl create token --audience capacity-planner`)
is accepted only when the server is started with `--token-audiences capacity-planner`;
with none, TokenReview checks the token for the API server's own audiences.

## Endpoints

| Method and path | Does |
| --- | --- |
| `POST /v1/inputs` | Resolves a source for one model: the replay of a recording to a cycle, or a Prometheus window captured and generated in memory (seconds). Returns `inputsId` and every term with its source. |
| `POST /v1/plan` | Prices a scenario against an `inputsId`. |
| `POST /v1/size` | The fewest prefill and decode replicas whose fleet limit reaches a rate, under the SLO, with the plan at that fleet; or why no fleet within the bound does. |
| `GET /v1/sources` | What the server reads, for the page's source picker: whether it reads a Prometheus, the namespaces it may read and the default one, and the file names in `--recordings` that a `recording` source may name (a regular file or a link to one, under a name that is no path and not hidden), sorted. It does not open them: a file that is not a learner recording is listed, and refused when loaded. It lists at most 1000, with `recordingsTruncated` true past that. Any user the API answers sees all of these: every allowed namespace and every recording name. |
| `GET /` | The page. Its script and style are `GET /page/planner.js` and `GET /page/planner.css`. |

`GET /v1/models` is a later phase. Any other path is a 404.

### The page

The page is three files in `internal/whatifapi/page/`, embedded in the binary
with `go:embed`: plain HTML, CSS and JavaScript, no library and no build step.
It is served on the same handler as the API, so the same check guards it:
loopback names only with `--auth=none`, and a bearer token allowed `get` on
`/whatif/` (the page) and `/whatif/page/*` (its files) with
`--auth=kubernetes`. The `/whatif/*` rule above covers both. A browser does not
send a bearer token by itself: with Kubernetes authentication, open the page
through a proxy that adds one. The page reaches its files and the API by paths
relative to itself, so a proxy may serve it under a prefix; open it with the
trailing slash (`/planner/`, not `/planner`).

The page holds no model. It calls `GET /v1/sources`, `POST /v1/inputs`,
`POST /v1/plan` and, in size mode, `POST /v1/size`, and draws what they return: a refused figure is drawn as a gap
or its reason, never as a number, and every string the API returns is escaped
before it is drawn. It computes only the axes' ranges and which knee is lower.
Its Content-Security-Policy allows its own script, style and API only, and no
inline script or style. One plan is in flight at a time; a knob moved
meanwhile is priced when it returns, and a response priced against inputs a
later load replaced is dropped. The rate prices on drag (100 ms after the last
move), a target on release.

A toggle above the controls switches between the two questions: "How much does
this fleet serve?" (plan mode) and "What fleet does this traffic need?" (size
mode). In size mode the page posts the same scenario less the fleet to
`POST /v1/size`, with `maxReplicas` from the "Search up to" box and
`sizeOn: low` when "Size for the worst corner of V_P and f" is ticked; the replica
steppers become read-only outputs showing the answer, and the rest of the page
draws the plan the size returned. A rate prices on release in size mode, not
on drag, since each size searches several fleets. With no fleet within the
bound, the page shows the refusal and the figure at the bound, and no plan.
Where the answer's limit at its worst corner of V_P and f is under the rate,
the size box says so. The per-bucket view is unticked while sizing and put
back on switching to plan mode, which keeps the fleet the size found. A
response for a question since switched, or for inputs since reloaded, is
dropped. While a size runs the page dims the last answer and counts the
seconds; a newer question aborts the request (`AbortController`), and the
server stops pricing it. A size no fleet serves is explained in the page's
words (the "Search up to" box, the term to type, the target to raise), with
the server's detail shown only for refusals the page does not name.

The address's fragment carries the source and the knobs
(`#recording=run.jsonl.gz&prefill=2&rate=6&ttft=0.8&vp=30000`), and the page
rewrites it after each plan. Seconds are seconds, as the API takes them. In
size mode it carries `mode=size`, `max` when it is not 32 and `low=1` when
sizing for the worst corner, and no replica counts: they are the answer.

Saved scenarios and compare (phase 2) are the page's alone; the API has no
endpoint for them and the server keeps nothing. A saved scenario is the
fragment's text (`linkParams()` in `planner.js`) under a name, in the
browser's `localStorage` key `capacity-planner.scenarios`, at most 50. An
exported one is a JSON file:

```json
{"kind": "capacity-planner-scenario", "version": 1, "name": "two prefill",
 "link": "recording=run.jsonl.gz&prefill=2&decode=1&rate=6&ttft=0.8", "saved": "2026-11-14T21:08:00.000Z"}
```

A file (at most 64 KiB) and every store entry read back are checked by
`checkScenario` before use: the kind and version, a name of 1 to 80
characters, and a link of at most 4096 characters whose every key is one
`linkParams` writes, each once, numbers finite and not negative, statistics
`mean` or `pNN`, and a recording or namespace named. A file that fails is
refused with the reason; a store entry that fails is left out. Names are put
into HTML only through `esc`. Opening a scenario replaces the link the page
read at start and loads its source as the page does when it opens; a
recording or namespace the server does not list is refused before any
request.

"Pin as A" copies the plan response on screen (in size mode, the answer's
plan) into `state.pin`. It is not priced again: the graphs draw its TTFT and
TPOT curves dashed (class `pin`), with its point B and its target where that
differs, and the compare box tables its fleet, limits, headroom, binding and
targets beside the plan on screen, with the change. The x axis runs far enough
to show both. A size no fleet serves has no plan, so nothing to pin, and the
table says so while A stays pinned.

To look at the page against the goldens' recordings:

```
make capacity-planner
mkdir -p /tmp/recs
cp internal/engines/analyzers/learnreplay/testdata/goldens/ols-sweep/recording.jsonl.gz /tmp/recs/ols-sweep.jsonl.gz
bin/capacity-planner serve --recordings /tmp/recs
# open http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004
```

The recording learned no V_P, so the link assumes one.

### The inputs

| Key | What it is |
| --- | --- |
| `inputsId` | `sha256:` and a hash of the recording's bytes, the cycle, the model and the variants. The same source gives the same id. |
| `source` | The kind, the recording's name or the window, the cycle read and how many the recording holds. |
| `fleet` | The prefill and the decode variant, with their replica counts at the cycle. |
| `observed` | The mean prompt and output and the prompt's spread; the fixed buckets (1k, 4k, 8k, 32k, 128k, each named by its upper edge) and the one the mean prompt falls in. The request rate and the share per bucket are refused: the replayed state carries neither. |
| `policy` | The policy's TTFT and TPOT objectives, the headroom (`scaleUpThreshold`), and the flow-control state. |
| `terms` | V_P, a, f, the prompt and its cv, the KV transfer, the router hop, the ITL line, the KV capacity and per-request KV, the batch cap, and R_q at p90: each with its source, or the refusal that stopped the fleet there. |
| `assumed` | Every input the source did not give, in the CLI's words. |

### The plan

| Key | What it is |
| --- | --- |
| `scenarioId` | A hash of the scenario with its defaults applied. The same scenario is the same id and the same bytes. |
| `banner` | What every TTFT figure says of itself, from `constants.SLOTTFTPredictionCaveat`. |
| `scenario` | The request with its defaults applied. |
| `ttft.line` | The mix line: its curve (65 rates, crowded toward the lower knee, the last two at and past it), its floor, point A (`atRate`, the TTFT at the chosen rate) and point B (`atTarget`, the rate at the target). |
| `ttft.perBucket` | With `views.allTrafficPerBucket`, one line per bucket, "if all traffic were bucket b": each priced with every request at the bucket's upper edge, with its own knees, target (`slo.ttft.bucketTargets`, default the mix target) and points. No limit reads it. |
| `ttft.markers` | The prefill and decode knees, the operating-point cap (headroom times the lower knee), and the flow-control onsets when flow control is on. |
| `tpot` | The TPOT line at the statistic asked (`mean`, or `pNN` priced as the mean times R_q, with R_q's source in `ratio`), its curve, floor and points. Absent with `"tpot": {"statistic": "none"}`, or when neither the request nor the policy sets a TPOT target. |
| `limits` | `prefill` (`slo-to-rate`'s prefill rate), `decode` (its decode rate), `fleet` (its fleet rate, the operating-point cap applied, with `withoutHeadroom` at headroom 1, `capApplied`, and `binding`, the role with the lower limit), and `chosen`, the chosen rate and its headroom under the fleet's limit. |
| `marginal` | The fleet's limit with one more prefill replica, and with one more decode replica: the limit, the change, and which role binds then. These limits carry no span over V_P and f. |
| `flags` | Every flag in the plan once, with the places it qualifies. |
| `assumed` | Every assumed input of the scenario's fleet, in the CLI's words. |

Every figure is an object holding `value` or `refused`, never both. A refused
figure has no number in it, and no refusal is written as 0. A figure's `band`
is its bootstrap band over the ITL line's readings; its `sensitivity` the span
over V_P and f, ±15%, with the worse side refused (`hiRefused`, `loRefused`)
where a corner is.

The full key list is pinned in
[`internal/whatifapi/testdata/schema-v1.txt`](../../internal/whatifapi/testdata/schema-v1.txt).
`whatifSchema` names the CLI schema the answer, refusal and flag shapes follow.

### What the scenario takes

| Key | Default | Notes |
| --- | --- | --- |
| `fleet.prefill`, `fleet.decode` | the recorded counts | 1 to 64. |
| `traffic.classes.critical.rate` | the operating point | r/s over the model: in v1 the total rate. `regular` and `batch` must be 0. |
| `traffic.mix` | `"observed"` | Or `{"promptTokens": N, "outputTokens": N}`: priced as the CLI's `--assume-input-tokens` and `--assume-output-tokens`, flagged `AssumedShape`. |
| `traffic.arrivalCV2` | Poisson | As `--arrival-cv2`. |
| `slo.ttft` | the policy's | `statistic` `mean` or `pNN`, `target` in seconds. A TTFT target is required. |
| `slo.tpot` | the policy's (a mean) | `statistic` `mean`, `pNN` or `none`. |
| `headroom` | the policy's `scaleUpThreshold` | (0, 1]. |
| `assume` | none | `vp`, `a` (seconds), `promptCV`, `tpotRatio`, `routerHop` (seconds): as the CLI's `--assume-*` flags, each flagged. |
| `views.allTrafficPerBucket` | off | The per-bucket view. |

### The size

`POST /v1/size` takes the plan's keys but `fleet` and `views`, and two more:
`maxReplicas` (`{"prefill": N, "decode": N}`, 1 to 64 each, 32 by default),
the most replicas of a role it tries, and `sizeOn`, what the fleet must reach
the rate with: `point`, its limit (the default), or `low`, the low end of the
limit's span over V_P and f. The rate, `traffic.classes.critical.rate`, is
required. The inputs must have a prefill variant.

How it searches. Every fleet it tries is priced as the plan's +1 steps are
(the limits at the targets and headroom, with no span over V_P and f), or with
`sizeOn: low` as the plan's own limits are (with the span, five times the
cost). Each count is found by galloping up from one replica (1, 2, 4, 8, ...
up to the bound) and then bisecting back, so the cost grows with the answer,
not with the bound: a rate one replica of each serves prices one fleet.

1. Decode: the fewest decode replicas whose own limit (TPOT, and the
   operating-point cap on decode's knee) reaches the rate, priced at one
   prefill replica, since the prefill count does not move it. With no TPOT
   asked, one.
2. Prefill: the fewest whose fleet limit reaches the rate at that decode
   count.
3. Only when no prefill count within the bound does: decode again, the
   fewest above that count whose fleet limit reaches the rate with prefill at
   its bound, then prefill again. This is the coupling: the lower knee and
   decode admission hold the prefill limit down, so the fleet may need more
   decode replicas than TPOT alone asks for.

A limit reaches the rate when it is no more than a billionth under it: limits
are bisections' answers, and on the worked recording the fleet limit wanders
by up to 8e-13 of itself as decode replicas are added on a plateau.

The search holds one plan slot from its start through its answer's plan, which
it prices as `POST /v1/plan` would (same request, the fleet set) and reads the
answer's limit, headroom and binding from, so the answer is what a plan of
that fleet says. The search is kept by `sizeId` before that plan is priced:
a size whose plan did not finish (its client gone) is not searched again.
Where the same plan is already being priced for another request, the size
waits for it; if that request was turned away busy, the size asks once more,
on its own slot. Decode's own limit has no span, so its probes are priced
without one even when sized `low`.

The search takes each limit to rise with its replica count. Where one does
not, galloping and bisecting can step over a smaller count that serves the
rate; the count just below the answer is always priced and checked, so a fleet
with one replica fewer of either role that serves the rate is a 500, not an
answer, and so is a plan at the answer that does not.

| Key | What it is |
| --- | --- |
| `sizeId` | A hash of the scenario, the bound and `sizeOn`. The same size is the same id and the same bytes. |
| `rate`, `maxReplicas`, `sizeOn` | The rate sized for, the bound searched, and what the fleet was sized on. |
| `searched` | How many fleets the search priced, the answer's plan aside. |
| `answer.fleet` | The fewest replicas: decode's fewest first, then prefill's fewest with them. |
| `answer.limit`, `answer.binding` | The plan's `limits.fleet` at that fleet, with its span over V_P and f in `sensitivity`, and the role that binds; `limit.boundBy` says which SLO or cap sets it. Sized on the `point`, the span's low end may be under the rate. |
| `answer.headroom` | The limit less the rate; sized `low`, the span's low end less the rate, the same basis as the steps'. |
| `answer.fewerPrefill`, `answer.fewerDecode` | The fleet with one replica fewer of the role: its limit (no span, unless sized `low`), how far short of the rate it is (`headroom`, negative; sized `low`, the span's low end's shortfall), and the role that binds. Absent when the role has one replica. |
| `answer.plan` | The plan at the fleet, as `POST /v1/plan` returns it, flags and assumed inputs included. |
| `unmet` | Instead of `answer`: `refused` (the engine's reason where it refuses the figure, such as `NoPrefillCapacity` or `TargetBelowFloor`, and sized `low`, where it refuses the figure at the worst corner of V_P and f, which no replica count changes; else `ReplicaBoundReached`, with the figure the bound reached), `role` (`decode`, `prefill`, or `fleet` when each role alone reaches the rate and the fleet does not), and `atBound`, the fleet at the bound as a step. |

A size with no fleet within the bound is a 200 with `unmet`, not an error: a
refusal is a figure, as in a plan.

A field asked wrongly is a 400 naming it, and an `inputsId` the server no
longer holds a 404: resolve the inputs again. The server prices as many plans at
once as it has CPUs, and two inputs at once; past that a request is 503 with
`Retry-After: 1`. A plan whose client goes away stops between figures. Reading
a request may take 30 s, an idle connection is closed after 2 minutes, and a
response may take 10 minutes (a cold plan, or a capture).

## Caching and latency

The server keeps caches only: the inputs by `inputsId`, each plan by
`scenarioId`, each fleet's curves by its knobs (replicas, mix, statistics,
assumptions), and each fleet's limits and marginal steps by its knobs, the
targets and the headroom, and each size by `sizeId`. So moving the rate prices the two points alone;
moving a target prices the limits again, not the curves; a new replica count
or assumption prices everything.

Measured on the goldens' recordings (`go test ./cmd/capacity-planner -run '^$' -bench
Plan`), see [As built](../proposals/capacity-planner.md#as-built-phase-0).

## Tests

| Test | Holds |
| --- | --- |
| `TestServeParity` (`cmd/capacity-planner`) | Every JSON golden in `cmd/capacity-planner/testdata`, asked of the API, gives the same limits, points, bounds, flags and refusals. |
| `TestServeCurvesAgreeWithPoints` | Each curve point is `TTFTAt` and `SLOAtReplicas` at its rate; point B lies on the curve; past the lower knee every point is refused `Saturated`. |
| `TestSchemaIsPinned` (`internal/whatifapi`) | The key list. `-update` rewrites it. |
| `TestPlanRefusalShape`, `apitest.CheckFigures` | No number beside a refusal, and no refusal written as 0, on every response. |
| `TestCaching` | The same scenario is the same id and bytes; a rate move prices no curve and no limit; a target move prices limits only. |
| `TestPageServed`, `TestServeServesThePage` | The page and its files are served from the binary with their content types and security headers; anything else under `/page/` is a 404. |
| `TestPageBehindAuth` | The page sits behind the API's check: 401 with no token, 403 for a user not allowed `/whatif/`, 403 for a foreign Host on loopback. |
| `TestPageLoadsNothingExternal`, `TestPageIDsExist` | The page names no other origin, runs no inline script or style, and uses no absolute path; every element its script reads is on it. |
| `TestPageNamesEveryFlag` | Every flag the engine, the SLO analyzer and the CLI declare has plain words on the page. |
| `TestSourcesListingFails` | A listing that fails is answered by its error's class, never as a listing. |
| `TestServeSources`, `TestSourcesWithoutLister` | `GET /v1/sources` lists every recording a `recording` source accepts, links included, and nothing it refuses; it says when it cut the list, and whether a Prometheus is read; an error names no server path. |
| `TestSizeAnswerIsThePlan`, `TestServeSizeIsThePlan` | A size's answer is `POST /v1/plan` at its fleet, byte for byte on the stub and key for key on the goldens' worked fleet. |
| `TestSizeOneFewerFails` | One replica fewer of either role misses the rate, in the step and in a plan of that fleet. |
| `TestSizeRefusals`, `TestServeSizeRefusesATermRefused` | No fleet within the bound is `unmet` with its reason and the figure at the bound: the bound reached by prefill, by decode, or by the fleet with neither role alone short; a refused term; a TPOT percentile with no ratio; no advice to raise a bound already at 64. |
| `TestSizeRequestsRefused`, `TestSizeCachingAndCost` | A size asked wrongly is a 400 naming the field; the same size is the same bytes, searched once, in a handful of fleets, and one fleet for an answer of one of each. |
| `TestSizeSearchSurvivesItsPlan` | One plan slot sizes; a size whose answer's plan ended with its client is not searched again. |
| `TestSizeOnTheLowEnd` | `sizeOn: low` sizes on the low end of the span, where `point` leaves it under the rate; the answer's headroom is the low end's. |
| `TestSizeOnLowRefusedAtTheWorstCorner` | Sized `low` where the worst corner refuses the limit, the size is that refusal, said of the corner, never `ReplicaBoundReached`. |
| `TestSizeConcurrentSearchOnce`, `TestSizeBusyAndDecodeOnly` | Identical sizes at once search once; with every slot taken a size is a 503 before any search; a source with no prefill variant is a 400. |
| `TestSizeOneFewerThatServesIsAnError` | On a limit that dips, a fleet one replica fewer that serves the rate is a 500, not an answer. |
| `TestSizeBehindAuth`, `TestPageAsksSize` | `/v1/size` sits behind the same check as the other endpoints, authorized as `get` on `/whatif/v1/size`; the page has the mode toggle and posts sizes by a relative path. |
| `TestPageSavesAndCompares` | The save, open, delete, export, import, pin and unpin controls are on the page and wired; the store and the file hold the link's text; every key the link writes is one an import admits; a name read back reaches HTML only through `esc`; a pinned plan is a copy drawn dashed. |
| `TestCheckListen`, `TestAuthenticate`, `TestServeRefusesUnsafeBinds` | Loopback serves with no token; any other bind refuses to start without `--auth=kubernetes`; with it, a token the SubjectAccessReview does not allow is 403. |
