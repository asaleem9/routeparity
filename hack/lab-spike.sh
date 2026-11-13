#!/usr/bin/env bash
# Spike for the lab design: one kind cluster running ingress-nginx and Envoy
# Gateway side by side, with the echo backend behind both, to show that the
# same request can be sent through each and compared.
#
# It reproduces the first behaviour from
# https://kubernetes.io/blog/2026/02/27/ingress-nginx-before-you-migrate/ :
# with use-regex, the path /[A-Z]{3} also matches /uuid on ingress-nginx. A
# literal RegularExpression match on an Envoy-based gateway should not.
#
# Usage: hack/lab-spike.sh            # create, measure, delete
#        KEEP=1 hack/lab-spike.sh     # leave the cluster running afterwards
#
# Needs docker, kind, kubectl, curl and go. It uses its own kubeconfig and
# never touches ~/.kube/config.
set -euo pipefail

CLUSTER=${CLUSTER:-routeparity-spike}
# ingress-nginx v1.15.1, the final release, is tested on Kubernetes 1.31-1.35.
NODE_IMAGE=kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0
INGRESS_NGINX_REF=controller-v1.15.1
ENVOY_GATEWAY_VERSION=v1.10.0
ECHO_IMAGE=routeparity-echo:spike
NGINX_PORT=${NGINX_PORT:-18080}
GATEWAY_PORT=${GATEWAY_PORT:-18081}

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
export KUBECONFIG="$WORK/kubeconfig"
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] || continue
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true # reap it so the shell does not report the kill
  done
  if [ "${KEEP:-0}" = 1 ]; then
    echo "cluster kept: kind export kubeconfig --name $CLUSTER; delete with: kind delete cluster --name $CLUSTER"
  else
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n[%4ds] %s\n' "$SECONDS" "$*"; }

# wait_for DESCRIPTION DUMP-COMMAND -- KUBECTL-ARGS... runs a kubectl wait or
# rollout command and, if it times out, prints the state that explains why.
wait_for() {
  local what=$1 dump=$2
  shift 3
  if ! kubectl "$@" >/dev/null 2>&1; then
    echo "timed out waiting for $what; current state:" >&2
    eval "$dump" >&2 || true
    exit 1
  fi
}

step "build the echo image"
GOOS=linux GOARCH=$(go env GOARCH) CGO_ENABLED=0 go build -C "$ROOT" -trimpath -o "$WORK/routeparity-echo-$(go env GOARCH)" ./cmd/routeparity-echo
docker build -q -t "$ECHO_IMAGE" -f "$ROOT/hack/echo.Dockerfile" "$WORK" >/dev/null

step "create kind cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --wait 120s >/dev/null 2>&1
T_CLUSTER=$SECONDS
kind load docker-image "$ECHO_IMAGE" --name "$CLUSTER" >/dev/null 2>&1

step "install ingress-nginx ($INGRESS_NGINX_REF) and Envoy Gateway ($ENVOY_GATEWAY_VERSION)"
kubectl apply -f "https://raw.githubusercontent.com/kubernetes/ingress-nginx/$INGRESS_NGINX_REF/deploy/static/provider/baremetal/deploy.yaml" >/dev/null
kubectl apply --server-side -f "https://github.com/envoyproxy/gateway/releases/download/$ENVOY_GATEWAY_VERSION/install.yaml" >/dev/null
# The admission webhook races with its own certificate job and is not needed
# to measure routing.
kubectl delete validatingwebhookconfiguration ingress-nginx-admission --ignore-not-found >/dev/null
kubectl wait --for=condition=Established --timeout=60s \
  crd/envoyproxies.gateway.envoyproxy.io crd/gateways.gateway.networking.k8s.io crd/httproutes.gateway.networking.k8s.io >/dev/null

step "apply the backend, the Ingress and a literally translated HTTPRoute"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: demo
spec:
  selector:
    matchLabels: {app: echo}
  template:
    metadata:
      labels: {app: echo}
    spec:
      containers:
      - name: echo
        image: $ECHO_IMAGE
        imagePullPolicy: Never
        args: ["--listen", "demo/httpbin:8000=:8000"]
        env:
        - name: POD_NAME
          valueFrom: {fieldRef: {fieldPath: metadata.name}}
        readinessProbe:
          httpGet: {path: /healthz, port: 9090}
---
apiVersion: v1
kind: Service
metadata:
  name: httpbin
  namespace: demo
spec:
  selector: {app: echo}
  ports:
  - port: 8000
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: regex-match
  namespace: demo
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
spec:
  ingressClassName: nginx
  rules:
  - host: regex-match.example.com
    http:
      paths:
      - path: "/[A-Z]{3}"
        pathType: ImplementationSpecific
        backend:
          service: {name: httpbin, port: {number: 8000}}
