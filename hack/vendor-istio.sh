#!/usr/bin/env bash
# Re-render the pinned Istio install bundle. Helm is a development tool only.
set -euo pipefail
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
curl -fsSL https://blob.istio.io/istio-release/charts/base-1.32.0.tgz -o "$work/base.tgz"
curl -fsSL https://blob.istio.io/istio-release/charts/istiod-1.32.0.tgz -o "$work/istiod.tgz"
curl -fsSL https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.1/experimental-install.yaml -o "$work/gateway-api.yaml"
python3 - "$work" <<'PY'
import hashlib
from pathlib import Path
import sys
root = Path(sys.argv[1])
for name, expected in {
    'base.tgz': '505ce0d9b6cf5d73a9998728fdf0821510a237470567e6c332164937c6850f05',
    'istiod.tgz': '17d275e3661e2e943a1ab630386bacab0cda76ea007978351a3a10fe9f4050d4',
    'gateway-api.yaml': 'd7fa77650e4ef28fca0411536fcb5e237deb4d50301cfded3be49d9a1b7bbd02',
}.items():
    if hashlib.sha256((root / name).read_bytes()).hexdigest() != expected:
        raise SystemExit('checksum mismatch: ' + name)
PY
helm template istio-base "$work/base.tgz" --namespace istio-system --kube-version 1.35.8 --include-crds --set defaultRevision=default > "$work/base.yaml"
helm template istiod "$work/istiod.tgz" --namespace istio-system --kube-version 1.35.8 --set global.hub=docker.io/istio --set global.tag=1.32.0 --set meshConfig.accessLogFile=/dev/stdout --set pilot.env.PILOT_ENABLE_GATEWAY_API_GATEWAYCLASS_CONTROLLER=false > "$work/istiod.yaml"
python3 - "$work" <<'PY'
import gzip
from pathlib import Path
import sys
root = Path(sys.argv[1])
parts = [b'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: istio-system\n']
parts += [(root / name).read_bytes() for name in ['gateway-api.yaml', 'base.yaml', 'istiod.yaml']]
Path('internal/lab/manifests/istio.yaml.gz').write_bytes(gzip.compress(b'\n---\n'.join(parts), mtime=0))
PY
