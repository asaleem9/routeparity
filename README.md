# routeparity

Find out whether your Gateway API setup behaves like the ingress-nginx setup it replaces.

ingress-nginx was retired in March 2026, and a lot of clusters are moving to Gateway API. Converting the manifests is the easy part. ingress-nginx also does several things that appear nowhere in an Ingress, so a route can apply cleanly and still turn a request that used to return 200 into a 404. I wanted a tool that tells me where that will happen before my users do.

## Status

This is early, and it works end to end:

- `routeparity lab run` starts ingress-nginx and a Gateway implementation side by side on your machine, sends the same requests through both, and reports where they differ.
- `routeparity audit` reads your Ingress manifests and predicts where a Gateway will behave differently. It sends no traffic.
- `routeparity live` sends the same requests to two endpoints you already have and compares what each did. `record`, `diff` and `verify` do the same with a snapshot file in between, with JUnit and Markdown reports for CI.
- `routeparity lab record` saves how real ingress-nginx treats your Ingresses, so you still have a reference once the controller is gone.

The lab runs Envoy Gateway, NGINX Gateway Fabric, and Istio. Tagging v0.1.0 publishes signed binaries and the echo image. That tag is not cut yet. The [roadmap](#roadmap) has what's next.

## Install

There is no release tag yet. Build from this repo with Go 1.25.5 or newer:

```sh
git clone https://github.com/asaleem9/routeparity.git
cd routeparity
make build        # writes bin/routeparity
```

Pushing a `vX.Y.Z` tag publishes signed binaries for Linux and macOS, and the echo image `ghcr.io/asaleem9/routeparity-echo` at that tag. The checksum file is signed with cosign. After that tag, `go install github.com/asaleem9/routeparity/cmd/routeparity@latest` installs the release.

## Compare both on your laptop

This needs Docker. A binary whose version is `vX.Y.Z` pulls `ghcr.io/asaleem9/routeparity-echo` at that version. Any other build, including one from this checkout, looks for `routeparity-echo:dev`, so run `make echo-image` first. Nothing touches a cluster of yours, and your kubeconfig is neither read nor written.

```console
$ routeparity lab run -f examples/five-behaviours/
[  0s] starting kind cluster routeparity-lab (Kubernetes v1.35.8)
[ 28s] loaded the local image routeparity-echo:dev into the cluster
[ 28s] installing ingress-nginx v1.15.1
[ 30s] installing envoy-gateway v1.10.0 (Gateway API v1.6.1)
[ 50s] applying 6 Ingress(es) with 2 Service(s) replaced by the echo backend
[ 50s] waiting for ingress-nginx to load the configuration
[ 53s] sending 40 probe(s) to ingress-nginx
[ 54s] applying 1 Gateway(s) and 6 HTTPRoute(s) from github.com/kubernetes-sigs/ingress2gateway@v1.2.0 (standard emitter)
[ 54s] waiting for the envoy-gateway proxy
[ 66s] sending 40 probe(s) to envoy-gateway

routeparity diff  ingress-nginx (ingress-nginx, kind cluster routeparity-lab)  →  envoy-gateway (gateway-api, kind cluster routeparity-lab)
  40 probes · 23 match · 17 differ · 0 unrouted · 0 unverified · 0 failed

DIFF        GET https://path-normalization.example.com/uuid
            from the configuration  ·  Ingress/demo/path-normalization
            client.header.strict-transport-security  max-age=31536000; includeSubDomains  →  
            NGX-TLS-002  HTTPS responses carry an HSTS header
            On this HTTPS response the baseline sent max-age=31536000; includeSubDomains and the
            candidate sent none. ingress-nginx adds Strict-Transport-Security by default for a host
            with TLS. The Ingress does not mention the header, so a translation does not add it either.
            fix: Add the header on the routes attached to the HTTPS listener.

DIFF        GET http://regex-match.example.com//AAA
            NGX-NORM-001, double-slash  ·  Ingress/demo/regex-match
            backend.rawTarget  //AAA  →  /AAA
            Both sides routed this request. The baseline forwarded the path as the client sent it
            (//AAA) and the candidate forwarded /AAA. ingress-nginx matches on a normalised path and
            forwards the original.
            fix: Envoy Gateway merges slashes, resolves dot segments and percent-decodes the path
            before forwarding, and an HTTPRoute cannot restore the original. A backend that depends
            on the path as the client sent it has to accept the normalised path.
            written for envoy

...

Result: The candidate behaves differently from the baseline on 17 probes.
This is not a statement of parity: 9 features were not exercised at all.
```

With only `-f`, the Gateway side is what [ingress2gateway](https://github.com/kubernetes-sigs/ingress2gateway) produces for your Ingresses. Every Service is replaced by `routeparity-echo`, which reports the request it received, so the comparison covers what each proxy forwarded and not only what the client got back.

That run is the converter's output, measured. It says three things:

- The converter gets the hard parts right on Envoy Gateway. Case-insensitive regex matching, regex mode leaking across Ingresses, `rewrite-target`, the trailing-slash redirect and the redirect to HTTPS all match. Its own behaviour tests run on Istio, so this was not a given.
- Sixteen probes return the same status and differ at the backend. ingress-nginx forwards `//AAA` as sent; Envoy Gateway forwards `/AAA`. An application that logs, signs or routes on the raw path sees a change. No HTTPRoute puts the original path back, and the report says so.
- ingress-nginx adds an HSTS header to HTTPS responses and the Gateway does not. Nothing in the Ingress asks for that header, and the converter does not mention it. I found it by running this. The fix is a `ResponseHeaderModifier` on the HTTPS routes.

To test routes you wrote yourself, pass them with `--gateway`. The example below copies each Ingress path into an HTTPRoute match and adds nothing else, which is what a careful person does by hand:

```console
$ routeparity lab run -f examples/five-behaviours/ --gateway examples/five-behaviours-gateway/
...
  40 probes · 15 match · 25 differ · 0 unrouted · 0 unverified · 0 failed

DIFF        GET http://regex-match.example.com/hEADER
            NGX-REGEX-002, case-flip  ·  Ingress/demo/regex-match-other
            routed           routed  →  not routed
            client.status    200  →  404
            backend.reached  demo/httpbin:8000  →  not reached
            The baseline routed this request and the candidate did not. The request differs from the
            configured path only in letter case. This Ingress did not ask for regex matching. Another
            Ingress on the host did, and ingress-nginx then matches every path on the host that way.
            A literal Exact or PathPrefix match does not.
            fix: Match the path as a case-insensitive regular expression that also accepts longer paths.
            RegularExpression support and syntax are implementation-specific, so confirm your Gateway
            accepts the (?i) flag.
            written for envoy
                matches:
                - path:
                    type: RegularExpression
                    value: "(?i)/Header.*"

DIFF        GET http://trailing-slash.example.com/my-path
            NGX-SLASH-001, strip-slash  ·  Ingress/demo/trailing-slash
            routed           routed  →  not routed
            client.status    301  →  404
            client.location  http://trailing-slash.example.com/my-path/  →  
            The baseline redirected the slashless path to the path with the slash, and the candidate
            did not. ingress-nginx adds that redirect for an Exact or Prefix path that ends in a slash.
            Gateway API does not, so the redirect has to be written as a filter.
            fix: Add a rule that redirects the slashless path.

...
```

The paths written in the manifests all match, which is why that translation looks finished. The 25 differences are one step away from them. Each one says which behaviour caused it and, where a route can remove it, the change to make. `routeparity explain NGX-REGEX-002` is the same behaviour, read from the manifests before any request is sent.

A run also prints Coverage: each rule, whether the probes differed or matched, and which rules the manifests predicted but no request checked. A rule in that last group is listed under Not verified as well, and `--strict` exits 2.

`--keep` leaves the cluster running. The second run above took 35 seconds that way, against 67 for the first. `routeparity lab down` deletes it. `--emitter envoy-gateway` converts with that emitter instead, and `--junit`, `--markdown` and `--strict` work as they do for `verify`.

What the lab cannot stand in for is listed under "Not verified" in every report: [the list is below](#record-ingress-nginx-on-your-laptop).

## Compare Gateway implementations

Envoy Gateway is the default. Select another implementation with:

```sh
routeparity lab run -f examples/five-behaviours/ --implementation nginx-gateway-fabric
routeparity lab run -f examples/five-behaviours/ --implementation istio
```

The same `--gateway` option tests your own routes. The lab assigns its own GatewayClass and proxy provisioning settings; route policies are carried through. All three implementations install from embedded manifests, including their Gateway API CRDs. NGINX Gateway Fabric's control plane and each Gateway's proxy communicate with the upstream certificate generator's mTLS certificates; the lab needs no cert-manager installation.

The compatibility matrix measures all three implementations in two views: the literal HTTPRoutes in this repository, and ingress2gateway's standard-emitter output. A rejected route is kept in the report with its controller's reason. A rule with no baseline hits is marked **not exercised**.

In measurements recorded November 16–17, 2026, against ingress-nginx 1.15.1, each view sent 40 probes:

| Gateway configuration | Envoy Gateway 1.10.0 | NGINX Gateway Fabric 2.8.1 | Istio 1.32.0 |
|---|---|---|---|
| Literal HTTPRoutes | 15 match, 25 differ | 23 match, 17 differ | 14 match, 26 differ |
| ingress2gateway standard emitter | 23 match, 17 differ | 14 match, 26 differ | 22 match, 18 differ |

NGINX Gateway Fabric supplies the trailing-slash redirect and preserves the raw path on accepted routes. It rejects the converter's `(?i)` patterns, so more routes differ after conversion than in the literal translation. The report includes those rejected routes and their reasons; these counts do not establish parity. Matrix rows group probes by their generating rule, so rejected routes can affect several rows.

Istio resolves dot segments and decodes unreserved percent-encoded characters, but its defaults do not merge repeated slashes. A doubled slash that Envoy Gateway normalises can therefore miss the route on Istio. The [upstream findings](docs/upstream-findings.md) include minimal reproductions and existing issue links.

To reproduce the nightly workflow locally, after `make build echo-image`:

```sh
python3 hack/run_matrix.py envoy-gateway
python3 hack/run_matrix.py nginx-gateway-fabric
python3 hack/run_matrix.py istio
python3 hack/render_matrix.py matrix --output matrix/README.md
python3 hack/build_matrix_site.py matrix --output dist/matrix
```

Kept lab clusters are bound to their Gateway implementation and versions, because different Gateway API bundles cannot safely replace one another. To switch implementations, use a different `--name` or remove the old cluster with `lab down`.

Each matrix run creates and removes its own named lab cluster. `matrix/` holds the three-column Markdown report, full comparisons, and both sides' snapshots for each view. Nightly runs attach the files to the [Compatibility matrix workflow](https://github.com/asaleem9/routeparity/actions/workflows/matrix.yml) and print the table in the job summary. The same workflow builds an interactive GitHub Pages site with rule filters, request evidence, downloads and fourteen measured runs of history. The [site guide](docs/matrix-site.md) covers local preview and enabling Pages for this repository.

## Audit your manifests

Point `audit` at files, a directory, or stdin:

```sh
routeparity audit -f ingress.yaml
routeparity audit -f manifests/ -o json --fail-on high
kubectl get ingress,service -A -o yaml | routeparity audit -f -
```

Exporting from the cluster is the better input. It keeps each Ingress's `creationTimestamp`, and ingress-nginx uses that to decide which Ingress wins when two of them claim the same path.

Running it on the example in this repo:

```console
$ routeparity audit -f examples/five-behaviours/
routeparity audit  ingress class "nginx"
  6 Ingresses · 5 hosts · 6 paths
  converter warnings from github.com/kubernetes-sigs/ingress2gateway@v1.2.0 (standard emitter)

HIGH    NGX-CONVERT-001  The converter did not translate a feature
        Ingress/demo/admin
        ingress2gateway: Unsupported annotation nginx.ingress.kubernetes.io/auth-url
        because: this controls who can reach the backend; without it requests that were rejected are
        served

HIGH    NGX-REGEX-002  Regex mode applies to every path on the host
        regex-match.example.com /Header  ·  Ingress/demo/regex-match-other
        Exact path "/Header" is matched as a case-insensitive regex prefix, because another Ingress
        on this host enables regex mode. Translated literally it will stop matching requests that
        differ in case or have a longer path.
        because: Ingress/demo/regex-match sets use-regex
        fix: Match the path as a case-insensitive regular expression that also accepts longer paths.
        RegularExpression support and syntax are implementation-specific, so confirm your Gateway
        accepts the (?i) flag.
            matches:
            - path:
                type: RegularExpression
                value: "(?i)/Header.*"

...

Summary: 4 high · 8 medium · 1 low · 0 info
```

`routeparity explain` lists the rules, and `routeparity explain NGX-REGEX-002` describes one with its sources.

`--fail-on` sets the lowest severity that makes `audit` exit 1 (`medium` by default, `none` to never fail). `--emitter envoy-gateway` shows the converter's warnings for that emitter instead of the standard one. `routeparity audit --help` lists the rest.

## Compare two endpoints

`live` takes the same manifests and two addresses: the setup you have and the one replacing it.

```sh
routeparity live -f manifests/ --baseline 203.0.113.10:80 --candidate 203.0.113.20:80 --mutations
```

It builds requests from your Ingress rules, sends each one to both addresses with the Host header set, and compares the answers. Redirects are not followed, and each request is written to the socket as is, so `/a/../b` arrives as `/a/../b`.

This is a run against real ingress-nginx 1.15.1 and Envoy Gateway 1.10.0, with an Ingress that sets `use-regex` on the path `/[A-Z]{3}` and an HTTPRoute translated literally:

```console
$ routeparity live -f ingress.yaml --baseline 127.0.0.1:18080 --candidate 127.0.0.1:18081 --mutations
routeparity diff  baseline (ingress-nginx, 127.0.0.1:18080)  →  candidate (gateway-api, 127.0.0.1:18081)
  8 probes · 2 match · 6 differ · 0 unrouted · 0 unverified · 0 failed

DIFF        GET http://regex-match.example.com/%41AA
            NGX-NORM-001, percent-encoded  ·  Ingress/demo/regex-match
            backend.rawTarget  /%41AA  →  /AAA

DIFF        GET http://regex-match.example.com//AAA
            NGX-NORM-001, double-slash  ·  Ingress/demo/regex-match
            backend.rawTarget  //AAA  →  /AAA

DIFF        GET http://regex-match.example.com/aaa
            NGX-REGEX-001, case-flip  ·  Ingress/demo/regex-match
            routed           routed  →  not routed
            client.status    200  →  404
            backend.reached  demo/httpbin:8000  →  not reached

...

Result: The candidate behaves differently from the baseline on 6 probes.
```

Each difference names the rule that produced the request, so `routeparity explain NGX-REGEX-001` tells you what you're looking at.

The first two are the ones a status check misses. Both proxies return 200. ingress-nginx forwarded the path as the client sent it, and Envoy Gateway decoded and normalised it first. The `backend.*` lines appear because the backend in that cluster was `routeparity-echo`, which reports the request it received. Against your real services you get the client's side only, and the report says so.

### Without `--mutations`

By default `live` only requests the paths written in your manifests. That is the polite setting for a production endpoint, and it found nothing in the run above: 2 probes, 2 matches. The differences were all on requests one step away from a configured path, which is what `--mutations` adds: the path in different case, with an extra segment, with and without a trailing slash, with dot segments, a doubled slash, or a percent-encoded letter. They are GET requests, but a WAF may not like them, so it is opt-in.

`routeparity probes -f manifests/ --mutations` lists what would be sent without sending anything.

### Snapshots

`record` saves one endpoint's behaviour to a file, and `diff` compares two files:

```sh
routeparity record -f manifests/ --address 203.0.113.10:80 --name old --mutations -o old.json
routeparity record -f manifests/ --address 203.0.113.20:80 --name new --mutations -o new.json
routeparity diff old.json new.json
```

A snapshot of ingress-nginx is worth keeping. It is the reference for the behaviour you had, and it still works after the controller is gone.

`verify` checks an endpoint against a snapshot in one step:

```sh
routeparity verify old.json --address 203.0.113.20:80
```

The snapshot carries its own requests, so `verify` needs no manifests. It replays exactly what the baseline was asked, mutations included. Run it after the migration, after a controller upgrade, or on a schedule.

### In CI

```sh
routeparity verify old.json --address gateway.staging:80 \
  --samples 3 --strict \
  --junit report.xml --markdown "$GITHUB_STEP_SUMMARY"
```

- `--junit` writes one test case per probe. A difference is a failure. Anything unverified is skipped, or a failure with `--strict`, so the file agrees with the exit status.
- `--markdown` writes a summary with a table of differences, sized for a job summary or a pull request comment.
- `--samples 3` sends each request three times. An endpoint that answers the same request in more than one way is reported as `UNVERIFIED` with the count, instead of passing or failing on whichever answer came first. That is what keeps a flaky backend from turning into a flaky pipeline.
- `--strict` exits 2 when nothing differs but something could not be verified.

`-o junit` and `-o markdown` print the same reports to stdout.

### What counts as a difference

- Status code, and whether the request was routed at all. Each run also sends one request that nothing should route, so it knows what each proxy's "no route" answer looks like. An application's 404 and a proxy's 404 are told apart that way.
- Where a redirect points, after resolving it. `/app/` and `http://host:8080/app/` are the same redirect.
- `Set-Cookie` names and attributes, HSTS, `WWW-Authenticate` and the CORS response headers.
- With the echo backend: which Service received the request, and the method, path and Host it saw.

Other response headers, such as `Server`, differ between any two proxies. Those are counted and listed at the end of the report without failing the run. `--compare-header NAME` makes a header count, and `--ignore-header NAME` stops one from counting; an ignored header is still listed. Response bodies are compared only with `--compare-body`, because real backends put per-request data in them.

Three results are never reported as a match. If neither endpoint routes a path that comes from your manifests, the verdict is `UNROUTED`: the likely cause is a wrong address or routes that never loaded. If an endpoint doesn't answer, the verdict is `ERROR`, even when both fail the same way. If an endpoint gives different answers to the same request, the verdict is `UNVERIFIED`.

### Exit codes

| Code | `audit` | `live`, `diff` and `verify` |
|---|---|---|
| 0 | No finding at or above `--fail-on`. | Nothing differs. |
| 1 | A finding at or above `--fail-on`. | Something differs. |
| 2 | Not used. | With `--strict`: nothing differs, but something could not be verified. |
| 3 | The tool could not finish. | The tool could not finish. |

## Rules

| ID | Severity | What ingress-nginx does |
|---|---|---|
| `NGX-REGEX-001` | high | With `use-regex`, a path matches case-insensitively and only has to match the start of the request path. |
| `NGX-REGEX-002` | high | Regex mode applies to every path on the host, including Exact and Prefix paths from other Ingresses. |
| `NGX-REGEX-003` | high | `rewrite-target` turns regex mode on, with the same host-wide effect. |
| `NGX-SLASH-001` | medium | For an Exact or Prefix path ending in `/`, a request without the slash gets a 301 to the path with it. |
| `NGX-TLS-001` | medium | A host with TLS redirects plain HTTP to HTTPS with a 308, unless the Ingress sets `ssl-redirect: "false"`. |
| `NGX-TLS-002` | medium | HTTPS responses for a host with TLS carry `Strict-Transport-Security: max-age=31536000; includeSubDomains`. |
| `NGX-CONFLICT-001` | medium | When two Ingresses define the same host and path, the older Ingress wins. |
| `NGX-CONVERT-001` | varies | ingress2gateway reported an annotation or feature it could not translate. |
| `NGX-NORM-001` | low | Request paths are normalised (`.`, `..`, `//`) before matching. |

The regex, trailing-slash and normalisation rules come from the Kubernetes blog post [Before You Migrate: Five Surprising Ingress-NGINX Behaviors You Need to Know](https://kubernetes.io/blog/2026/02/27/ingress-nginx-before-you-migrate/). I don't take the post's word for it: `e2e/lab_test.go` reproduces each of the five on real ingress-nginx, and CI runs it on every change. `NGX-TLS-002` is not in the post. It turned up as a difference in a lab run, and the same test now checks it.

`NGX-CONVERT-001` runs [ingress2gateway](https://github.com/kubernetes-sigs/ingress2gateway) in process and reports its warnings, so the list of untranslated features always matches the converter version in the binary. Anything that controls access, such as `auth-url` or a source-range allow list, is raised to high, because losing it means serving requests that used to be rejected.

## What the audit can't tell you

Every finding is a prediction. The audit reads manifests, and three things that decide real behaviour are not in them:

- How your Gateway implementation matches regular expressions and normalises paths. Gateway API leaves both to the implementation, and Envoy-based gateways and NGINX Gateway Fabric disagree.
- Your controller's ConfigMap and flags, which change ingress-nginx's defaults.
- Whether the Gateway side already compensates. ingress2gateway 1.x translates the regex and trailing-slash behaviours itself; a hand-written HTTPRoute may not.

An audit with no findings does not mean the two setups match. Measuring that is what `live` is for.

## Record ingress-nginx on your laptop

ingress-nginx is retired, and at some point the one in your cluster goes away. `lab record` captures how it treats your Ingresses before that happens, on a kind cluster on your machine:

```console
$ routeparity lab record -f examples/five-behaviours/ -o ingress-nginx.json
[  0s] starting kind cluster routeparity-lab (Kubernetes v1.35.8)
[ 41s] loaded the local image routeparity-echo:dev into the cluster
[ 41s] installing ingress-nginx v1.15.1
[ 68s] applying 6 Ingress(es) with 2 Service(s) replaced by the echo backend
[ 69s] waiting for ingress-nginx to load the configuration
[ 71s] sending 40 probe(s)
[ 72s] deleting cluster routeparity-lab
recorded 40 observation(s) of ingress-nginx v1.15.1
2 thing(s) could not be reproduced and are listed in the snapshot as not verified:
  backend-dependent: nginx.ingress.kubernetes.io/auth-url (Ingress/demo/admin)
  unsupported: nginx.ingress.kubernetes.io/limit-rps (Ingress/demo/admin)
```

It applies your Ingresses as they are and replaces every Service they route to with `routeparity-echo`, so the snapshot holds what each backend received as well as what the client got. Then check a Gateway against it:

```sh
routeparity verify ingress-nginx.json --address gateway.staging:80
```

Things to know before you rely on it:

- It needs Docker running. It creates its own kind cluster and never reads or writes your kubeconfig. `--keep` leaves the cluster up, which cuts the next run to around ten seconds; `routeparity lab down` deletes it.
- It runs ingress-nginx 1.15.1, the final release, with default settings. If your controller is older or has a customised ConfigMap, its behaviour can differ from what the lab records.
- Annotations that need something the lab doesn't have are removed before applying and listed as not verified: external authentication, source-range allow lists, rate limits, snippets, and anything that makes the backend speak a protocol other than HTTP. The paths under those Ingresses are still recorded, without the removed behaviour.
- A binary that is not a `vX.Y.Z` release looks for `routeparity-echo:dev`. Build it with `make echo-image`. A release binary uses `ghcr.io/asaleem9/routeparity-echo` at the same version.
- Against a real Gateway, `verify` compares the client's side only. What the Gateway forwards to the backend is only visible with the echo behind it, which is what `lab run` sets up.

## What it doesn't do yet

- It speaks HTTP/1.1. No HTTP/2 and no gRPC.
- It sends GET requests. Behaviour that only shows on other methods or on request bodies, such as a body size limit or a CORS preflight, is not exercised.
- A path behind a weighted canary is requested ten times. If the answers vary, which they should, it is reported as `UNVERIFIED`. Checking that the split itself is the same on both sides needs a statistical comparison, which isn't there. Canaries selected by header or cookie aren't probed with that header or cookie yet.
- Patterns that only PCRE accepts, such as lookaheads, can't be turned into a matching path. They are listed as not verified.
- Where Envoy Gateway changes the path before forwarding it, no HTTPRoute puts the original path back. Applying the other printed fixes does not clear that difference.
- The lab runs Envoy Gateway 1.10.0, NGINX Gateway Fabric 2.8.1, and Istio 1.32.0. A result holds for the implementation and version measured. Fixes written for Envoy are labelled, and their snippets are left out for NGINX Gateway Fabric.
- For NGINX Gateway Fabric and Istio, each Gateway has its own proxy. The lab selects a proxy using route hostnames and parent references, checks its readiness, and records it separately. A request that could target multiple independent Gateways is refused before cluster creation; run those configurations separately.
- The lab probes HTTP and HTTPS on ports 80 and 443. Listeners on other ports are applied and not probed.

## Roadmap

1. Tag v0.1.0. Pushing that tag publishes the signed binaries and `ghcr.io/asaleem9/routeparity-echo`. An unreleased build still needs `make echo-image`.
2. Enable the prepared GitHub Pages deployment for the measured matrix.
3. Access-log replay, a GitHub Action, and HTML reports (weeks 10–11).

The complete [twelve-week plan](docs/roadmap.md) records the milestones. The [compatibility matrix workflow](.github/workflows/matrix.yml) measures Envoy Gateway, NGINX Gateway Fabric, and Istio nightly and retains reports and replayable snapshots.

## Development

```sh
make test      # unit, golden and matrix report tests
make e2e       # the lab against a real kind cluster; needs Docker, takes a few minutes
make lint      # golangci-lint
make golden    # regenerate golden files, then read the diff before committing
```

Each rule has a fixture under `internal/rules/testdata/<RULE-ID>/`. The comment at the top of the fixture says what the rule should and should not report, and `audit.golden.json` is the exact output.

## License

Apache-2.0. See [LICENSE](LICENSE).