---
# kind assigns no LoadBalancer addresses, and Envoy Gateway does not report a
# Gateway as Programmed until its proxy Service has one. A ClusterIP Service
# has an address immediately, and the proxy is reached by port-forward anyway.
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: lab
  namespace: envoy-gateway-system
spec:
  provider:
    type: Kubernetes
    kubernetes:
      envoyService:
        type: ClusterIP
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: eg
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: lab
    namespace: envoy-gateway-system
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: eg
  namespace: demo
spec:
  gatewayClassName: eg
  listeners:
  - name: http
    protocol: HTTP
    port: 80
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: regex-match
  namespace: demo
spec:
  parentRefs:
  - name: eg
  hostnames: ["regex-match.example.com"]
  rules:
  - matches:
    - path: {type: RegularExpression, value: "/[A-Z]{3}"}
    backendRefs:
    - name: httpbin
      port: 8000
EOF

step "wait for both data planes"
wait_for "the ingress-nginx controller" "kubectl -n ingress-nginx get pods -o wide" -- \
  -n ingress-nginx rollout status deploy/ingress-nginx-controller --timeout=300s
wait_for "the Envoy Gateway controller" "kubectl -n envoy-gateway-system get pods -o wide" -- \
  -n envoy-gateway-system rollout status deploy/envoy-gateway --timeout=300s
wait_for "the echo backend" "kubectl -n demo describe pods -l app=echo | tail -20" -- \
  -n demo rollout status deploy/echo --timeout=120s
wait_for "the Gateway to be programmed" \
  "kubectl -n demo get gateway eg -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{\"\\n\"}{end}'; kubectl -n envoy-gateway-system get pods,svc" -- \
  -n demo wait gateway/eg --for=condition=Programmed --timeout=240s
wait_for "the HTTPRoute to be accepted" "kubectl -n demo get httproute regex-match -o yaml | sed -n '/^status:/,\$p'" -- \
  -n demo wait httproute/regex-match --for=jsonpath='{.status.parents[0].conditions[?(@.type=="Accepted")].status}'=True --timeout=120s
T_READY=$SECONDS

ENVOY_SVC=$(kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=eg -o jsonpath='{.items[0].metadata.name}')
kubectl -n ingress-nginx port-forward --address 127.0.0.1 svc/ingress-nginx-controller "$NGINX_PORT:80" >/dev/null 2>&1 &
PIDS+=($!)
kubectl -n envoy-gateway-system port-forward --address 127.0.0.1 "svc/$ENVOY_SVC" "$GATEWAY_PORT:80" >/dev/null 2>&1 &
PIDS+=($!)

# probe PORT PATH prints "STATUS backend-saw=TARGET". --path-as-is stops curl
# from normalising the path before sending it.
probe() {
  local out status view
  out=$(curl -sS -o /dev/null -D - --path-as-is --max-time 5 -H 'Host: regex-match.example.com' "http://127.0.0.1:$1$2" 2>/dev/null || true)
  status=$(printf '%s' "$out" | awk 'NR==1 {print $2}')
  view=$(printf '%s' "$out" | awk 'tolower($1)=="routeparity-backend:" {print $2}' | tr -d '\r')
  if [ -n "$view" ]; then
    # base64url without padding -> JSON -> the target the backend received
    view=$(printf '%s' "$view" | python3 -I -c 'import sys,base64,json; s=sys.stdin.read().strip(); print(json.loads(base64.urlsafe_b64decode(s + "=" * (-len(s) % 4)))["rawTarget"])')
  fi
  printf '%s backend-saw=%s' "${status:-none}" "${view:--}"
}

step "wait for the routes to answer"
for port in "$NGINX_PORT" "$GATEWAY_PORT"; do
  for _ in $(seq 1 60); do
    case "$(probe "$port" /ABC)" in 200*) break ;; esac
    sleep 1
  done
done
T_ROUTED=$SECONDS

step "compare"
printf '%-22s %-34s %-34s\n' "request" "ingress-nginx" "envoy gateway"
for path in /ABC /uuid /abc/def /AB /a/../ABC //ABC; do
  printf '%-22s %-34s %-34s\n' "GET $path" "$(probe "$NGINX_PORT" "$path")" "$(probe "$GATEWAY_PORT" "$path")"
done

echo
echo "versions: $(kubectl version -o json | python3 -I -c 'import sys,json; print("kubernetes", json.load(sys.stdin)["serverVersion"]["gitVersion"])'), ingress-nginx $INGRESS_NGINX_REF, envoy gateway $ENVOY_GATEWAY_VERSION"
echo "timings: cluster up ${T_CLUSTER}s, controllers ready ${T_READY}s, routes answering ${T_ROUTED}s"
