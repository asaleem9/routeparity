# Development plan

The original twelve-week milestone plan. Weeks describe deliverables, not calendar deadlines. The README describes the current implementation and limitations.

| Week | Deliverable | Demo |
|---|---|---|
| 1 | Repo scaffold, core types, Ingress model. Two spikes: `go mod` with ingress2gateway as a dependency; scripted kind + ingress-nginx + Envoy Gateway + echo hello-world | Spike output; go/no-go on in-process conversion |
| 2 | `audit` with ~10 rules and annotation inventory, text + JSON | Runs in CI on sample manifests |
| 3 | Derived + mutation probes, raw prober, snapshots, `record` / `diff` / `live` | Diff two local proxies |
| 4 | Normalisers, noise handling, `verify`, JUnit + Markdown | Verify across a controller upgrade |
| 5 | Echo image; lab part 1 (kind, ingress-nginx, backend swap, baseline) | The five documented traps reproduced as tests |
| 6 | Envoy Gateway, user manifests, ingress2gateway conversion, readiness gating | Headline demo |
| 7 | Explanations, fix templates (keyed by rule and implementation family), coverage + UNVERIFIED accounting. **v0.1.0** | Public release |
| 8 | NGINX Gateway Fabric; implementation interface hardened; nightly matrix job | Two-column matrix |
| 9 | Istio; matrix site on GitHub Pages; file upstream issues for confirmed divergences | Public matrix |
| 10 | Access-log corpus (default format has no `$host`: join `$proxy_upstream_name` back to Ingress rules; also accept JSONL); GitHub Action | Replay real traffic |
| 11 | HTML report, docs site, generated rule pages | Docs |
| 12 | Buffer, launch write-up, **v0.2.0** | Launch |

Week 8 adds NGINX Gateway Fabric, per-Gateway proxy selection and readiness, and a scheduled comparison of Envoy Gateway and NGINX Gateway Fabric. Each matrix run retains both the literal-route and ingress2gateway-converted views, controller versions, timestamps, differences and unverified features.

Week 9 adds Istio 1.32.0, a three-column interactive matrix site, and a GitHub Pages deployment job. The site retains fourteen complete measured runs, including raw comparisons and replayable snapshots. Minimal capture-rewrite and regex-portability fixtures support upstream reports. See `matrix-site.md` for publication setup and `upstream-findings.md` for evidence and issue links.
