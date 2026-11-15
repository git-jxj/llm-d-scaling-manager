# Install in two commands, from the tools image

**Status:** proposal, 2026-11-15. Nothing here is built.

**Audience:** contributors. Operators install today from
[the install guide](../guides/install-in-namespace/).

**Builds on:** [Install from the release](install-from-release.md) (the
release files, the settings ConfigMap, the preflight Job, the scaler TLS
bootstrap) and the release's tools image (`Dockerfile.tools`,
`deploy/scaling-manager-tools.sh`).

## The problem

The README's install block is about 20 commands: download seven files and
their bundles, check the sums, two `sed` passes, three admin applies, an edited
settings file, a preflight loop, the scaler TLS Secrets, the controller, the
KEDA certificate step, a second preflight run, and a cleanup. Each step is
simple; together they are easy to get wrong, and the order matters.

The tools image is already on the operator's machine for `workload-patch`,
`scaledobjects-plan` and `utilization-share`. It carries `kubectl`, `jq` and
`yq`. It can run the install too.

## What the operator runs

Once per machine, as today for the other tools:

```bash
V=<release>
kubectl config view --minify --flatten > smt.kubeconfig && chmod 600 smt.kubeconfig
smt() { docker run --rm --user "$(id -u):$(id -g)" \
  -v "$PWD/smt.kubeconfig:/kube/config:ro" -v "$PWD:/work" \
  ghcr.io/llm-d-incubation/llm-d-scaling-manager/tools:$V "$@"; }
```

Then:

```bash
# Cluster admin, once per namespace.
smt admin-setup -n team-a --keda-namespace keda

# Namespace owner.
smt install -n team-a --prometheus-url https://prometheus.example:9090 --quota "H200=16"

# Cluster admin, after the install: KEDA's client certificate, then drop the preflight's cluster reads.
smt admin-cleanup -n team-a
```

`install` ends by printing the third line, for the owner to pass on. Each
person runs with their own kubeconfig.

The release files and the full guide stay, for anyone who applies objects by
hand or through GitOps. The README shows these commands and links the guide.
The two-role split stays.

## The three commands

### `smt admin-setup -n NS --keda-namespace KNS`

1. Refuse a namespace that does not exist. It never creates one.
2. Find KEDA's namespace: the flag; else the live scaler NetworkPolicy's
   source namespace; else the namespace of the Service behind the
   `v1beta1.external.metrics.k8s.io` APIService, when it is KEDA's (the
   detection `make setup-prereqs` already uses). None found: stop and ask for
   the flag.
3. Write that namespace into the scaler NetworkPolicy, then apply the
   `part=admin` objects of the namespaced file.
4. Apply the preflight's `part=admin` objects (its account, Role, ClusterRole
   and bindings).
