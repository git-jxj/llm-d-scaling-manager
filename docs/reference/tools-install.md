# Install commands of the tools image

`smt admin-setup`, `smt install` and `smt admin-finish` install the scaling
manager from the release files the tools image carries. Step by step:
[install in a namespace](../guides/install-in-namespace/) or
[for the whole cluster](../guides/install-cluster-wide/). Design:
[two-command install](../proposals/two-command-install.md).

## Set up `smt`

Once per machine. Name the image by digest: the cluster admin hands it
cluster-admin credentials, and a tag can move. The release run's summary
prints the digest, in the `cosign verify` command for it.

```bash
TOOLS=ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools@sha256:<the release's tools digest>
mkdir -p ~/.smt && kubectl config view --minify --flatten > ~/.smt/kubeconfig && chmod 600 ~/.smt/kubeconfig
smt() { docker run --rm --user "$(id -u):$(id -g)" \
  -v "${SMT_KUBECONFIG:-$HOME/.smt/kubeconfig}:/kube/config:ro" -v "$PWD:/work" "$TOOLS" "$@"; }
```

Run it from a working directory, not your home directory: that directory
is mounted read-write at `/work`, and `~/.smt` must stay out of it. Each person uses
their own kubeconfig. The container cannot run kubeconfig `exec` plugins
(cloud CLIs are not in it): use a token kubeconfig. An API server at
`127.0.0.1` (kind, a local tunnel) is reachable from the container only with
`docker run --network host`.

The kubeconfig is a snapshot of your current context. If you are both the
cluster admin and a namespace owner, keep one snapshot per identity and
point `SMT_KUBECONFIG` at the one to use, or the two commands run as the
same identity without saying so.

**Who needs what.** The cluster admin runs `admin-setup` and `admin-finish`.
The namespace owner needs the stock `admin` ClusterRole bound in the namespace
(a RoleBinding) and nothing cluster-wide: that is all `install` uses.

To check the image's signature first:

```bash
cosign verify "$TOOLS" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/<release>
```

The signature covers the release files inside the image; `install` checks
them against their `SHA256SUMS` before it uses them.

## The commands

