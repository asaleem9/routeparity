# Compatibility matrix site

The static site reads complete measured comparisons for Envoy Gateway, NGINX Gateway Fabric and Istio. It offers literal and converted route views, rule search and filtering, request-level deltas, controller versions, per-implementation recording times, unverified features and raw downloads. It contains no third-party scripts, fonts or analytics.

## Build and preview

From the repository root, with Go, Python 3, and Docker:

```sh
make build echo-image
python3 hack/run_matrix.py envoy-gateway
python3 hack/run_matrix.py nginx-gateway-fabric
python3 hack/run_matrix.py istio
python3 hack/build_matrix_site.py matrix --output dist/matrix --run-id local
python3 -m http.server 8769 --bind 127.0.0.1 --directory dist/matrix
```

Open `http://127.0.0.1:8769/`. Each implementation uses its own kind cluster, which the measurement script removes afterwards. The builder refuses a missing column, transport failures, mismatched source commits or missing replayable snapshots. It never substitutes an older result for a failed current measurement.

`--previous PATH` imports an earlier built site. The current run and the thirteen preceding complete runs are retained by default; `--keep N` changes that count. Historical results keep their original versions, recording times, comparisons and snapshots. Each run is immutable under `history/<run-id>/`; the selector reads the index in `runs.json`.

## GitHub Pages

The repository must use **Settings → Pages → Build and deployment → Source: GitHub Actions**. Pages was not enabled when checked on November 18, 2026. Enabling it is a repository setting change; building the site locally does not publish it.

Once the workflow is on the default branch and Pages is enabled, run **Compatibility matrix** manually, or wait for its nightly schedule. The workflow:

1. Measures all three implementations in separate jobs, retaining comparisons and both snapshots for both route sources.
2. Builds Markdown and the static site only after all measurement jobs succeed.
3. Downloads the previous successful default-branch site artifact to retain run history. The first run starts with one entry.
4. Uploads a reviewable `compatibility-matrix-site` artifact and a Pages artifact.
5. Deploys the Pages artifact from `main` with the `github-pages` environment. Pull requests and tag-only runs retain artifacts without deploying.

The deployment job has `pages: write` and `id-token: write`; measurement jobs retain read-only repository permissions. History retrieval uses `actions: read`. A failed measurement or build leaves the last published site in place. History is recovered from retained Actions artifacts, which currently have ninety-day retention.

The expected project URL after successful deployment is `https://asaleem9.github.io/routeparity/`. The workflow's deployment output is the authoritative URL.

## Interpretation

Rows group requests by the rule that generated them. Rejected routes can make several rows differ; this does not isolate each behavior independently. Request evidence shows which fields changed. A rule with no baseline hits is not exercised, and unverified features remain visible even when the observed requests match.
