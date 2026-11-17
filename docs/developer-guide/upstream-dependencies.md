# Upstream dependencies

We pin the versions of the upstream projects we deploy and test against:
llm-d-router, FMA, vLLM, KEDA, Kueue and others. Once a month a workflow
checks every pin against the latest upstream release and opens an issue
when one has fallen behind.

## Check the pins yourself

```bash
hack/upstream-versions.sh            # print the report
hack/upstream-versions.sh --check    # also exit 1 if a pin is behind or pins disagree,
                                     # 2 if a component could not be checked
```

You need `curl`, `jq` and `yq` v4. Set `GITHUB_TOKEN` to any GitHub token
first: without one the API allows 60 requests an hour, and a single run uses
about 30 of them. A network failure or a rate limit stops the run at once: it
exits 2 and says why, and never reports a pin as current that it did not
check.

The report has one row per component: our pin, the latest stable release
(drafts and prereleases are skipped), how many releases and days we are behind,
and a link to the upstream changes. Below the table it lists every file each
pin lives in.

## What gets checked

[`hack/upstream-pins.yaml`](../../hack/upstream-pins.yaml) lists each
component, its GitHub repository, and where we pin it. It holds no versions:
the script reads them from the real pin locations, so the list cannot fall
out of date with them.

A component pinned in several files must carry one version in all of them.
If the files disagree, the report says **pins disagree** and names each file.

Some components have more than one entry on purpose. The benchmark scenarios
pin their own llm-d-router and vLLM, apart from the e2e, and the entry's
`note` says why. SGLang and guidellm are listed as tracked: we depend on them
but do not pin them, so the report shows their latest release for reference
and never fails on them.

The vLLM most of the repository runs is not pinned here at all: it is the
llm-d-benchmark default at the ref we pin (v0.26.0 at llm-d-benchmark
v0.7.8). The *vLLM (harness default)* row reads it from llm-d-benchmark at
that ref, so moving the llm-d-benchmark pin moves it.

If you add, move or rename a pin, update its entry in the same pull request.
`make lint-deploy-scripts` reads every location in the file and fails on one
it can no longer find.

## The monthly issue

[`.github/workflows/upstream-versions.yaml`](../../.github/workflows/upstream-versions.yaml)
runs on the 1st of each month, and from the Actions tab whenever you want. It
keeps a single issue, titled *Upstream components behind our pins* and
labelled `upstream-versions`:

- If anything is behind, it opens the issue, or replaces the body of the one
  already open. It never opens a second one.
- If everything is current or held (below), it closes the issue.
- If it could not check a component (an API error, or a pin the inventory no
  longer finds), it updates the issue but does not close it, and the run
  fails.

## When the issue opens

For each component that is behind:

1. **Read the upstream release notes** between our pin and the latest release
   (the *compare* link in the report). Look for breaking changes: renamed
   flags, removed metrics, changed CRD versions, new defaults.
2. **Move the pin in one pull request**, every file the report lists for it.
   The next run reports any file you missed as **pins disagree**. For KEDA,
   also move `KEDA_TESTED_VERSION` in `deploy/lib/scaler_runtime.sh` when the
   minor changes; it is a minor, not a release, so the report does not read it.
3. **Rerun what exercises the component**, and put the result in the pull
   request:

   | Component | Rerun |
   |---|---|
   | llm-d-router, gateway-api-inference-extension, Gateway API, llm-d-inference-sim, KEDA, LWS, Kueue, the kind node image | the e2e suites ([testing](testing.md)) |
   | a Go module (gateway-api-inference-extension, LWS, KEDA) | `make test`, then the e2e suites |
   | FMA | the two-model warm-pool benchmark scenario |
   | vLLM, llm-d-benchmark, the modelservice chart, the benchmark router and sidecar | the benchmark scenario that pins it ([benchmark internals](benchmark-guide.md)) |
   | kube-prometheus-stack | an install with `DEPLOY_PROMETHEUS=true` |

## Holding a pin back

Staying behind on purpose is fine; say so in the inventory, or the issue never
closes. Add a `hold` to the component in `hack/upstream-pins.yaml`:

```yaml
    hold:
      version: v0.4.16          # the pin you are holding
      reason: matched with the routing sidecar pin
      until: "2027-02-01"       # review date
```

Until the review date the report shows the component as *held* with the
reason, and it does not keep the issue open. After the date it is *behind*
again: renew the date or move the pin. If the pin moves and the hold does not,
the report says **hold is stale** and the run fails, so a hold cannot outlive
the pin it was written for. `make lint-deploy-scripts` checks that too.
