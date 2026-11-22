# Capacity planner: every way to get and run it

The [quick start](README.md) takes one path: the binary for your computer,
on a sample. This page has the others: the container image and its
signature, the Windows binary from Git Bash, a build from a checkout,
reading Prometheus from your machine or from the container, and sharing a
planner that runs on another host. To run one planner in a cluster for a
team, see [Run it in a cluster](in-cluster.md).

Before v0.12.2 there is no Windows binary: on Windows, use the linux binary
in WSL. `cosign` 3.0 or later checks the image and the binary; 2.4 is enough
for the binary only.

## Get the planner

Use the planner of the release your controller runs. The planner replays
recordings with the controller's learners, so each release ships both under
one tag, and `capacity-planner version` prints it. The image and the
binaries ship from v0.11.2 on.

The image serves recordings only, so put one in `./recordings` first: your
own, from [Record a window](../record-for-capacity-planner/), or the sample.
With an empty directory the page says "This server reads no source".

<!-- guide:sample.get start -->
```bash
# Two sample recordings, from the source tree of the release you run. ols-sweep: one prefill and one decode
# replica, an hour of a test run. ttft-staircase-b-on: two and two, 72 minutes of rising load.
export V=<release, v0.12.0 or later>
mkdir -p recordings
curl -fsSL -o recordings/ols-sweep.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ols-sweep/recording.jsonl.gz
curl -fsSL -o recordings/ttft-staircase-b-on.jsonl.gz \
  https://raw.githubusercontent.com/llm-d-incubation/llm-d-scaling-manager/${V}/internal/engines/analyzers/learnreplay/testdata/goldens/ttft-staircase-b-on/recording.jsonl.gz
```
<!-- guide:sample.get end -->

Run the image:

<!-- guide:get.image start -->
```bash
# The image of the release you run. It serves the recordings in the directory you mount, with no authentication.
# --user runs it as you: the recordings the record guide writes are 0600, so the container must run as you to
# read them. -p 127.0.0.1:... keeps the port
# off your network; never publish it beyond 127.0.0.1. Rootless Podman: --userns=keep-id in place of --user,
# and with SELinux mount with :ro,Z.
export V=<release, v0.12.0 or later>
mkdir -p recordings
docker run --rm --user "$(id -u):$(id -g)" -p 127.0.0.1:8080:8080 -v "$PWD/recordings:/recordings:ro" \
  ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V}
```
<!-- guide:get.image end -->

