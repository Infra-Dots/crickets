# Crickets — plan

Go app that finds Deployments/StatefulSets exposed through an Ingress or a LoadBalancer Service,
asks Prometheus whether that edge received traffic over a window, and labels the idle ones for
garbage collection. It labels and never deletes. Acting on the label is someone else's job
(kube-janitor, a policy, a human).

## Prior art (2026-11-08)

Nothing open source does this specific join (workload → exposure → LB/ingress metric → label).

| Tool | What it does | Why it isn't this |
|---|---|---|
| [kor](https://github.com/yonahd/kor) | Lists unused ConfigMaps/Secrets/Services/Deployments…, `--delete` | Looks at references only, no traffic |
| [useless-operator](https://pkg.go.dev/github.com/Nastradamus/useless-operator) | Uses Prometheus to find pods with no traffic | Tiny, early-stage, pod-level, no LB/ingress providers |
| [Kubecost Abandoned Workloads](https://docs.kubecost.com/using-kubecost/navigating-the-kubecost-ui/savings/abandoned-workloads) | Flags workloads under a bytes/s threshold | Commercial, pod network bytes (health checks and scrapes count as traffic), reports only, no label |
| [K8sPurger](https://github.com/yogeshkk/K8sPurger) | Exports unused resources as metrics | No traffic signal |
| kube-janitor / kube-green / KEDA HTTP add-on | TTL delete / scheduled sleep / scale-to-zero | Not idle detection. kube-janitor is a good *consumer* of our label |

Gap: edge traffic per provider, "no data" kept apart from "zero traffic", and a label that other tools can act on.

## How it decides

1. List Deployments + StatefulSets (namespace include/exclude, optional label selector).
   Skip anything with `crickets.io/ignore: "true"` or younger than `minAge`.
2. Exposures = Services in the same namespace whose non-empty selector matches the pod template labels, then:
   - each `type: LoadBalancer` Service is an exposure;
   - each Ingress whose rule/default backend points at one of those Services is an exposure;
   - each Gateway API `HTTPRoute` (`gateway.networking.k8s.io/v1`) whose `backendRefs` point at one
     of those Services is an exposure. Its class comes from parentRef → Gateway → `gatewayClassName`.
     The Gateway API CRDs not being installed means no HTTPRoutes. That is not an error.
   No exposures → **skip** (not our concern).
3. Each exposure is matched to a **provider profile** (first match wins: ingress class, gateway
   class / controllerName, annotation, status hostname suffix). No match → workload **unknown**, never idle.
4. Per profile: run `health` at `now` **and** at `now - window`. Both must return data, or the
   source is **unknown**. Two checks cover both cases: the exporter is down now, or Prometheus
   retention is shorter than the window.
5. Run the profile's `query` (a Go `text/template` rendering a PromQL expression). An empty vector
   from a healthy source counts as 0. ingress-nginx and Traefik emit no series for a backend that
   was never hit.
6. Workload is **idle** only if *every* exposure is known and `value <= threshold`.
   Idle → apply label `crickets.io/idle=true` plus annotation `crickets.io/idle-evidence`
   (JSON: since, window, per-exposure provider/query/value). Traffic seen again → remove both.
7. Writes go through server-side apply with field manager `crickets`, on `metadata` only. The
   pod template is untouched, so nothing rolls out, and GitOps tools see a field another manager owns.
8. `dryRun: true` is the default: it logs what it would label and writes nothing.
9. Prometheus unreachable → no writes at all, exit 1.

Template vars: `.Namespace .Kind .Workload .Service .ServicePorts .Ingress .Route .Gateway .GatewayNamespace .Hosts .LBHostname .LBIP .Annotations .Window`, plus func `lbName`.

## Provider profiles (built-in, embedded YAML, overridable/extendable in config)

Every metric/label name below gets checked in Phase 0 before any code relies on it. Cloud LB metrics
are not in Prometheus by default. Each one needs an exporter, and the docs name it.

| Profile | Match | Metric (via) | Join key |
|---|---|---|---|
| traefik (k3s default) | ingressClass `traefik` | `traefik_service_requests_total` (native, `:9100/metrics`) | `service=~"<ns>-<svc>-.*@kubernetes"` (live: `spike-whoami-80@kubernetes`) |
| ingress-nginx (retired upstream, still everywhere) | class `nginx` | `nginx_ingress_controller_requests` (native) | `namespace`/`exported_namespace`, `ingress`, `service` |
| kong | class `kong` | `kong_http_requests_total` (Prometheus plugin, `status_code_metrics=true`) | `service=~"<ns>[.]<svc>[.][^.]+"` |
| istio | class `istio` | `istio_requests_total` (native) | `destination_service_name`, `destination_service_namespace`, `source_workload="istio-ingressgateway"`, `reporter="source"` (a gateway reports as source) |
| aws alb / nlb | Ingress class `alb` / Service LB-controller annotations (hostname alone is ambiguous) | `aws_applicationelb_request_count_sum`, `aws_networkelb_new_flow_count_sum` (YACE) | `dimension_LoadBalancer=~"app/<lbName>/.*"` |
| gcp | GKE Ingress, annotation `ingress.kubernetes.io/url-map` | `stackdriver_https_lb_rule_loadbalancing_googleapis_com_https_request_count` (stackdriver_exporter) | `url_map_name` = the annotation value |
| azure | Service `service.beta.kubernetes.io/azure-*`; AGIC unsupported (no gateway name) | LB `ByteCount` as `azure_lb_bytecount_total` (webdevops azure-metrics-exporter) | `dimension="<LBIP>"` (FrontendIPAddress) |
| hetzner | annotation `load-balancer.hetzner.cloud/name` | `hetzner_load_balancer_connections_per_second` (wacken89/hetzner-load-balancer-prometheus-exporter) | `hetzner_load_balancer_name` |
| digitalocean | annotation `service.beta.kubernetes.io/do-loadbalancer-*` | **unsupported**: no exporter with per-LB traffic | n/a |
| traefik-gateway | gatewayClass controller `traefik.io/gateway-controller` | `traefik_service_requests_total` | `service=~"<ns>-<svc>-https?-[0-9]+@kubernetesgateway"` (live: `spike-whoami-http-80@kubernetesgateway`) |
| envoy-gateway | controller `gateway.envoyproxy.io/gatewayclass-controller` | `envoy_cluster_upstream_rq_total` (native) | `envoy_cluster_name=~"httproute/<routeNS>/<route>/rule/.*"` |
| istio-gateway | controller `istio.io/gateway-controller` | `istio_requests_total` | as istio, `source_workload=~"<gateway>-.*"` (unverified live) |
| nginx-gateway-fabric | controller `gateway.nginx.org/nginx-gateway-controller` | **unsupported**: no per-route request metric found | n/a |
| kong-gateway | controller `konghq.com/kic-gateway-controller` | `kong_http_requests_total` | `service=~"httproute[.].*[.]svc[.](.*_)?<ns>[.]<svc>[.].*"` |
| gke-gateway | class `gke-l7-*` | **unsupported**: url map is in the Gateway's annotation, not in `Exposure` | n/a |

Counters use `sum(increase(m[{{.Window}}]))` and gauges use `sum(sum_over_time(m[{{.Window}}]))`.
The profile author writes the whole expression, and the app only compares the scalar to `threshold`.

## Config (ConfigMap, YAML)

```yaml
prometheus: { url: http://prometheus:9090, bearerTokenFile: "", timeout: 30s }
window: 168h        # 7d
minAge: 168h        # defaults to window
threshold: 0        # requests (or profile unit) over window
dryRun: true
namespaces: { include: [], exclude: [kube-system, kube-public] }
selector: ""        # optional opt-in label selector
label: crickets.io/idle
providers: []       # extra/override profiles; same schema as built-ins
```

Runs as a **CronJob** (`0 */6 * * *`): one pass, then exit. That means no leader election, no
informers and no long-lived state. The exit code and logs are the signal, and the label itself shows
up in Prometheus through kube-state-metrics `--metric-labels-allowlist`.

## Contracts (shared by the Phase 1 agents; change only here)

`internal/providers/profiles.yaml`, a list of:

```yaml
- name: traefik
  supported: true            # false: shipped for docs, never matched
  unit: requests             # docs only
  match:                     # kinds AND (any of the other non-empty matchers)
    kinds: [Ingress]         # Ingress | HTTPRoute | Service (type LoadBalancer)
    ingressClasses: [traefik]
    gatewayControllers: []   # GatewayClass spec.controllerName, exact
    gatewayClasses: []       # Gateway spec.gatewayClassName, path.Match glob (gke-l7-*)
    annotations: []          # annotation key present on the exposure object
    hostnameSuffixes: []     # status.loadBalancer.ingress[].hostname suffix
  query: 'sum(increase(traefik_service_requests_total{service=~"{{.Namespace}}-{{.Service}}-.*@kubernetes"}[{{.Window}}]))'
  health: 'up{job="traefik"}'
```

Template data, `providers.Exposure`:
`Namespace, Kind (Deployment|StatefulSet), Workload, ExposureKind (Ingress|HTTPRoute|Service), Service string; ServicePorts []int32; Ingress, Route, Gateway, GatewayNamespace string; Hosts []string; LBHostname, LBIP string; Annotations map[string]string (of the exposure object); Window string`.
`Window` is whole seconds as a Prometheus duration (`604800s`), never Go's `168h0m0s`. Funcs: `lbName` (AWS ELB hostname → LB name; Phase 0 pins the rule).

Binary: `crickets --config /etc/crickets/config.yaml [--kubeconfig path]`, one pass. Exit 0 = pass done (dry or not); 1 = Prometheus errored, config invalid, or a write failed. Image `ghcr.io/infra-dots/crickets`, runs as nonroot 65532.

## Layout

```
cmd/crickets/main.go         flags: --config, --kubeconfig
internal/config/             load, defaults, validate
internal/discovery/          workloads → services → exposures
internal/providers/          embedded profiles.yaml, match, render
internal/prom/               instant query at time t → float64|empty; health check
internal/reconcile/          decide + SSA label/annotation
charts/crickets/             CronJob, SA, ClusterRole(+Binding), ConfigMap
test/e2e/                    go test -tags e2e against a k3d cluster
hack/e2e-up.sh, hack/e2e-down.sh
docs/                        *.md
.github/workflows/ci.yml     lint, unit, helm lint, e2e
```

Deps: `k8s.io/client-go`, `sigs.k8s.io/gateway-api`, `github.com/prometheus/client_golang/api`, `sigs.k8s.io/yaml`. Nothing else.

Module: `github.com/Infra-Dots/crickets` (moves out of Infra-Dots later).

RBAC: `get,list` deployments, statefulsets, services, ingresses, ingressclasses, httproutes, gateways, gatewayclasses; `patch` deployments, statefulsets.

## Checklist

### Phase 0: provider spike (subagent **P**, parallel with A/B)
- [x] For each profile above: exact metric + label names from the exporter's source/docs, with a link
- [x] Pick the Hetzner and DigitalOcean exporters, or mark the profile "unsupported: no exporter"
- [x] Write `internal/providers/profiles.yaml` + one `.prom` fixture sample per profile (`test/e2e/fixtures/`)
- [x] Confirm the k3s Traefik metrics endpoint/port and service label format on a live k3d cluster, for both the Ingress and the Gateway API provider
- [x] Confirm how to switch on the k3s Traefik Gateway API provider (`HelmChartConfig` + Gateway API CRDs) and pin the CRD version
  - Done: CRDs `v1.4.0` standard channel, listener port 8000, manifests in `test/e2e/manifests/spike/`; details in `docs/providers-research.md`.

### Phase 1: core (subagent **A**)
- [x] `go mod init`, Makefile (`test`, `lint`, `build`, `image`; `e2e` target left to C), golangci-lint config
- [x] config: load/defaults/validate + table tests
- [x] discovery: selector matching, Ingress backends (rules + defaultBackend), LB services. Tests on the client-go fake clientset
- [x] discovery: HTTPRoute backendRefs (same-namespace + cross-namespace with ReferenceGrant ignored for v1, since the route still routes), parentRef → Gateway → GatewayClass. Use `sigs.k8s.io/gateway-api` typed client + fake, and treat missing CRDs (discovery 404) as no routes
- [x] providers: matching order, template render, `lbName`. Tests per built-in profile
- [x] prom: query at time t, empty → (0, ok), error → unknown. Tests against `httptest` Prometheus JSON
- [x] reconcile: idle/active/unknown/skip matrix, SSA apply + removal, dryRun writes nothing, Prometheus down writes nothing
- [x] main: one pass, structured `slog` JSON logs, exit codes 0/1
- [x] Dockerfile (distroless static, nonroot)

### Phase 1: chart (subagent **B**)
- [ ] `charts/crickets`: CronJob, ServiceAccount, ClusterRole/Binding, ConfigMap from `.Values.config`
- [ ] values: schedule, image, resources, securityContext (non-root, read-only FS), `config` passthrough
- [ ] `values.schema.json`; `helm lint` + `helm template` golden test in CI

### Phase 2: e2e on k3s (subagent **C**, after A+B)
- [ ] `hack/e2e-up.sh`: k3d cluster (k3s ships Traefik), build + `k3d image import`, minimal Prometheus manifest scraping Traefik + a fixture exporter (busybox httpd serving `fixtures/*.prom`), `helm install` with `window: 2m`, `minAge: 0`, `dryRun: false`
- [ ] Scenarios (see showcase): busy ingress kept; idle ingress labeled; unexposed skipped; ignore-annotation skipped; young workload skipped; unknown ingress class skipped; idle StatefulSet behind a Hetzner-fixture LB labeled; ALB-fixture Ingress with fake status and traffic kept; busy HTTPRoute via Traefik Gateway kept; idle HTTPRoute labeled; workload with a busy Ingress and an idle HTTPRoute kept (every exposure must be idle)
- [ ] Recovery: send traffic to the idle one, re-run the job, label removed
- [ ] Prometheus scaled to 0, job fails, no labels changed
- [ ] Runs the job with `kubectl create job --from=cronjob/...` and asserts through client-go
- [ ] CI job on ubuntu runner

### Phase 3: docs (subagent **D**, after A+P)
- [ ] `README.md`: what it does, what it won't do (delete), 60-second quickstart
- [ ] `docs/how-it-works.md`: the decision rules above, unknown ≠ idle
- [ ] `docs/configuration.md`: every key, generated from the config struct comments where possible
- [ ] `docs/providers.md`: per profile, the exporter to install, the metric, the join, a sample PromQL, and how to write your own
- [ ] `docs/acting-on-the-label.md`: kube-janitor / Kyverno / manual `kubectl get deploy -l crickets.io/idle`
- [ ] `docs/development.md`: unit + e2e

### Phase 4: integration (me)
- [ ] Review each agent's diff, run `make test lint e2e`, fix seams
- [ ] Review section below

## Subagent split

| Wave | Agent | Model | Input | Output |
|---|---|---|---|---|
| 1 | P: provider spike | sonnet | profile table above | `profiles.yaml`, fixtures, verified links |
| 1 | A: core | sonnet | "How it decides", layout, config | `cmd/`, `internal/`, unit tests, Dockerfile |
| 1 | B: chart | sonnet | config schema, RBAC | `charts/crickets` |
| 2 | C: e2e | sonnet | A + B + P fixtures | `hack/`, `test/e2e/`, CI e2e job |
| 2 | D: docs | sonnet | A + P | `README.md`, `docs/` |
| 3 | me | — | all | integrated, green `make test lint e2e` |

Each agent works in its own git worktree on its own branch, with no stacked branches. I merge in wave order.

## Out of scope (v1)
- Deleting or scaling anything
- Gateway API `GRPCRoute`/`TLSRoute`/`TCPRoute` (HTTPRoute is in v1); Cilium Gateway profile
- Pod network-bytes fallback for unknown providers (health checks make it lie)
- Real-cloud verification: cloud profiles are tested against fixture metrics with real label shapes, not live LBs
- Release automation (goreleaser, image signing)

## Review
_(filled in after landing)_