| command | who | what it does |
| --- | --- | --- |
| `smt admin-setup -n NS [--keda-namespace KNS]` | cluster admin, once per namespace | the `part=admin` objects, the preflight's RBAC, the model-namespace file, KEDA's namespace in the scaler NetworkPolicy, and a stamp (ConfigMap `scaling-manager-admin-setup`). Then adds NS to the [weights labeller](workload-preparation.md#gate-replicas-on-the-weights)'s watch list, with rights in NS and the labeller's namespace only; with no labeller, or without those rights, it prints the command instead and still succeeds |
| `smt install -n NS --prometheus-url URL` | namespace owner | the settings (created once), the preflight, the scaler TLS Secrets (created once), the controller, the preflight again |
| `smt admin-finish -n NS` | cluster admin, after `install` | KEDA's client certificate, a controller restart when it changed, waits for `mutual TLS`, the preflight, then removes the preflight's cluster reads |
| `smt install --scope cluster --keda-namespace KNS --prometheus-url URL --model-namespace NS` | cluster admin | all three, for one controller in `scaling-manager-system`, and the [weights labeller](workload-preparation.md#gate-replicas-on-the-weights) (it patches nodes), watching every `--model-namespace` and every namespace it already watches. A new labeller only when a model namespace has a weights DaemonSet; otherwise it prints how to add it |
| `smt release-files --out DIR` | anyone | writes the release files the image carries, to verify with cosign or apply by hand |

Every command can be run again, and again after a failure part way. A
re-run changes nothing that is already right: the settings and the scaler TLS
Secrets are never written over, and `admin-finish` restarts the controller
only when KEDA's side changed.

## Flags

| flag | commands | meaning |
| --- | --- | --- |
| `-n NS` | all but `release-files` | the namespace the model servers and the controller run in |
| `--keda-namespace KNS` | `admin-setup`, `install --scope cluster`, `admin-finish` | where KEDA runs. `admin-setup` default: the live scaler NetworkPolicy, else the namespace serving the external-metrics APIService, else (OpenShift) `openshift-keda`. `admin-finish` default: the namespace `admin-setup` wrote in its stamp, which must agree with the live NetworkPolicy, else the APIService |
| `--prometheus-url URL` | `install` | sets `prometheusURL` when the settings are created. Required then, except on OpenShift (Thanos Querier) |
| `--prometheus-ca FILE` | `install` | a CA file under the mounted `/work`: creates or updates Secret `scaling-manager-prometheus-ca`, sets `prometheusCACertPath` on a first install, restarts the controller when it changed |
| `--allow-http` | `install` | for a Prometheus that serves plain HTTP only: sets `prometheusAllowHTTP: "true"` on a first install, and applies the controller with `PROMETHEUS_TOKEN_PATH` empty, since the controller refuses to send its token over HTTP. Give it on every run: `install` refuses an `http://` URL (the flag's, or the live settings') without it, and an `https://` one with it. Not with `--prometheus-ca` |
| `--quota "GPU=N[,GPU=N]"` | `install` | sets `limiter: quota` and `quotas` |
| `--set KEY=VALUE` | `install` | any other settings key, on a first install. Not a key a flag sets (`prometheusURL`, `prometheusCACertPath`, `prometheusAllowHTTP`, `quotas`; `limiter` with `--quota`). Not `passive`: the install is passive, and the refusal prints the `kubectl patch` to run after `admin-finish` |
| `--model-namespace NS` | `install --scope cluster` | a namespace with model servers; repeat it. Add one later by running again with it |
| `--platform kubernetes\|openshift` | all | overrides the detection (API discovery of `security.openshift.io`) |
| `--files DIR` | `admin-setup`, `install`, `admin-finish` | install from release files in DIR instead of the image's. They must match their `SHA256SUMS` and be this image's release |
| `--image-repo REGISTRY/PATH` | `install` | for a mirror: the controller and preflight images from there, same digest. With `--scope cluster`, give `--weights-labeller-image` too, or no labeller is applied |
| `--weights-labeller-image IMG` | `install --scope cluster` | the image the weights labeller runs. Default: this release's tools image, `ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:<release>`, by tag (the tools image carries the release files, so its digest is known only after it is built). Pass `tools@sha256:<digest>` from the release notes to pin it |
| `--no-weights-labeller` | `install --scope cluster` | apply no weights labeller (one already running is left as it is) |
| `--dry-run --out DIR` | `admin-setup`, `install` | writes the objects in apply order, one file per step, and contacts no cluster |
| `--strict` | `install`, `admin-finish` | stop on a preflight `UNKNOWN` the controller does not answer. After `admin-finish` (the preflight binding removed), the checks its cluster reads made are refused and do not stop it |
| `--restart-keda` | `admin-finish`, `install --scope cluster` | restart KEDA's operator when it still holds a connection from before a reinstall. Without it, the command prints the restart |

If the settings already exist, `install` never writes them. A flag that
differs from the live value is reported with the `kubectl patch` that would
change it, and the install goes on. Two cases stop instead, before any
write, and print the patches to run (exit 2):

- A new URL of the other scheme (http to https or back): the patches for
  `prometheusURL` and `prometheusAllowHTTP`, and to http also those that
  clear `prometheusCACertPath`, `prometheusServerName` and
  `prometheusInsecureSkipVerify` when they are set. Run them, then the same
  command again right away: until then a controller restart fails, since the
  live Deployment still has the token setting of the old scheme.
- `--allow-http` while the live `prometheusAllowHTTP` is not `"true"`: the
  patch that sets it. Run the printed `kubectl patch`, then the same command.

## Exit codes

The same for every command.

| code | meaning | what to do |
| --- | --- | --- |
| 0 | done; `WARN` and `UNKNOWN` lines, if any, are in the summary | |
| 1 | the preflight has a `FAIL`: `install` stopped before the controller, or `admin-finish` kept the preflight binding | apply the fix printed under each `FAIL`, run again |
| 2 | usage: an unknown flag, a missing value, a bad namespace name; or release files that fail their `SHA256SUMS` or are another release; or an image without files and no `--files`; or live settings that `--prometheus-url` or `--allow-http` disagree with (a scheme switch, or `--allow-http` while `prometheusAllowHTTP` is not `"true"`) | fix the command; for the settings, run the printed `kubectl patch`, then the same command |
| 3 | needs the cluster admin: no stamp, or a stamp from other admin objects (an upgrade that changes them) | the admin runs the `smt admin-setup` line printed |
| 4 | the cluster is not ready, or needs a person: the namespace does not exist (or you may not read it), KEDA's namespace cannot be found or disagrees with the stamp, KEDA's side was refused (an object of its name this tool did not write, or one it cannot read; the line prints the `kubectl delete` and the command to run again), the controller was installed with `make`, or `admin-finish` ran before `install` | the line says which |
| 5 | timed out: the controller rollout, the client certificate, the preflight Job, or `scalerTLS: mutual TLS` | the line names the command that shows why |
| 6 | `--strict`: an `UNKNOWN` outside the checks that need the controller | make the check possible, or run without `--strict` |
| 7 | a `kubectl` call failed (refused, or the API server unreachable) | the line names the call |

## The preflight in `install`

| result | `install` does |
| --- | --- |
| `FAIL` | stops before the scaler TLS Secrets and the controller; exit 1. Settings this run created are removed, so the next run takes its flags again |
| `WARN` | goes on; prints each line again at the end |
| `UNKNOWN` of `rbac.controller`, `keda.mtls`, `scaler.tls` | goes on; the run after the install checks them |
| any other `UNKNOWN` | goes on and lists it as not checked; `--strict` stops (exit 6) |

`scaler.tls` stays `UNKNOWN` after the install: the scaler NetworkPolicy
admits KEDA only, so the Job cannot dial it. `scalerTLS` in
`scaling-manager-controller-status` shows `mutual TLS` instead. After
`admin-finish` the preflight's cluster reads are gone, so a later run reports
those checks `UNKNOWN`. See [preflight](preflight.md).

## Upgrade

Run `smt install` again with the new release's tools image. If the release
changed the admin objects, it exits 3 and prints the `smt admin-setup` line for
the admin. The settings and the scaler TLS Secrets stay as they are.

The stamp is a convenience, not a control: the namespace owner can edit it.
It catches an upgrade run before the admin's step.

An install made by hand from the release files is adopted; the first `smt
install` asks for one `smt admin-setup`. An install made with `make
deploy-scaling-manager` is not adopted yet (exit 4).

## GitOps and air-gapped clusters

`--dry-run --out DIR` writes the release files with the substitutions made
(`10-admin.yaml`, `20-settings.yaml`, ...), in apply order. It reads no
cluster, so it renders a fresh install. KEDA's side of mutual TLS is not
rendered: it copies the client certificate the controller makes.

Air-gapped: mirror the controller image (`skopeo copy --all` keeps the
digest) and the tools image, and pass `--image-repo`. Verify both with `cosign
verify` on a connected machine before mirroring; the signature does not travel
with `skopeo copy`.
