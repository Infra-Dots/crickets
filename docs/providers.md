# Providers

A provider profile maps an exposure to one PromQL query and one health query. The built-ins live in `internal/providers/profiles.yaml`. Evidence for each (links, label names, what was run live) is in [providers-research.md](providers-research.md).

Example queries below are rendered for namespace `shop`, Service `web`, Ingress or route `web`, window `168h` (which renders as `604800s`).

Two rules apply to every profile:

- **Health is checked twice.** The `health` query must return a sample now and one window ago. A source that is new, down, or kept for less than the window gives unknown, not idle. Details: [how it works](how-it-works.md#unknown-is-never-idle).
- **Counters** use `sum(increase(m[window]))`. **Gauges** use `sum(sum_over_time(m[window]))`. Idle means the result is `<= threshold`.
- **Both label spellings.** A metric label called `service` or `namespace` collides with the scrape target's own labels. With `honor_labels: false` (the default, and what Grafana Alloy and Prometheus Operator ServiceMonitors use) Prometheus renames the metric's label to `exported_service` / `exported_namespace`. A query that only spells the original finds nothing, and nothing reads as zero traffic, so every such profile `or`s both spellings. A test guards this.

| Profile | Exposure | Matches | Supported |
|---|---|---|---|
| [traefik](#traefik) | Ingress | class `traefik` | yes |
| [traefik-gateway](#traefik-gateway) | HTTPRoute | controller `traefik.io/gateway-controller` | yes |
| [ingress-nginx](#ingress-nginx) | Ingress | class `nginx` | yes |
| [kong](#kong) | Ingress | class `kong` | yes |
| [kong-gateway](#kong-gateway) | HTTPRoute | controller `konghq.com/kic-gateway-controller` | yes |
| [istio](#istio) | Ingress | class `istio` | yes |
| [istio-gateway](#istio-gateway) | HTTPRoute | controller `istio.io/gateway-controller` | yes |
| [envoy-gateway](#envoy-gateway) | HTTPRoute | controller `gateway.envoyproxy.io/gatewayclass-controller` | yes |
| [aws-alb](#aws-alb) | Ingress | class `alb` | yes |
| [aws-nlb](#aws-nlb) | Service | annotation `aws-load-balancer-type` or `-nlb-target-type` | yes |
| [gcp](#gcp) | Ingress | class `gce`, `gce-internal`, or annotation `ingress.kubernetes.io/url-map` | yes |
| [azure](#azure) | Service | any of four `service.beta.kubernetes.io/azure-*` annotations | yes |
| [hetzner](#hetzner) | Service | annotation `load-balancer.hetzner.cloud/name` | yes |
| [Unsupported](#unsupported-profiles) | | | no |

## Native ingress and gateway controllers

### traefik

- **Enable:** Traefik's Prometheus metrics. On k3s they are on by default: port `9100`, path `/metrics`. Scrape that port.
- **Metric:** `traefik_service_requests_total`, a counter.
- **Join:** label `service` is `<ns>-<svc>-<port>@kubernetes`. The profile matches `<ns>-<svc>-.*@kubernetes`.
- **Query:**
  ```promql
  sum(increase(traefik_service_requests_total{service=~"shop-web-.*@kubernetes"}[604800s]))
  or
  sum(increase(traefik_service_requests_total{exported_service=~"shop-web-.*@kubernetes"}[604800s]))
  ```
- **Health:** `traefik_config_last_reload_success`.
- **Caveat:** the port part is a wildcard, so Service `a` also matches `shop-a-b-80`. That errs toward busy. Traefik emits no series for a backend never hit; the empty result counts as 0.

### traefik-gateway

- **Enable:** the Traefik Gateway API provider (on k3s, a `HelmChartConfig` for `traefik` with `providers.kubernetesGateway.enabled: true`) and the Gateway API CRDs. The Gateway listener must use Traefik's entrypoint port (8000 on k3s). A working example is in `test/e2e/manifests/spike/`.
- **Metric:** `traefik_service_requests_total`.
- **Join:** `service` is `<ns>-<svc>-<scheme>-<port>@kubernetesgateway`, for example `spike-whoami-http-80@kubernetesgateway`. The route and Gateway names do not appear.
- **Query:**
  ```promql
  sum(increase(traefik_service_requests_total{service=~"shop-web-https?-[0-9]+@kubernetesgateway"}[604800s]))
  or
  sum(increase(traefik_service_requests_total{exported_service=~"shop-web-https?-[0-9]+@kubernetesgateway"}[604800s]))
  ```
- **Health:** `traefik_config_last_reload_success`.
- **Caveat:** same wildcard-toward-busy note as Traefik.

### ingress-nginx

- **Enable:** the controller's metrics (`controller.metrics.enabled=true` in the Helm chart) and a scrape of port 10254. Upstream has retired the project, but it is still common.
- **Metric:** `nginx_ingress_controller_requests`, a counter.
- **Join:** `ingress`, `service`, and the namespace. A pod scrape renames only `namespace` to `exported_namespace`; an endpoints scrape (Alloy, a ServiceMonitor) renames `service` too. The query tries all three spellings with `or`.
- **Query:**
  ```promql
  sum(increase(nginx_ingress_controller_requests{ingress="web",service="web",namespace="shop"}[604800s]))
  or
  sum(increase(nginx_ingress_controller_requests{ingress="web",service="web",exported_namespace="shop"}[604800s]))
  or
  sum(increase(nginx_ingress_controller_requests{ingress="web",exported_service="web",exported_namespace="shop"}[604800s]))
  ```
- **Health:** `nginx_ingress_controller_config_last_reload_successful`.
- **Caveat:** no series for a never-hit backend; counts as 0.

### kong

- **Enable:** the Kong `prometheus` plugin with `status_code_metrics: true`. Without that setting `kong_http_requests_total` does not exist.
- **Metric:** `kong_http_requests_total`.
- **Join:** the Kong service for an Ingress is named `<ns>.<svc>.<port>` by KIC. Kubernetes Service names cannot contain dots, so `[^.]+` for the port is exact.
- **Query:**
  ```promql
  sum(increase(kong_http_requests_total{service=~"shop[.]web[.][^.]+"}[604800s]))
  or
  sum(increase(kong_http_requests_total{exported_service=~"shop[.]web[.][^.]+"}[604800s]))
  ```
- **Health:** `kong_datastore_reachable`.

### kong-gateway

- **Enable:** as kong, with Gateway API enabled in KIC.
- **Join:** the Kong service for an HTTPRoute is `httproute.<routeNS>.svc.<backendNS>.<svc>.<port>[.<weight>]`; several backends are joined with `_`.
- **Query:**
  ```promql
  sum(increase(kong_http_requests_total{service=~"httproute[.].*[.]svc[.](.*_)?shop[.]web[.].*"}[604800s]))
  or
  sum(increase(kong_http_requests_total{exported_service=~"httproute[.].*[.]svc[.](.*_)?shop[.]web[.].*"}[604800s]))
  ```
- **Health:** `kong_datastore_reachable`.
- **Caveat:** the wildcards (route namespace, port, weight, other backends) are loose on purpose. A false match counts someone else's traffic, so it errs toward busy.

### istio

- **Enable:** Istio's standard metrics, scraped from the ingress gateway.
- **Metric:** `istio_requests_total`.
- **Join:** the gateway reports as `reporter="source"`, with `source_workload="istio-ingressgateway"`. This works even if the destination workload has no sidecar.
- **Query:**
  ```promql
  sum(increase(istio_requests_total{reporter="source",source_workload="istio-ingressgateway",destination_service_name="web",destination_service_namespace="shop"}[604800s]))
  ```
- **Health:** `istio_build`.
- **Caveat:** a gateway with another deployment name needs your own profile. `istio_build` as a health series was not confirmed live.

### istio-gateway

- **Enable:** as istio, with Istio's Gateway API controller.
- **Join:** the auto-provisioned gateway deployment is named `<gateway>-<class>`, hence the prefix match on `source_workload`, plus the Gateway's namespace.
- **Query:**
  ```promql
  sum(increase(istio_requests_total{reporter="source",source_workload=~"gw-.*",source_workload_namespace="infra",destination_service_name="web",destination_service_namespace="shop"}[604800s]))
  ```
  (Gateway `gw` in namespace `infra`.)
- **Health:** `istio_build`.
- **Caveat:** the deployment name was not verified on a live cluster.

### envoy-gateway

- **Enable:** per-cluster Envoy stats exposed to Prometheus. Whether the default proxy bootstrap does so is not confirmed.
- **Metric:** `envoy_cluster_upstream_rq_total`.
- **Join:** the cluster name is `httproute/<routeNS>/<route>/rule/<n>`. Note this is the route, not the Service.
- **Query:**
  ```promql
  sum(increase(envoy_cluster_upstream_rq_total{envoy_cluster_name=~"httproute/shop/web/rule/.*"}[604800s]))
  ```
- **Health:** `envoy_server_live`.

## Cloud load balancers

### Cloud exporters are gauges

CloudWatch, Cloud Monitoring and similar APIs give one value per period (a one-minute sum, say). Their exporters re-export the latest value on every Prometheus scrape, so the series is a gauge and one period appears many times. The profiles use `sum(sum_over_time(...))`. The total is inflated by the scrape-to-period ratio. **Only threshold `0` is meaningful** for these profiles: zero means no period had traffic.

### aws-alb

- **Enable:** [YACE](https://github.com/prometheus-community/yet-another-cloudwatch-exporter) with job `AWS/ApplicationELB`, metric `RequestCount`, statistics `[Sum]`.
- **Metric:** `aws_applicationelb_request_count_sum`.
- **Join:** `dimension_LoadBalancer` is `app/<name>/<id>`. The name comes from the Ingress status hostname through `lbName`. The id is not in the hostname, so the query ends with `/.*`.
- **Matches:** Ingress class `alb` only. The `.elb.amazonaws.com` hostname alone is ambiguous, because ingress-nginx or Traefik behind an ELB show the same hostname.
- **Query** (hostname `k8s-shop-web-0a1b2c3d4e-1234567890.us-east-1.elb.amazonaws.com`):
  ```promql
  sum(sum_over_time(aws_applicationelb_request_count_sum{dimension_LoadBalancer=~"app/k8s-shop-web-0a1b2c3d4e/.*"}[604800s]))
  ```
- **Health:** `yace_cloudwatch_requests_total`.
- **Caveat:** no status hostname yet, or a hostname `lbName` cannot parse, means unknown. Classic ELBs are not covered.

### aws-nlb

- **Enable:** YACE with job `AWS/NetworkELB`, metric `NewFlowCount`, statistics `[Sum]`.
- **Metric:** `aws_networkelb_new_flow_count_sum`. Unit is flows.
- **Join:** `dimension_LoadBalancer` is `net/<name>/<id>`.
- **Matches:** a `type: LoadBalancer` Service with annotation `service.beta.kubernetes.io/aws-load-balancer-type` or `service.beta.kubernetes.io/aws-load-balancer-nlb-target-type`. A Service using only `loadBalancerClass: service.k8s.aws/nlb` does not match (unknown, which is safe) until you add a profile.
- **Query:**
  ```promql
  sum(sum_over_time(aws_networkelb_new_flow_count_sum{dimension_LoadBalancer=~"net/k8s-shop-web-1a2b3c4d5e/.*"}[604800s]))
  ```
- **Health:** `yace_cloudwatch_requests_total`.

`lbName` turns these hostnames into the name:

| Hostname | `lbName` |
|---|---|
| `k8s-shop-web-0a1b2c3d4e-1234567890.us-east-1.elb.amazonaws.com` | `k8s-shop-web-0a1b2c3d4e` |
| `internal-k8s-shop-web-0a1b2c3d4e-1234567890.us-east-1.elb.amazonaws.com` | `k8s-shop-web-0a1b2c3d4e` |
| `dualstack.k8s-shop-web-0a1b2c3d4e-1234567890.eu-west-1.elb.amazonaws.com` | `k8s-shop-web-0a1b2c3d4e` |
| `k8s-shop-web-1a2b3c4d5e-0123456789abcdef.elb.us-east-1.amazonaws.com` (NLB) | `k8s-shop-web-1a2b3c4d5e` |
| anything else | empty, so the `need` guard makes the exposure unknown |

### gcp

- **Enable:** [stackdriver_exporter](https://github.com/prometheus-community/stackdriver_exporter) with `--monitoring.metrics-type-prefixes=loadbalancing.googleapis.com/https/request_count`. Leave `--monitoring.aggregate-deltas` **off**. The delta metric is then exported as a gauge per interval, which is what `sum_over_time` expects. Turning it on is not needed.
- **Metric:** `stackdriver_https_lb_rule_loadbalancing_googleapis_com_https_request_count`.
- **Join:** `url_map_name` equals the Ingress annotation `ingress.kubernetes.io/url-map`, which GKE sets on the Ingress.
- **Query:**
  ```promql
  sum(sum_over_time(stackdriver_https_lb_rule_loadbalancing_googleapis_com_https_request_count{url_map_name="k8s2-um-abc123"}[604800s]))
  ```
- **Health:** `stackdriver_monitoring_scrapes_total`.
- **Caveat:** one URL map serves the whole Ingress. Traffic to a sibling Service on the same Ingress counts, which errs toward busy. Without the annotation the exposure is unknown.

### azure

- **Enable:** [azure-metrics-exporter](https://github.com/webdevops/azure-metrics-exporter) probing `Microsoft.Network/loadBalancers`, metric `ByteCount`, aggregation `total`, `name=azure_lb`, `template={name}_{metric}_{aggregation}`, `metricFilter=FrontendIPAddress eq '*'`. Needs a Standard SKU load balancer.
- **Metric:** `azure_lb_bytecount_total`. Unit is bytes, so a threshold above 0 is bytes.
- **Join:** `dimension` is the frontend IP, taken from the Service's `status.loadBalancer.ingress[].ip`. AKS shares one LB per cluster, with one frontend IP per Service.
- **Matches:** a `type: LoadBalancer` Service with one of `azure-load-balancer-resource-group`, `azure-load-balancer-internal`, `azure-pip-name`, `azure-dns-label-name` (all under `service.beta.kubernetes.io/`).
- **Query:**
  ```promql
  sum(sum_over_time(azure_lb_bytecount_total{dimension="20.1.2.3"}[604800s]))
  ```
- **Health:** `azurerm_api_ratelimit`.
- **Caveat:** bytes include health probes and scans, so a "quiet" LB may read busy. The `{metric}` template output case was not run.

### hetzner

- **Enable:** [hetzner-load-balancer-prometheus-exporter](https://github.com/wacken89/hetzner-load-balancer-prometheus-exporter) with `ACCESS_TOKEN` and `LOAD_BALANCER_IDS` (`all` works).
- **Metric:** `hetzner_load_balancer_connections_per_second`, a gauge. Not `requests_per_second`, which exists only for HTTP services.
- **Join:** `hetzner_load_balancer_name` equals the Service annotation `load-balancer.hetzner.cloud/name`, which hcloud-CCM uses. A Service without it is unmatched, so unknown.
- **Query:**
  ```promql
  sum(sum_over_time(hetzner_load_balancer_connections_per_second{hetzner_load_balancer_name="web-lb"}[604800s]))
  ```
- **Health:** `hetzner_load_balancer_open_connections`.
- **Caveat:** whether Hetzner's own health checks count in the connection rate is not verified.

## Unsupported profiles

Shipped for the docs, never matched (`supported: false`). The exposure is unknown.

| Profile | Why not | What would enable it |
|---|---|---|
| nginx-gateway-fabric | The source exposes controller and server-level metrics. No per-route or per-upstream request counter was found. | Such a metric upstream, or a server-level metric you accept as a join. |
| gke-gateway | The URL map name sits in the Gateway's `networking.gke.io/url-maps` annotation, which `Exposure` does not carry. | A `GatewayAnnotations` field on `Exposure`, then the gcp metric with `url_map_name`. |
| azure-agic | Application Gateway `TotalRequests` is per gateway, and the gateway name is not in `Exposure`. | A way to name the gateway, for example from an Ingress annotation, in a user profile. |
| digitalocean | The DO API has a per-LB HTTP request metric, but no maintained Prometheus exporter publishes it. | An exporter that does. Then a profile matching `service.beta.kubernetes.io/do-loadbalancer-name`. |

## Write your own profile

Put profiles under `providers:` in the config (Helm: `config.providers`). They use the same schema as the built-ins.

```yaml
providers:
  - name: acme-edge
    supported: true
    unit: requests
    match:
      kinds: [Ingress]
      ingressClasses: [acme]
    query: 'sum(increase(acme_edge_requests_total{backend_namespace="{{.Namespace}}",backend_service="{{need .Service}}"}[{{.Window}}]))'
    health: 'acme_edge_build_info'
```

### Schema

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Required. Unique. The same name as a built-in replaces it. |
| `supported` | bool | `false` ships the profile for docs and never matches it. A supported profile needs `query`, `health` and one matcher besides `kinds`. |
| `unit` | string | Docs only. |
| `match.kinds` | list | Required. Any of `Ingress`, `HTTPRoute`, `Service` (a type LoadBalancer Service). |
| `match.ingressClasses` | list | Exact match on the Ingress class. |
| `match.gatewayControllers` | list | Exact match on the GatewayClass `spec.controllerName`. |
| `match.gatewayClasses` | list | Glob (Go `path.Match`) on the Gateway's `gatewayClassName`, e.g. `gke-l7-*`. |
| `match.annotations` | list | Annotation key is present on the exposure object (the Ingress, HTTPRoute or Service). |
| `match.hostnameSuffixes` | list | Suffix of a `status.loadBalancer.ingress[].hostname` (or a Gateway status hostname address). |
| `query` | string | A Go `text/template` that renders one PromQL expression. |
| `health` | string | PromQL. Must return a sample now and one window ago. Not templated. |

Unknown fields are an error. A profile that fails to compile stops the run at start with exit 1.

### Match semantics

- `kinds` must contain the exposure's kind, **and** at least one of the other matchers must hit. The other matchers are OR-ed together.
- The first **supported** match wins. Your profiles come first, in the order you wrote them, then the built-ins in their file order. A profile of yours with a built-in's name replaces that built-in. An unsupported profile is never picked.
- Class and controller names must be non-empty to match. If an Ingress has no class and the cluster has no default IngressClass, class matchers cannot hit.

### Template data

Fields on `.`:

| Field | Type | Notes |
|---|---|---|
| `Namespace` | string | Workload namespace. Also the Service's. |
| `Kind` | string | `Deployment` or `StatefulSet`. |
| `Workload` | string | Workload name. |
| `ExposureKind` | string | `Ingress`, `HTTPRoute` or `Service`. |
| `Service` | string | The Service that selects the workload. |
| `ServicePorts` | list of int32 | The Service's ports. |
| `Ingress` | string | Ingress name, empty otherwise. |
| `Route`, `RouteNamespace` | string | HTTPRoute name and namespace, empty otherwise. |
| `Gateway`, `GatewayNamespace` | string | The route's parent Gateway; the namespace defaults to the route's. |
| `Hosts` | list of string | Ingress rule hosts, or route hostnames. |
| `LBHostname`, `LBIP` | string | First hostname and first IP from the status of the Ingress, the LoadBalancer Service, or the Gateway addresses. |
| `Annotations` | map | Annotations of the exposure object. |
| `Window` | string | Whole seconds as a PromQL duration, such as `604800s`. |

Functions:

- `need` fails the render when its argument is empty, and the exposure becomes **unknown**. Wrap every join key in it. An empty key matches no series, and no series reads as zero traffic, so an unwrapped key would label a busy app idle.
- `lbName` turns an AWS ALB or NLB hostname into the load balancer name, or `""` if it is not one. Use it as `{{need (lbName .LBHostname)}}`.
- Standard `text/template` builtins such as `index`, `printf` and `with`.

Annotation values are PromQL-escaped (`\`, `"`, newline) before rendering, so they are safe inside a quoted PromQL string. Other fields (names, hostnames) are Kubernetes names and not escaped. A missing struct field, such as a typo, fails the render, so the exposure is unknown.

Write the whole expression yourself: counters with `increase`, gauges with `sum_over_time`. Crickets compares the summed scalar to `threshold`. Check a profile by pasting the `query` from a dry-run `would label` log line into Prometheus.

### Worked example

A made-up controller, "Acme Edge". Its Ingress class is `acme`. It exports `acme_edge_requests_total{backend_namespace, backend_service}` as a counter, and `acme_edge_build_info` as a health series.

```yaml
config:
  providers:
    - name: acme-edge
      supported: true
      unit: requests
      match:
        kinds: [Ingress]
        ingressClasses: [acme]
      query: 'sum(increase(acme_edge_requests_total{backend_namespace="{{.Namespace}}",backend_service="{{.Service}}"}[{{.Window}}]))'
      health: 'acme_edge_build_info'
```

For a Service with a load-balancer annotation instead, with a gauge that carries the LB name:

```yaml
config:
  providers:
    - name: acme-lb
      supported: true
      unit: connections
      match:
        kinds: [Service]
        annotations: [lb.acme.example/name]
      query: 'sum(sum_over_time(acme_lb_connections{lb_name="{{need (index .Annotations "lb.acme.example/name")}}"}[{{.Window}}]))'
      health: 'acme_lb_exporter_up'
```

To override a built-in, reuse its name. This one narrows the Traefik query to a single port:

```yaml
config:
  providers:
    - name: traefik
      supported: true
      match:
        kinds: [Ingress]
        ingressClasses: [traefik]
      query: 'sum(increase(traefik_service_requests_total{service=~"{{.Namespace}}-{{.Service}}-(80|8080)@kubernetes"}[{{.Window}}]))'
      health: 'traefik_config_last_reload_success'
```
