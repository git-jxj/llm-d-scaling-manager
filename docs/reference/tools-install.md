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
  -v "$HOME/.smt/kubeconfig:/kube/config:ro" -v "$PWD:/work" "$TOOLS" "$@"; }
```

Run it from a directory that does not contain `~/.smt`. Each person uses
their own kubeconfig. The container cannot run kubeconfig `exec` plugins
(cloud CLIs are not in it): use a token kubeconfig.

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
| `smt admin-setup -n NS [--keda-namespace KNS]` | cluster admin, once per namespace | the `part=admin` objects, the preflight's RBAC, the model-namespace file, KEDA's namespace in the scaler NetworkPolicy, and a stamp (ConfigMap `scaling-manager-admin-setup`) |
| `smt install -n NS --prometheus-url URL` | namespace owner | the settings (created once), the preflight, the scaler TLS Secrets (created once), the controller, the preflight again |
| `smt admin-finish -n NS` | cluster admin, after `install` | KEDA's client certificate, a controller restart when it changed, waits for `mutual TLS`, the preflight, then removes the preflight's cluster reads |
| `smt install --scope cluster --keda-namespace KNS --prometheus-url URL --model-namespace NS` | cluster admin | all three, for one controller in `scaling-manager-system` |
| `smt release-files --out DIR` | anyone | writes the release files the image carries, to verify with cosign or apply by hand |

Every command can be run again, and again after a failure part way. A
re-run changes nothing that is already right: the settings and the scaler TLS
Secrets are never written over, and `admin-finish` restarts the controller
only when KEDA's side changed.

## Flags

| flag | commands | meaning |
| --- | --- | --- |
| `-n NS` | all but `release-files` | the namespace the model servers and the controller run in |
| `--keda-namespace KNS` | `admin-setup`, `install --scope cluster`, `admin-finish` | where KEDA runs. Default: the live scaler NetworkPolicy, else the namespace serving the external-metrics APIService, else (OpenShift) `openshift-keda` |
| `--prometheus-url URL` | `install` | sets `prometheusURL` when the settings are created. Required then, except on OpenShift (Thanos Querier) |
| `--prometheus-ca FILE` | `install` | a CA file under the mounted `/work`: creates or updates Secret `scaling-manager-prometheus-ca`, sets `prometheusCACertPath` on a first install, restarts the controller when it changed |
| `--quota "GPU=N[,GPU=N]"` | `install` | sets `limiter: quota` and `quotas` |
| `--set KEY=VALUE` | `install` | any other settings key, on a first install. Not `passive`: the install is passive |
| `--model-namespace NS` | `install --scope cluster` | a namespace with model servers; repeat it. Add one later by running again with it |
| `--platform kubernetes\|openshift` | all | overrides the detection (API discovery of `security.openshift.io`) |
| `--files DIR` | `admin-setup`, `install`, `admin-finish` | install from release files in DIR instead of the image's. They must match their `SHA256SUMS` and be this image's release |
| `--image-repo REGISTRY/PATH` | `install` | for a mirror: the controller and preflight images from there, same digest |
| `--dry-run --out DIR` | `admin-setup`, `install` | writes the objects in apply order, one file per step, and contacts no cluster |
| `--strict` | `install`, `admin-finish` | stop on a preflight `UNKNOWN` the controller does not answer |
| `--restart-keda` | `admin-finish`, `install --scope cluster` | restart KEDA's operator when it still holds a connection from before a reinstall. Without it, the command prints the restart |

If the settings already exist, `install` never writes them. A flag that
differs from the live value is reported with the `kubectl patch` that would
change it, and the install goes on.

## Exit codes

The same for every command.

| code | meaning | what to do |
| --- | --- | --- |
| 0 | done; `WARN` and `UNKNOWN` lines, if any, are in the summary | |
| 1 | the preflight has a `FAIL`: `install` stopped before the controller, or `admin-finish` kept the preflight binding | apply the fix printed under each `FAIL`, run again |
| 2 | usage: an unknown flag, a missing value, a bad namespace name; or release files that fail their `SHA256SUMS` or are another release; or an image without files and no `--files` | fix the command |
| 3 | needs the cluster admin: no stamp, or a stamp from other admin objects (an upgrade that changes them) | the admin runs the `smt admin-setup` line printed |
| 4 | the cluster is not ready: the namespace does not exist, KEDA's namespace cannot be found or disagrees with the stamp, KEDA's side could not be written, or the controller was installed with `make` | the line says which |
| 5 | timed out: the controller rollout, the client certificate, the preflight Job, or `scalerTLS: mutual TLS` | the line names the command that shows why |
| 6 | `--strict`: an `UNKNOWN` outside the checks that need the controller | make the check possible, or run without `--strict` |
| 7 | a `kubectl` call failed (refused, or the API server unreachable) | the line names the call |

## The preflight in `install`

| result | `install` does |
| --- | --- |
| `FAIL` | stops before the scaler TLS Secrets and the controller; exit 1 |
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