5. Apply the model-namespace file (PodMonitor, controller Role).
6. On OpenShift, label the namespace `openshift.io/user-monitoring=true`.
7. Write the stamp: ConfigMap `scaling-manager-admin-setup` in `NS`, holding
   the release and a hash of the admin objects as shipped
   ([Upgrade](#upgrade)).

### `smt install -n NS --prometheus-url URL [--quota "GPU=N"]`

1. Take the release files from the image ([Where the files come
   from](#where-the-files-come-from)) and check them against `SHA256SUMS`.
2. Substitute `__NAMESPACE__` and `__MODEL_NAMESPACE__`.
3. Check the admin stamp. Missing, or from other admin objects: stop, exit 3,
   and print the `smt admin-setup` line for the admin to run.
4. Create `scaling-manager-settings` from the flags, if it does not exist.
   If it exists, leave it alone ([Settings](#settings)).
5. With `--prometheus-ca FILE`: create or update Secret
   `scaling-manager-prometheus-ca`.
6. Run the preflight Job (delete, apply, wait, print). Any `FAIL`: stop,
   exit 1, nothing more is created ([Preflight results](#preflight-results)).
7. Create the scaler TLS Secrets if absent. Never apply them: that would
   empty Secrets the controller filled.
8. Apply the `part=controller` objects; wait for the rollout.
9. Run the preflight again, now that the controller exists, and print it.
10. Print what is left: the `smt admin-cleanup` line, and the command that
    shows `scalerTLS` turning to `mutual TLS`.

### `smt admin-cleanup -n NS`

1. Wait (bounded) until the controller has filled the client certificate
   Secret.
2. Copy it into KEDA's namespace, apply the ClusterTriggerAuthentication and
   marker from `scaling-manager-keda-auth.yaml`, restart the controller. The
   same steps `scalerTLSAction` prints, run from the code that makes them,
   not by parsing that text.
3. Wait for `scalerTLS: mutual TLS`. On a reinstall where KEDA keeps a stale
   connection, print the KEDA operator restart; `--restart-keda` runs it.
   It never restarts KEDA unasked: other workloads share it.
4. Run the preflight a last time.
5. Delete ClusterRoleBinding `scaling-manager-preflight-NS`. If the last run
   had a `FAIL`, keep it so the owner can run again, and print the delete
   command instead.

## Decisions

### Where the files come from

**Baked into the tools image.** Not fetched at install time.

| | baked in | fetched by the container |
| --- | --- | --- |
| files match the image | always, by construction | needs the tag lookup, and a re-tagged release can drift |
| air-gapped cluster | works: mirror one image | needs a mirror of GitHub release assets too |
| signature check | the image's signature covers the files; one `cosign verify` of the tools image, outside | needs cosign and the Sigstore trust root inside |
| image size | the YAML files and their bundles: small | none |

Why not cosign in the image:

- A verifier inside the artefact it verifies proves nothing: a tampered image
  would carry a tampered check. The trust step is verifying the image, and
  that happens outside it.
- Keyless verification needs the Sigstore trust root, fetched over the
  network or shipped in the image and left to go stale. Neither works
  air-gapped.
- cosign is a large Go binary in an image whose release scan blocks on any
  fixable CRITICAL. More surface, nothing gained.

So the image carries, at `/opt/scaling-manager-tools/release/`, the release
files, their `.sigstore.json` bundles and `SHA256SUMS`, byte-identical to the
GitHub release assets. Two more commands use them:

- `smt release-files --out DIR` writes them out, to verify with cosign or to
  apply by hand.
- `smt install --files DIR` installs from a directory instead: files
  downloaded and checked separately, or a development image that carries
  none. It checks `SHA256SUMS` there and refuses files whose
  `app.kubernetes.io/version` is not the image's own: the install logic and
  the files ship together.

**Release workflow change.** Today `release-files` renders after the tools
image is pushed. It moves before it: `release` (controller image, digest) →
render and sign (`id-token: write`, no `contents: write`) → `release-tools`
builds with `--build-context release=dist` → upload (`contents: write`
only). `Dockerfile.tools` copies from a `release` stage that defaults to an
empty `scratch` stage, so PR and development builds still build and carry no
files.

The tools tag is mutable. The README says to run `cosign verify` on the tools
image (the release run's summary already prints the command), or to pin it
by digest.

### Air-gapped clusters

Mirror two images: the controller (by digest: `skopeo copy --all` keeps it)
and the tools image. `--image-repo REGISTRY/PATH` rewrites the repository of
every pinned controller reference (controller and preflight Job) and keeps the
digest. The digest is the identity; a mirror that changes it is refused at
pull. KEDA and Prometheus are prerequisites and out of scope here.

### Settings

- Created once, from the flags: `--prometheus-url`, `--prometheus-ca`
  (sets `prometheusCACertPath`), `--quota` (sets `limiter: quota` and
  `quotas`), and `--set KEY=VALUE` for any other key. `passive` stays `"true"`.
- If `scaling-manager-settings` exists, it is never written. A flag that
  differs from the live value is reported with the `kubectl patch` that would
  change it; the install goes on.
- No settings and no `--prometheus-url`: stop before anything is created.
  On OpenShift the file's Thanos Querier URL is the default.

### Prometheus CA

`--prometheus-ca FILE` (a path under the mounted `/work`) creates or updates
Secret `scaling-manager-prometheus-ca` and, on first install, sets
`prometheusCACertPath`. On a later run with existing settings that lack the
key, it prints the patch. If the Secret changed, it restarts the controller,
since the key is read at start. The owner cannot read a Secret in
Prometheus's namespace, so copying the CA from there is not offered.

### Upgrade

Re-run `smt install` with the new release's tools image. Settings and scaler
TLS Secrets are untouched; only the `part=controller` objects and the
preflight Job change.

A new release may change the admin objects. The owner cannot read
ClusterRoles, and after `admin-cleanup` the preflight cannot either, so a
SubjectAccessReview is not available. The stamp answers it: `install`
compares the hash of the image's admin objects with the one `admin-setup`
wrote. Equal: go on. Different: stop, exit 3, and print the `smt
admin-setup` line with the new tag. A release that does not touch the admin
objects needs no admin.

An install made by hand from the release files is adopted: same names, the
settings and TLS Secrets exist and are kept. It has no stamp, so the first
`smt install` asks for one `smt admin-setup`. An install made by `make
deploy-scaling-manager` is adopted only once that path renders from the same
files (phase 6 of [install-from-release](install-from-release.md#phases)).
Until then `install` stops when the live controller Deployment lacks the
release labels, and names the migration section.

### Who runs what, with which rights

Each command uses the kubeconfig mounted at `/kube/config`; nothing is
shared between the two people.

| command | runs as | needs | reads cluster-wide |
| --- | --- | --- | --- |
| `admin-setup` | cluster admin | create ClusterRoles and bindings (they hold every right they grant), apply in `NS`, get the external-metrics APIService | the APIService and the NetworkPolicy |
| `install` | namespace owner | the stock `admin` ClusterRole bound in `NS`: ConfigMaps, Secrets, Deployments, Services, Jobs, `pods/log` | nothing; OpenShift is detected from API discovery, which any user may read |
| `admin-cleanup` | cluster admin | get the client certificate Secret in `NS`, write a Secret in KEDA's namespace, apply the ClusterTriggerAuthentication, restart the controller, delete the preflight binding | KEDA's namespace only |
| `--dry-run`, `release-files` | anyone | no kubeconfig | nothing |

The preflight still runs as its own ServiceAccount, never the caller's. The
container cannot run kubeconfig `exec` plugins (cloud CLIs are not in it):
use a token kubeconfig, as the tools already require.

### Idempotency and re-runs

Every command can be run again, and again after a failure part way.

| object | how it is written |
| --- | --- |
| `part=admin`, preflight RBAC, model-namespace, keda-auth | `kubectl apply` |
| settings, scaler TLS Secrets | created if absent, never applied |
| Prometheus CA Secret | applied when `--prometheus-ca` is given |
| preflight Job | deleted, then created, every run (its template is immutable) |
| controller | `kubectl apply`, then `rollout status` |
| admin stamp | applied by `admin-setup` only |

A second identical `install` changes no object: the e2e asserts each
`resourceVersion` is unchanged except the preflight Job's and its result.

### The KEDA namespace in the NetworkPolicy

`admin-setup` owns it: the policy is a `part=admin` object, so the owner
never applies it, and an upgrade by `install` cannot reset it. A re-run of
`admin-setup` without the flag keeps the live value (step 2 above). The `sed`
in today's guide disappears from this path.

### Preflight results

| result | `install` does |
| --- | --- |
| `FAIL` | stops before the scaler TLS Secrets and the controller; exit 1; prints each fix |
| `WARN` | goes on; prints each line again in the final summary |
| `UNKNOWN`, a check that needs the controller (`rbac.controller`, `keda.mtls`, `scaler.tls`) | goes on; it is checked by the run after the install |
| any other `UNKNOWN` | goes on and lists it as not checked; `--strict` stops instead |

`UNKNOWN` is never shown as a pass. `scaler.tls` stays `UNKNOWN` after the
install too (the NetworkPolicy keeps the Job out), and the summary says why.
After `admin-cleanup` the preflight's cluster reads are gone, so an upgrade's
preflight reports those checks as `UNKNOWN`; that is expected and does not
stop it.

### `--dry-run`

`smt install --dry-run --out DIR` (and the same on `admin-setup`) writes the
rendered objects in apply order, one file per step (`10-admin.yaml`,
`20-settings.yaml`, ...), and prints the list. It contacts no cluster and
needs no kubeconfig, so it renders as for a fresh install: settings from the
flags, TLS Secrets included. It does not run the preflight. This is the GitOps
path: the output is the release files with the substitutions made, and the
checks assert exactly that.

### Cluster-wide scope

`--scope cluster` installs `scaling-manager-cluster.yaml` in
`scaling-manager-system`. There, everything is cluster-scoped or in a
namespace the cluster admin owns, so there is no second person: one command
does it all.

```bash
smt install --scope cluster --keda-namespace keda \
  --prometheus-url https://prometheus.example:9090 --model-namespace team-a --model-namespace team-b
```

It creates `scaling-manager-system` if absent, runs the admin steps, the
install steps, and the `admin-cleanup` steps in that order. `--model-namespace`
(repeatable) applies the model-namespace file to each; adding one later is a
re-run with the extra flag. `admin-setup` and `admin-cleanup` refuse `--scope
cluster` and point here.

### OpenShift

Detected from API discovery (`security.openshift.io`); `--platform
kubernetes|openshift` overrides. It selects the `-openshift` files, defaults
KEDA's namespace to `openshift-keda` and the Prometheus URL to Thanos
Querier, and `admin-setup` sets the user-monitoring label. The OpenShift
files are not tested end to end today; `install` prints that line, and the
CI gate below does not cover them.

### Relation to `make deploy-scaling-manager`

Both stay supported. The new commands live in `deploy/lib/release_install.sh`,
sourced by `scaling-manager-tools.sh`, and reuse what exists rather than copy
it:

| piece | shared from |
| --- | --- |
| KEDA namespace detection | the function `make setup-prereqs` uses |
| KEDA client certificate copy, stale-connection check | `deploy/lib/scaler_tls.sh`, `deploy/lib/verify.sh` |
| flag or variable to settings key | one table and one writer; `make` merge-patches the keys it was given, `install` creates once |
| the objects | the release files, once `make` renders from them (install-from-release phase 6) |

`make` keeps what the tools path does not do: find Prometheus and its CA,
install KEDA and Prometheus on request, kind and the e2e targets.
`test/release-manifests/e2e.sh` stops repeating the guide's commands and
calls the same library, so the kubectl-only test and the tools test cannot
drift.

## Testing

The gate is a new CI job beside `e2e-tests-release-manifests`, in both
scopes, each on a fresh kind cluster:

1. Render the release files for the image the job built, and build the tools
   image with them (`--build-context release=dist`).
2. Install the prerequisites with the existing e2e setup.
3. **Namespace scope**, with two kubeconfigs: cluster admin for `admin-setup`
   and `admin-cleanup`; for `install`, a ServiceAccount token bound to the
   stock `admin` ClusterRole in the namespace and nothing else. That proves
   the owner's least privilege, not just that it works as admin.
4. **Cluster scope**: one `install --scope cluster` as cluster admin.
5. Each job then asserts:
   - preflight with no `FAIL`, and `scalerTLS: mutual TLS`;
   - the smoke suite with `ginkgo -v`, and more than zero specs run;
   - a second `install` changes no `resourceVersion` (beyond the preflight);
   - negative control: a wrong `--prometheus-url` exits 1, names
     `prometheus.reachable`, and leaves no controller Deployment;
   - `install` before `admin-setup` exits 3;
   - an upgrade: the hand install of the previous release's files, then
     `smt install`; settings unchanged.

Without a cluster, in `make lint-deploy-scripts` (`hack/check-*.sh`):

- `--dry-run` output equals the release files after `sed` substitution;
- an image without files refuses `install` unless `--files` is given;
- `--files` with a mismatched version or a bad sum is refused;
- `hack/check-tools-image.sh` checks the baked `SHA256SUMS`.

## Out of scope

- Uninstall. The guide's cleanup blocks stay; `smt uninstall` may follow.
- Going active: `scaling-manager-activate.yaml` stays a separate admin step.
- The optional files: Kueue, alerts, metrics reader, policy example.
- A namespaced controller that watches other namespaces (`watchNamespaces`).
- Installing KEDA, Prometheus or llm-d.
- Registering workloads: `scaledobjects-plan` and `-apply` already do it.
- Migration from installs made before v0.11.0.
- An OpenShift CI gate.

## Phases

1. Bake the files into the tools image; `release-files`, `install --files`,
   `--dry-run`; the release workflow reorder.
2. `admin-setup`, `install`, `admin-cleanup` in namespace scope; the e2e job
   in namespace scope.
3. `--scope cluster`; the e2e job in cluster scope.
4. README: the three lines and a link to the guide. The guides keep the
   by-hand path and gain the tools path first.

## Open questions

- **Baked, not fetched.** The approved shape had `install` fetch the files
  and verify their signatures. This proposal bakes them into the image and
  leaves signature checking to one `cosign verify` of the image, outside it
  ([why](#where-the-files-come-from)). Confirm the change.
- **The name `admin-cleanup`.** It does two things: gives KEDA its
  certificate and removes the preflight's cluster reads. "Cleanup" reads like
  uninstall. `admin-finish` says what it is; the approved name is kept here
  until decided.
