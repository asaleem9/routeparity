# Standard emitter: document or warn about regex output rejected by NGINX Gateway Fabric

Upstream report: [kubernetes-sigs/ingress2gateway #424](https://github.com/kubernetes-sigs/ingress2gateway/issues/424)

## What happened

Using ingress2gateway v1.2.0 with the ingress-nginx provider and standard emitter, the Ingress below produces a `RegularExpression` match with value `(?i)/[A-Z]{3}.*`. NGINX Gateway Fabric v2.8.1 rejects the HTTPRoute with `Accepted=False`, `UnsupportedValue`.

A request that ingress-nginx v1.15.1 routes to the backend (`GET /AAA`, Host `regex-portability.example.com`) then returns 404 on NGF. Both endpoints were measured with the same echo backend in an isolated local kind cluster, on Kubernetes v1.35.8. There were no transport errors.

## Minimal input

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: regex-portability
  namespace: demo
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
spec:
  ingressClassName: nginx
  rules:
  - host: regex-portability.example.com
    http:
      paths:
      - path: /[A-Z]{3}
        pathType: ImplementationSpecific
        backend:
          service:
            name: httpbin
            port:
              number: 80
```

Relevant converted match:

```yaml
matches:
- path:
    type: RegularExpression
    value: (?i)/[A-Z]{3}.*
```

NGF's rejection message says the path must start with `/` and must not include whitespace, `{`, `}` or `;`. The failure therefore affects the configured path itself, before considering case or extra path segments.

## Reproduction

From a routeparity checkout containing `examples/regex-portability/ingress.yaml`:

```sh
make build echo-image
./bin/routeparity lab run -f examples/regex-portability/ \
  --implementation nginx-gateway-fabric \
  --save-baseline regex-baseline.json --save-candidate regex-ngf.json
```

This installs the pinned controllers, converts with ingress2gateway v1.2.0 in process, and sends identical requests through both proxies. Expected tool exit is 1 because behavior differs. The run measures eight probes: seven differ, one matches. It retains the HTTPRoute rejection under Not verified.

## Requested behavior

Could the converter document the standard emitter's regex compatibility limits, or emit an actionable warning when using ingress-nginx case-insensitive regex paths without a compatible implementation-specific emitter? An NGF emitter or compatible policy-based translation would also address this, if supported by NGF.

I am not assuming Gateway API requires NGF to accept RE2 inline flags or that all regular-expression engines should behave identically. The problem is discovering the incompatibility before applying the converted routes.

Related reports checked: #354 (case-insensitive regex) and #421 (NGINX emitter). Happy to consolidate this reproduction there if that is a better fit.
