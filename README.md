# routeparity

Find out whether your Gateway API setup behaves like the ingress-nginx setup it replaces.

ingress-nginx was retired in March 2026, and a lot of clusters are moving to Gateway API. Converting the manifests is the easy part. ingress-nginx also does several things that appear nowhere in an Ingress, so a route can apply cleanly and still turn a request that used to return 200 into a 404. I wanted a tool that tells me where that will happen before my users do.

## Status

This is early. One command works today:

- `routeparity audit` reads your Ingress manifests and predicts where a Gateway will behave differently. It sends no traffic.

The part I'm building toward is the comparison itself: send the same requests through both setups, diff what the client saw and what the backend received, and name the cause of each difference. That is not built yet. The [roadmap](#roadmap) has the order.

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

## Usage

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

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `-f, --filename` | required | File, directory, or `-` for stdin. Repeatable. |
| `--ingress-class` | `nginx` | Audit Ingresses of this class. |
| `--include-classless` | off | Also audit Ingresses that name no class. |
| `--emitter` | `standard` | ingress2gateway emitter whose warnings to include (`standard`, `envoy-gateway`). |
| `--no-convert` | off | Skip running ingress2gateway. |
| `-o, --output` | `text` | `text` or `json`. |
| `--fail-on` | `medium` | Lowest severity that makes the command exit 1: `high`, `medium`, `low`, `info`, or `none`. |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | No finding at or above `--fail-on`. |
| 1 | At least one finding at or above `--fail-on`. |
| 3 | The tool could not finish, for example because a file was unreadable. |

Code 2 is reserved for the comparison commands.

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

An audit with no findings does not mean the two setups match. Measuring that is what the comparison commands are for.

## A preview of the comparison

`hack/lab-spike.sh` is a script version of where this is going. It starts a kind cluster with ingress-nginx and Envoy Gateway side by side, puts the same echo backend behind both, and sends each request through each proxy. The Ingress uses `use-regex` with the path `/[A-Z]{3}`, and the HTTPRoute is the literal translation. It takes about a minute once the images are cached.

```console
$ hack/lab-spike.sh
request                ingress-nginx                      envoy gateway
GET /ABC               200 backend-saw=/ABC               200 backend-saw=/ABC
GET /uuid              200 backend-saw=/uuid              404 backend-saw=-
GET /abc/def           200 backend-saw=/abc/def           404 backend-saw=-
GET /AB                404 backend-saw=-                  404 backend-saw=-
GET /a/../ABC          200 backend-saw=/a/../ABC          200 backend-saw=/ABC
GET //ABC              200 backend-saw=//ABC              200 backend-saw=/ABC

versions: kubernetes v1.35.8, ingress-nginx controller-v1.15.1, envoy gateway v1.10.0
```

`/uuid` and `/abc/def` are `NGX-REGEX-001` happening for real: ingress-nginx serves them and the Gateway returns 404.

The last two rows are why the backend's view matters. Both proxies answer 200, so a comparison of status codes calls them a match. The backend disagrees: ingress-nginx forwarded the path as the client sent it, and Envoy Gateway forwarded the normalised one. An application that signs or logs the raw path would see the difference.

## Roadmap

1. `live`: send the same requests to two reachable endpoints and diff the responses.
2. `record` and `verify`: save one setup's behaviour to a file and check another setup, or a later version of the same one, against it.
3. `lab`: start a local kind cluster with ingress-nginx and a Gateway implementation, replace every backend with an echo server, and diff what each backend received. Nothing touches production.
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
