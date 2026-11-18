# Keeping replicas off nodes whose weights are not there yet

- **Status:** built (`deploy/weights.sh label` and `labeller`, the verifying
  downloader, and the gate in `make workload-patch`). Not yet run on a cluster.
- **Problem:** with [weights on the node's disk](../reference/workload-preparation.md#weights-on-the-nodes-disk),
  each node holds its own copy. Nothing stops the scheduler from placing a
  replica on a node whose copy is missing, still being written, or gone. The
  replica then reads a partial model, or fails in a way that looks like an
  engine bug.
- **Decision:** the downloader verifies its files; a labeller run by the
  cluster admin sets `llm-d.ai/<model>-cache=ready` on each node whose copy
  verifies and removes it when the copy breaks; the workload patch adds a
  **preferred** node affinity on that label (required is opt-in) and an init
  container that waits for the copy.

How to use it: [Gate replicas on the weights](../reference/workload-preparation.md#gate-replicas-on-the-weights).

## 1. Roll-out tooling and scaling tooling want different things

Some clusters already prepare nodes this way. A node-preparation DaemonSet syncs
the models to each node's NVMe and writes a `.ready` file when every model has
synced. A separate step checks the files and labels the node
`llm-d.ai/<model>-cache=ready`. Each namespace gets a read-only `local`
PersistentVolume with node affinity on that label, and each model server
requires the label and waits for `config.json` in an init container.

That design is for **roll-out**: a fixed set of nodes is prepared once, then
the model is deployed onto them. The label is set by a person or a job after the
copies are complete, and nothing removes it. Those clusters learned the cost of
that: a disk failed silently, and nodes still labelled `ready` had empty
caches.

The scaling manager adds and removes replicas all day, and a scale-out may land
on a node that joined the cluster minutes ago. So for **scaling**:

| | Roll-out | Scaling |
| --- | --- | --- |
| Who sets the label | a person or a job, once | the labeller, from the files, continuously |
| Who removes it | nobody | the labeller, when the copy stops verifying or its downloader is gone |
| A new node | prepared before the model is deployed | gets the label only when its own download completes |
| Affinity | required: there are always enough prepared nodes | preferred by default: required can strand a scale-out |

We reuse the label key, so both kinds of tooling agree on one node label and a
node prepared by either counts for both.

## 2. Design

### 2.1 The downloader verifies the files

`deploy/weights.sh apply` already runs one downloader pod per GPU node. It now:

- writes `.download-complete` after the last file, atomically, listing each
  file with its size;
- has a readiness probe that checks the marker, a `config.json` (or
  `params.json` for a Mistral-format checkpoint), and every listed file at its
  size, every 15 s, failing only after three misses;
- re-checks every 30 s, and when the copy breaks removes the marker and
  exits, so the kubelet restarts it and it downloads again. The marker goes
  first because the init container of a replica waits only for the marker and
  a config: it does not check the sizes;
- upgrades an empty marker from an earlier version in place, but only when the
  files still look whole (a config, and every shard the index names); else it
  downloads again.

A disk that fails or is remounted empty takes the marker with it, so the probe
fails. That is the case the roll-out lesson was about.

### 2.2 The labeller derives the label from the files

`weights.sh label` reads the downloader pods. For each node and each label key:

- **set** `<key>=ready` when at least one downloader for that key has a pod on
  the node and every such downloader's pod there is Ready (two namespaces may
  share a key; both must be complete);
- **remove** it when this command set it and that no longer holds, including
  when the downloader is deleted;
- **never touch** a label it did not set. It records what it set in the node
  annotation `scaling-manager.llmd.ai/weights-node-labels`. If another tool's
  label disagrees with the downloader, `label` reports it and changes nothing.

The files are read through the kubelet (the probe), not by the labeller. That
keeps the labeller off the node's disk: no hostPath, Pod Security `restricted`,
and it works on OpenShift, where files carry their project's SELinux level and
another namespace could not read them.

`weights.sh labeller` runs `label --watch 30` in the cluster, from the tools
image.

### 2.3 The label key

`llm-d.ai/<name>-cache`, where `<name>` is the Hugging Face id without its
organisation, lower-cased: `meta-llama/Llama-3.1-8B-Instruct` becomes
`llm-d.ai/llama-3.1-8b-instruct-cache`: the shape those node-preparation jobs
use (dots kept). A label name allows
letters, digits, `-`, `_` and `.`, at most 63 characters, starting and ending
with a letter or digit, so:

- any other character becomes `-`, and the ends are trimmed;
- a name longer than 57 characters (63 minus `-cache`) keeps its first 48 and
  an 8-character hash of the whole id, so two long ids do not share a key.

`--node-label` on `apply` (`WEIGHTS_NODE_LABEL`) replaces the key, for a cluster
whose existing labels name the model differently. It must have the form
`<prefix>/<name>-cache`. `apply` records the key on the claim and the
DaemonSet. The labeller writes only keys of that form under its
`--label-prefix` (`llm-d.ai` by default): a model namespace can edit its own
DaemonSet, and must not be able to steer the labeller onto `node-role.*` or a
GPU product label. Within the prefix, the namespaces in scope are trusted:
a pod there with a downloader's labels can set or clear a `-cache` label on
its node. That is why the scope is a list of model namespaces, and `-A` is
opt-in. Dropping the organisation means two models of one name share a key;
`label` warns when it sees that.

### 2.4 Required or preferred affinity

Required affinity is the safe choice for roll-out and the risky one for
scaling. Early in a scale-out few nodes may be labelled. With required
affinity, every replica beyond their free GPUs stays `Pending`, even though the
downloader on the other nodes would finish in minutes.

So the patch writes **preferred** (weight 100) by default. Labelled nodes come
first. A replica that lands on an unlabelled node waits in `wait-for-weights`
until that node's download finishes, which is bounded because the downloader
runs on every GPU node; after `LLMSM_WEIGHTS_WAIT_TIMEOUT` (60 minutes by
default) it exits 1, so a node that never gets a copy shows as a failing pod
instead of a silent wait. `LLMSM_WEIGHTS_NODE_AFFINITY=required` is for models
whose download takes longer than an acceptable start, or clusters where some
GPU nodes never get a copy. `none` writes only the init container.

The init container waits for the marker, not only `config.json`: the
downloader may fetch `config.json` before the shards, so its presence does not
mean the copy is complete. It runs the engine's own image, which is on the node
already and passes whatever image policy the engine passes. A separate small
image would be one more pull, and one more image for an admission policy to
refuse.

### 2.5 How the scaling manager counts nodes without the weights

- **Required:** a node without the label is not capacity for that model. Node
  fit already reads the template's required node affinity when `gpu-inventory`
  is declared ([GPU limiter](../reference/gpu-limiter.md)), so nothing changes
  in the controller.
- **Preferred:** a node without the label is capacity, with a slower start. The
  start-time model does not yet know that a replica there pays the download.
  That is the main reason not to make `preferred` look free: for a very large
  model, use `required`.

### 2.6 Rights

Labelling a node needs `patch` on nodes. RBAC cannot narrow that to one label,
so it is a cluster-admin piece, like `weights apply` (which creates
PersistentVolumes):

- `weights.sh label`, run by the admin with their own credentials; or
- `weights.sh labeller`, which creates a namespace of its own
  (`scaling-manager-weights-labeller`, Pod Security `restricted`), a
  ServiceAccount with node `get`/`list`/`patch`, namespace `get`, and read-only DaemonSets and
  pods in the model namespaces it watches (a RoleBinding per namespace, or
  cluster-wide with `-A`), and its status ConfigMap in its own namespace. Every object carries `scaling-manager.llmd.ai/part:
  admin`, like the install's admin objects.

It must not run in a model namespace: anyone with `pods/create` there could
run a pod as its ServiceAccount and patch nodes. `labeller` refuses a
`--labeller-namespace` that is also in its scope. The namespace owner needs no
new rights.

Where it is installed:

- `smt install --scope cluster` applies and updates it. It watches every
  `--model-namespace`, plus every namespace the running labeller already
  watches, so a re-run never drops one. It applies a new one only when a model
  namespace has a weights DaemonSet; otherwise it prints how to add it.
  `--no-weights-labeller` leaves it out; `--weights-labeller-image` names the
  image (default: the release's tools image, by tag: the tools image carries
  the release files, so its digest is known only after it is built).
- `smt admin-setup -n NS` (one namespace) checks that a labeller runs, and adds
  NS to its watch list: a RoleBinding in NS and the labeller's Deployment
  re-applied with NS in its list. Both are namespaced writes, in NS and the
  labeller's namespace, so no cluster-wide right is used. With no labeller, or
  without those rights, it prints the command and still succeeds.
- `weights.sh labeller` stays the standalone step, for a cluster without the
  cluster-wide install.
- `weights.sh labeller --remove -n NS` takes one namespace off (the same two
  namespaced writes); `--delete` removes it for the whole cluster, as does the
  uninstall of a cluster-wide install.
- Its health is ConfigMap `weights-labeller-status` in its own namespace,
  written when the state changes.

## 3. Failure modes

| What happens | What the gate does |
| --- | --- |
| A node joins during a scale-out | its downloader starts; no label until its copy verifies; preferred: a replica may land there and waits; required: it does not land there |
| A copy is half-written | no marker, so not Ready, no label; the init container waits |
| The disk fails, or is remounted empty | the probe fails within ~45 s; the labeller removes the label on its next pass (30 s); the downloader downloads again |
| A file is truncated or deleted | its size check fails; the same as above |
| The downloader is deleted (`weights delete`) | no downloader pod on the node: the label goes |
| The labeller is down | labels freeze as they are; a broken copy keeps its label until it is back. The probe and the init container still protect new replicas on nodes that never had a copy |
| Another tool set the label | left alone; a disagreement with the downloader is reported |
| The node list is Forbidden | the pass fails and changes nothing; ConfigMap `weights-labeller-status` says `failed` |
| A watched namespace cannot be read (Forbidden) | the labels it asked for when last read are held as they are, the other namespaces are labelled as usual; not read since the labeller started, no label is removed anywhere. The status says `degraded` and names it |
| A downloader from before v0.12.0 (it records no node label; its probe checked the marker only) | skipped, with a warning; `weights apply` again |
| A watched namespace is deleted | it is left out of each pass, so the labels only its downloaders held go; the next `smt install --scope cluster` drops it from the list |
| A replica waits on a node the downloader never reaches | after `LLMSM_WEIGHTS_WAIT_TIMEOUT` (60 min) the init container exits 1 with the reason; the pod shows `Init:CrashLoopBackOff` on that node. Each restart waits the full timeout again, plus the kubelet's back-off; the pod is not rescheduled. Use `required`, or give the model server and the downloader the same node selector |
| A file is truncated under a marker while a replica starts | the init container checks the marker and a config only, so it passes; the probe fails within 15 s (the label goes) and the downloader's re-check removes the marker |
| A replica is running when its node's copy breaks | it keeps running; only new replicas avoid the node |

## 4. Not built

- A LeaderWorkerSet gets no gate yet: its patch is a JSON patch per template,
  and the leader and workers may read different claims. `workload-patch` warns
  for each template that reads a node-local weights claim.
- The gate is emitted, never applied by `workload-patch --apply`: placement is
  the owner's decision.
- No admission policy narrows the labeller's node `patch` to its own labels. A
  ValidatingAdmissionPolicy could; not written until it can be tested on a
  cluster.
- The start-time model does not charge a preferred placement for the download.
