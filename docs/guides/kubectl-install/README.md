# Install with kubectl, from one manifest (draft)

## Overview

**Draft.** This is phase 1 of the [kubectl install
proposal](https://github.com/llm-d-incubation/llm-d-scaling-manager/pull/404). No release carries
`scaling-manager.yaml` yet. It is tested on kind only (`make
test-e2e-manifest-install`). Install a real cluster from [the cluster-wide
guide](../install-cluster-wide/) until a release carries the manifest.

This guide installs one scaling manager for the cluster, the way KEDA, Kueue
and LeaderWorkerSet install: one signed manifest, applied with `kubectl apply
--server-side`. No docker and no checkout. A namespace opts in with a label.
The install is **passive**: it decides and publishes, and moves nothing.

What the controller does for you on this path:

- **Its own checks.** It runs the preflight checks itself, every minute. It
  writes each result to its status ConfigMap, plus `ready`. Each change is
  an Event. There is no preflight Job.
- **Its own TLS.** It fills the scaler's certificate Secrets, which the
  manifest creates empty. It does not need `create` on Secrets.
- **KEDA's copy.** It writes KEDA's client certificate into the one Secret
  the manifest creates in KEDA's namespace. No step for a person.
- **Settings.** When a setting that is read at start changes, the controller
  restarts itself.

## Prerequisites

- [ ] Cluster-admin rights.
- [ ] **KEDA** 2.18 or later, in namespace `keda`. Elsewhere, run the edit
  in step 2.
- [ ] A Prometheus with the Prometheus Operator, scraping the model servers.
- [ ] `kubectl`, and `cosign` to verify the download.

## Install

1. Download and verify the manifest:

<!-- guide:install.download start -->
```bash
# The manifest and its signature. Pick the release you install.
V=<release, v0.13.0 or later>
URL=https://github.com/llm-d-incubation/llm-d-scaling-manager/releases/download/$V
curl -fsSLO $URL/scaling-manager.yaml && curl -fsSLO $URL/scaling-manager.yaml.sigstore.json
cosign verify-blob scaling-manager.yaml --bundle scaling-manager.yaml.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/llm-d-incubation/llm-d-scaling-manager/.github/workflows/ci-release.yaml@refs/tags/$V"
```
<!-- guide:install.download end -->

2. If KEDA runs in another namespace than `keda`:

<!-- guide:install.kedans start -->
```bash
# Only when KEDA does not run in namespace keda. The file header gives the same edit. Repeat it on every upgrade.
KEDA_NS=<the namespace KEDA runs in>
sed -i -e "s/^\(  *namespace: \)keda\$/\1$KEDA_NS/" \
  -e "s/kubernetes.io\/metadata.name: keda\$/kubernetes.io\/metadata.name: $KEDA_NS/" \
  -e "s/--keda-namespace=keda\$/--keda-namespace=$KEDA_NS/" scaling-manager.yaml
```
<!-- guide:install.kedans end -->

3. Apply the manifest:

<!-- guide:install.manifest start -->
```bash
# Command 1. The namespace, the RBAC, the controller and KEDA's side.
kubectl apply --server-side -f scaling-manager.yaml
```
<!-- guide:install.manifest end -->

4. Write the settings. They are the existing settings ConfigMap. Keep them
   in git if you like, but never the switches `passive`,
   `utilizationShare` and `dsetRoleWriter`: those are set with `kubectl
   patch`.

   ```yaml
   # settings.yaml
   apiVersion: v1
   kind: ConfigMap
   metadata:
     name: scaling-manager-settings
     namespace: scaling-manager-system
   data:
     prometheusURL: https://prometheus.monitoring.svc:9090
     namespaceSelector: "scaling-manager.llmd.ai/managed=true,kubernetes.io/metadata.name in (team-a)"
   ```

   If a private CA signed Prometheus's certificate, put it in Secret
   `scaling-manager-prometheus-ca` (key `ca.crt`) in
   `scaling-manager-system`, and add `caFromSecret: "true"`.

<!-- guide:install.settings start -->
```bash
# Command 2. Write settings.yaml as shown above, then apply it.
kubectl apply --server-side -f settings.yaml
```
<!-- guide:install.settings end -->

   Until the settings name Prometheus, the controller cannot start, and
   the kubelet restarts it.

5. Opt a namespace in:

<!-- guide:install.label start -->
```bash
# Command 3. Opt a namespace in. The selector in the settings must admit it too.
kubectl label namespace <model namespace> scaling-manager.llmd.ai/managed=true
```
<!-- guide:install.label end -->

**The label alone is not consent.** Anyone who can create a namespace can
label it. `namespaceSelector` is what you narrow it with: name the
namespaces, or require a label only you set. The scaler refuses KEDA's calls
from every namespace the selector does not admit, and the status lists them
in `namespacesRefused`.

## Check it

<!-- guide:check.ready start -->
```bash
# ok once every check passes; a failing check's key says what to fix.
kubectl get configmap scaling-manager-controller-status -n scaling-manager-system \
  -o jsonpath='{.data.ready}{"\n"}{.data.namespacesManaged}{"\n"}{.data.namespacesRefused}{"\n"}{.data.scalerTLS}{"\n"}'
```
<!-- guide:check.ready end -->

`ready` is `ok` when no check fails. A WARN or an UNKNOWN does not block
it, but each one is listed:

<!-- guide:check.checks start -->
```bash
# Each preflight check, and the Events when one changes.
kubectl get configmap scaling-manager-controller-status -n scaling-manager-system -o json \
  | jq -r '.data | to_entries[] | select(.key | startswith("preflight.")) | "\(.key) \(.value)"'
kubectl get events -n scaling-manager-system --field-selector involvedObject.name=scaling-manager-controller-status
```
<!-- guide:check.checks end -->

`kubectl wait` cannot wait on ConfigMap data, so a script polls `ready`.

Then register your models with ScaledObjects, as on the other install
paths. They point at `scaling-manager-external-scaler.scaling-manager-system.svc:9090`
with `ClusterTriggerAuthentication` `scaling-manager-external-scaler-scaling-manager-system`.

## Upgrade

Apply the new release's manifest the same way. The settings and your
ScaledObjects are not in it, so they stay. The scaler certificates and
KEDA's copy are Secrets with no data in the file, so a re-apply keeps what
the controller wrote.

## Uninstall

<!-- guide:uninstall.all start -->
```bash
# Removes everything this install made, then checks nothing is left.
I=app.kubernetes.io/instance=scaling-manager-system
kubectl delete scaledobject -A -l "$I,app.kubernetes.io/managed-by=llm-d-scaling-manager"
kubectl label namespace -l scaling-manager.llmd.ai/managed=true scaling-manager.llmd.ai/managed-
kubectl delete -f scaling-manager.yaml
kubectl get clusterrole,clusterrolebinding,clustertriggerauthentication -l "$I"
```
<!-- guide:uninstall.all end -->

The settings go with the namespace.

## Not in this draft yet

- The suggested ScaledObjects (`scaling-manager-scaledobjects-plan`).
- `scaling-manager-grants.yaml` (pod writer, DisaggregatedSet role writer)
  and `scaling-manager-monitoring.yaml`.
- The conflict checks against another scaling manager.
- A move from an `smt` install. The scaler Secrets keep their names here
  but change type, which an existing install cannot take.
