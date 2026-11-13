# routeparity

Find out whether your Gateway API setup behaves like the ingress-nginx setup it replaces.

ingress-nginx was retired in March 2026, and a lot of clusters are moving to Gateway API. Converting the manifests is the easy part. ingress-nginx also does several things that appear nowhere in an Ingress, so a route can apply cleanly and still turn a request that used to return 200 into a 404. I wanted a tool that tells me where that will happen before my users do.

## Status

This is early. Two things work today:

- `routeparity audit` reads your Ingress manifests and predicts where a Gateway will behave differently. It sends no traffic.
- `routeparity live` sends the same requests to two endpoints and compares what each did. `record` and `diff` do the same in two steps, with a snapshot file in between.

What's missing is the part that sets up the comparison for you: a command that starts a local cluster with both proxies and stand-in backends. Today that is a script. The [roadmap](#roadmap) has the order.

## Install

```sh
go install github.com/asaleem9/routeparity/cmd/routeparity@latest
```

Or build from source with Go 1.25.5 or newer:

```sh
git clone https://github.com/asaleem9/routeparity.git
cd routeparity
make build        # writes bin/routeparity
```

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

Summary: 4 high · 7 medium · 1 low · 0 info
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

### What counts as a difference

- Status code, and whether the request was routed at all. Each run also sends one request that nothing should route, so it knows what each proxy's "no route" answer looks like. An application's 404 and a proxy's 404 are told apart that way.
- Where a redirect points, after resolving it. `/app/` and `http://host:8080/app/` are the same redirect.
- `Set-Cookie` names and attributes, HSTS, `WWW-Authenticate` and the CORS response headers.
- With the echo backend: which Service received the request, and the method, path and Host it saw.

Other response headers, such as `Server`, differ between any two proxies. Those are counted and listed at the end of the report without failing the run. Response bodies are compared only with `--compare-body`, because real backends put per-request data in them.

Two results are never reported as a match. If neither endpoint routes a path that comes from your manifests, the verdict is `UNROUTED`: the likely cause is a wrong address or routes that never loaded. If an endpoint doesn't answer, the verdict is `ERROR`, even when both fail the same way.

### Exit codes

| Code | `audit` | `live` and `diff` |
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
| `NGX-CONFLICT-001` | medium | When two Ingresses define the same host and path, the older Ingress wins. |
| `NGX-CONVERT-001` | varies | ingress2gateway reported an annotation or feature it could not translate. |
| `NGX-NORM-001` | low | Request paths are normalised (`.`, `..`, `//`) before matching. |

The regex, trailing-slash and normalisation rules come from the Kubernetes blog post [Before You Migrate: Five Surprising Ingress-NGINX Behaviors You Need to Know](https://kubernetes.io/blog/2026/02/27/ingress-nginx-before-you-migrate/).

`NGX-CONVERT-001` runs [ingress2gateway](https://github.com/kubernetes-sigs/ingress2gateway) in process and reports its warnings, so the list of untranslated features always matches the converter version in the binary. Anything that controls access, such as `auth-url` or a source-range allow list, is raised to high, because losing it means serving requests that used to be rejected.

## What the audit can't tell you

Every finding is a prediction. The audit reads manifests, and three things that decide real behaviour are not in them:

- How your Gateway implementation matches regular expressions and normalises paths. Gateway API leaves both to the implementation, and Envoy-based gateways and NGINX Gateway Fabric disagree.
- Your controller's ConfigMap and flags, which change ingress-nginx's defaults.
- Whether the Gateway side already compensates. ingress2gateway 1.x translates the regex and trailing-slash behaviours itself; a hand-written HTTPRoute may not.

An audit with no findings does not mean the two setups match. Measuring that is what `live` is for.

## Try it on a laptop

`hack/lab-spike.sh` starts a kind cluster with ingress-nginx and Envoy Gateway side by side and puts `routeparity-echo` behind both. It needs Docker, kind and kubectl, and takes about a minute once the images are cached.

```sh
KEEP=1 hack/lab-spike.sh     # prints a small comparison of its own, then leaves the cluster up
```

With the cluster up, port-forward the two proxies and point `live` at them. That is how the output above was produced. Delete the cluster with `kind delete cluster --name routeparity-spike`.

## What `live` doesn't do yet

- It speaks HTTP/1.1. No HTTP/2 and no gRPC.
- It sends GET requests. Behaviour that only shows on other methods or on request bodies, such as a body size limit or a CORS preflight, is not exercised.
- Weighted canaries are random per request, so they need many requests and a statistical comparison.
- Patterns that only PCRE accepts, such as lookaheads, can't be turned into a matching path. They are listed as not verified.
- It doesn't explain a difference beyond naming the rule. Fix suggestions are in `audit` for now.

## Roadmap

1. `verify`: check an endpoint against a saved snapshot in one step, with JUnit output for CI.
2. `lab`: do what the script does as a command, from your own manifests, with every backend replaced by the echo. Nothing touches production.
3. Explanations and fixes attached to each observed difference.
4. A published table of how Envoy Gateway, NGINX Gateway Fabric and Istio each differ from ingress-nginx, regenerated on a schedule.

## Development

```sh
make test      # unit and golden tests
make lint      # golangci-lint
make golden    # regenerate golden files, then read the diff before committing
```

Each rule has a fixture under `internal/rules/testdata/<RULE-ID>/`. The comment at the top of the fixture says what the rule should and should not report, and `audit.golden.json` is the exact output.

## License

Apache-2.0. See [LICENSE](LICENSE).
