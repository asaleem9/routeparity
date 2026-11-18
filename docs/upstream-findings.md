# Upstream findings

These findings come from pinned controllers in local kind clusters with echo backends. The snapshots retain what the client received and what the backend received. They use synthetic manifests from this repository; no production traffic or credentials are involved.

## Capture-group rewrites: already tracked, current converter warns

Related upstream issue: [ingress2gateway #411](https://github.com/kubernetes-sigs/ingress2gateway/issues/411), open when checked on November 17, 2026. That report describes a `ReplaceFullPath` value with `$1` being forwarded literally on Istio. Gateway API full-path replacement does not perform regex-capture substitution; this is a translation limitation, not evidence of an Istio routing bug.

The current pinned ingress2gateway **1.2.0** behaves differently from the older issue's example: it explicitly warns that capture-group references are unsupported and omits the rewrite. With ingress-nginx **1.15.1** and Istio **1.32.0**, the [minimal fixture](../examples/capture-rewrite/ingress.yaml) produced:

| Request | ingress-nginx backend path | Istio backend path |
|---|---|---|
| `GET http://capture.example.com/items/x` | `/x` | `/items/x` |

Both proxies returned 200. The difference is visible only in the backend view. Of eight probes, seven differed, one matched, and none had transport errors. The report retains the converter warning as `not-converted`:

> Path rewrites with capture group references are not supported

Reproduce with a development build and local echo image:

```sh
make build echo-image
./bin/routeparity lab run -f examples/capture-rewrite/ --implementation istio \
  --save-baseline capture-baseline.json --save-candidate capture-istio.json
```

`TestIstioCaptureRewriteIsUnverified` checks the path difference and the explicit unverified accounting. No duplicate upstream issue is needed for the existing capture-rewrite limitation.

## Standard-emitter regex output is rejected by NGINX Gateway Fabric

With ingress2gateway **1.2.0**, ingress-nginx **1.15.1**, and NGINX Gateway Fabric **2.8.1**, the [minimal fixture](../examples/regex-portability/ingress.yaml) converts `/[A-Z]{3}` with `use-regex` to `(?i)/[A-Z]{3}.*`.

The NGINX Gateway Fabric HTTPRoute status reports `Accepted=False`, reason `UnsupportedValue`. Its message says the path must start with `/` and must not contain whitespace, `{`, `}` or `;`. `GET http://regex-portability.example.com/AAA` returns 200 through ingress-nginx and 404 through the Gateway. Of eight probes, seven differed, one matched, and none had transport errors.

This is a concrete portability gap for the standard emitter. The intended upstream request is a warning, compatibility documentation or an implementation-specific translation; it does not assume every Gateway must implement the same regular-expression syntax. A [ready-to-file report](upstream/ngf-standard-regex.md) includes the manifest and commands.

Before preparing the report, searches in ingress2gateway covered regex handling and NGINX Gateway Fabric. [#354](https://github.com/kubernetes-sigs/ingress2gateway/issues/354) discusses case-insensitive RE2 matching; [#421](https://github.com/kubernetes-sigs/ingress2gateway/issues/421) requests an NGINX emitter for other policies. Neither retrieved report describes this measured NGF regex rejection. The draft links both to let maintainers decide whether to consolidate it.

## Differences that are not new bug reports

Path normalization and default HSTS differ across the implementations. Istio's defaults preserve repeated slashes, whereas Envoy Gateway merges them. NGINX Gateway Fabric supplies the trailing-slash redirect in the literal fixture. The matrix records these observations with versions; a documented implementation choice alone is not treated as an upstream bug.