Keep the port on `127.0.0.1`: the server has no access control of its own
([why](api.md#requests)).

To check the image's signature:

<!-- guide:get.verify start -->
```bash
# Optional. The image is signed by the release workflow, as the release files are. Needs cosign 3.0 or later; cosign 2.x reports no signatures for these images.
cosign verify ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V} \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/${V}"
```
<!-- guide:get.verify end -->

Then [open the page](#open-the-page). Any other command runs in place of
`serve`, for example `capacity-planner:${V} version`. To read Prometheus
from the container, see [In a container](#in-a-container).

Or download the binary for your platform. The checks are the ones
[the release files](../../reference/install-methods.md#verify-the-release-files)
use:

<!-- guide:get.binary start -->
```bash
# Or the binary for your platform: OS linux or darwin, ARCH amd64 or arm64; on Windows, the .exe below (v0.12.2 or later).
# Check it against SHA256SUMS and its signature (cosign 2.4 or later), as for the other release files. On macOS,
# use shasum -a 256 -c - for sha256sum -c -.
export V=<release, v0.12.0 or later> OS=linux ARCH=amd64
B=capacity-planner-${V}-${OS}-${ARCH}
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/${V}
curl -fsSL -O ${URL}/${B} -O ${URL}/${B}.sigstore.json -O ${URL}/SHA256SUMS
grep " ${B}$" SHA256SUMS | sha256sum -c -
cosign verify-blob ${B} --bundle ${B}.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/${V}"
mkdir -p bin && install -m 0755 ${B} bin/capacity-planner
bin/capacity-planner version
```
<!-- guide:get.binary end -->

On Windows:

<!-- guide:get.windows start -->
```bash
# On Windows (amd64), from Git Bash, which has curl and sha256sum. cosign as above, when you have it.
# The planner commands below run as written in Git Bash.
export V=<release, v0.12.2 or later>
B=capacity-planner-${V}-windows-amd64.exe
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/${V}
curl -fsSL -O ${URL}/${B} -O ${URL}/${B}.sigstore.json -O ${URL}/SHA256SUMS
grep " ${B}$" SHA256SUMS | sha256sum -c -
mkdir -p bin && cp ${B} bin/capacity-planner.exe
bin/capacity-planner.exe version
```
<!-- guide:get.windows end -->

In PowerShell, `Get-FileHash -Algorithm SHA256 <file>` prints the hash to
compare with the file's line in `SHA256SUMS`. On Windows, open the page
with `start http://127.0.0.1:8080/`; `serve --allow-upload` keeps uploads
in `%TMP%` (usually `%TEMP%`), and Ctrl-C stops it and removes them.

Or build it from a checkout:

<!-- guide:get.checkout start -->
```bash
# Or from a clone of the release you run, with Go 1.26 or later.
make capacity-planner
```
<!-- guide:get.checkout end -->

## Start the planner

With the binary or a checkout, pick one source. The server runs until you
stop it.

From Prometheus:

<!-- guide:start.prometheus start -->
```bash
# From Prometheus. Reach it from your machine first; use the Service your Prometheus has.
# The planner reads the namespace your model servers run in, with the server's own access to Prometheus.
# --kube-state-metrics reads each Pod's role and readiness from kube-state-metrics in the same Prometheus.
# ENGINE_ARGS are the decode Deployment's engine arguments, read as the record guide reads them: kube-state-metrics
# does not export them. Without them the planner does not know what they set, such as the batch cap; the Inputs
# drawer then shows the batch cap's source.
export PROM_NS=<prometheus namespace>
export PROM_SVC=<prometheus service>
export NAMESPACE=<model namespace>
export ENGINE_ARGS="<the decode Deployment's engine arguments, space-separated>"
kubectl -n ${PROM_NS} port-forward svc/${PROM_SVC} 9090:9090 &
bin/capacity-planner serve --prometheus http://localhost:9090 --namespace ${NAMESPACE} --kube-state-metrics \
  --engine vllm --engine-args "${ENGINE_ARGS}"
```
<!-- guide:start.prometheus end -->

Or from recordings:

<!-- guide:start.recordings start -->
```bash
# From recordings you saved with the record guide. Put each recording `generate` wrote (run.jsonl.gz) in one
# directory; the capture (run.capture.jsonl.gz) is not a recording. Every file in the directory is offered by name.
mkdir -p recordings
mv run.jsonl.gz recordings/
bin/capacity-planner serve --recordings ./recordings
```
<!-- guide:start.recordings end -->

Both sources can be served at once: give both flags. The server prints the
address it listens on, and refuses to start if `--recordings` is not a
directory. Every flag is in [the API page](api.md#flags).

### In a container

The image's own command serves the recordings mounted at `/recordings`, as
in [Get the planner](#get-the-planner). Name a recording by its file name in
**Source**.

To read Prometheus from the container, use the host's network, so the
container reaches your port-forward, and give the command below, which
listens on `127.0.0.1` ([why not the default command](api.md#in-a-container)).
With the variables set as above:

<!-- guide:container.prometheus start -->
```bash
# The image, reading Prometheus (Linux; on macOS use the binary). --network host lets the container reach your
# port-forward. Never use the image's default command with --network host: it would listen on every interface
# of your machine. Give the command below, which listens on 127.0.0.1 without --listen-all-in-container.
# For an https Prometheus, mount the token and CA files and pass --bearer-token-file and --ca-file.
# serve reads no kubeconfig unless --auth=kubernetes, so none is mounted. The port-forward uses 9091, so it does
# not clash with one already on 9090.
kubectl -n ${PROM_NS} port-forward svc/${PROM_SVC} 9091:9090 &
docker run --rm --network host ghcr.io/llm-d-incubation/llm-d-scaling-manager/capacity-planner:${V} \
  serve --listen 127.0.0.1:8080 --prometheus http://127.0.0.1:9091 --namespace ${NAMESPACE} --kube-state-metrics \
  --engine vllm --engine-args "${ENGINE_ARGS}"
```
<!-- guide:container.prometheus end -->

## Open the page

<!-- guide:open start -->
```bash
# The server listens on your machine only. Leave it running and open the page.
xdg-open http://127.0.0.1:8080/ || open http://127.0.0.1:8080/
```
<!-- guide:open end -->

With the sample recording, open this address instead. It assumes a prefill
rate (V_P, prefill tokens per second per prefill replica), because the
sample has no prefill measurement. With your own recording, leave `vp` out;
only if the page refuses with `NoPrefillCapacity`, type one under **Assume a
term the source did not learn**
([the refusal](troubleshooting.md#refused-figures)).

<!-- guide:sample.open start -->
```bash
# vp=30000 assumes a prefill rate V_P of 30000 tokens/s per prefill replica: the sample has no prefill
# measurement to learn it from. The address also sets targets the sample can meet. If neither command opens
# a browser, paste the address into one.
xdg-open 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004' \
  || open 'http://127.0.0.1:8080/#recording=ols-sweep.jsonl.gz&vp=30000&priceAs=1k&out=1000&ttft=0.5&tpot=0.004'
```
<!-- guide:sample.open end -->

The page opens on the last hour of Prometheus, ending now; with recordings
alone, on the first recording. To read another window or recording, open
**Source** at the top, pick it and press **Load**. If the source holds more
than one model, type the model's name; if the model has more than one prefill
or decode variant, type the variant's name too.

When you are done:

<!-- guide:stop start -->
```bash
# Ctrl-C stops the server. Only if you started a port-forward, stop it too, from the shell that started it (%1 is that shell's first background job).
kill %1
```
<!-- guide:stop end -->

## Run it for others

To use a planner running on another host, tunnel to it. The browser then
names `127.0.0.1`, which a server with no authentication answers:

<!-- guide:tunnel start -->
```bash
# From your laptop, to a planner running on a host you reach by SSH. The browser then names 127.0.0.1, which the server answers.
ssh -N -L 8080:127.0.0.1:8080 <user>@<host>
# then open http://127.0.0.1:8080/ on your laptop
```
<!-- guide:tunnel end -->

A proxy in front of a server with no authentication must keep the `Host`
header the browser sent, `127.0.0.1:8080`, or the server answers 403
([why](api.md#requests)); do not expose such a proxy beyond your machine.

Bound to any address other than loopback, the server needs Kubernetes
authentication and TLS (`--auth=kubernetes`). The page is then behind the same
check as the API: a browser must send a bearer token, so open it through a
proxy that adds one. The flags and the RBAC are in
[Kubernetes authentication](api.md#kubernetes-authentication).
