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
about 20 of them.

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
- If everything is current, it closes the issue.
- If it could not check a component (an API error, or a pin the inventory no
  longer finds), it updates the issue but does not close it, and the run
  fails.

## When the issue opens

For each component that is behind:

1. **Read the upstream release notes** between our pin and the latest release
   (the *compare* link in the report). Look for breaking changes: renamed
   flags, removed metrics, changed CRD versions, new defaults.
2. **Move the pin in one pull request**, every file the report lists for it.
   The next run reports any file you missed as **pins disagree**.
3. **Rerun what exercises the component**, and put the result in the pull
   request:

   | Component | Rerun |
   |---|---|
   | llm-d-router, gateway-api-inference-extension, Gateway API, llm-d-inference-sim, KEDA, LWS, Kueue | the e2e suites ([testing](testing.md)) |
   | FMA | the two-model warm-pool benchmark scenario |
   | vLLM, llm-d-benchmark, the modelservice chart, the benchmark router and sidecar | the benchmark scenario that pins it ([benchmark internals](benchmark-guide.md)) |
   | kube-prometheus-stack | an install with `DEPLOY_PROMETHEUS=true` |

Staying behind on purpose is fine. Some pins are held deliberately, for
example the routing sidecar and the modelservice chart move together. When
you hold one, say so in the issue, and in a comment at the pin if the reason
will last. The issue stays open while anything is behind.
