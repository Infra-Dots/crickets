# Provider research (Phase 0, 2026-11-08)

Confidence: **verified-live** (ran it), **verified-from-source** (read the exporter/controller code or reference docs), **unverified** (profile ships `supported: false`, or the part is flagged).
"Join" is how an `Exposure` field reaches the metric label. Queries live in `internal/providers/profiles.yaml`.

| Profile | Supported | Confidence | Metric | Join |
|---|---|---|---|---|
| traefik | yes | verified-live | `traefik_service_requests_total` | `service=~"<ns>-<svc>-.*@kubernetes"` |
| traefik-gateway | yes | verified-live | `traefik_service_requests_total` | `service=~"<ns>-<svc>-https?-[0-9]+@kubernetesgateway"` |
| ingress-nginx | yes | verified-from-source | `nginx_ingress_controller_requests` | `ingress`, `service`, `namespace` or `exported_namespace` |
| kong | yes | verified-from-source | `kong_http_requests_total` | `service=~"<ns>[.]<svc>[.][^.]+"` |
| kong-gateway | yes | verified-from-source | `kong_http_requests_total` | `service=~"httproute[.].*[.]svc[.](.*_)?<ns>[.]<svc>[.].*"` |
| istio | yes | verified-from-source | `istio_requests_total` | `reporter="source"`, `source_workload="istio-ingressgateway"`, `destination_service_{name,namespace}` |
| istio-gateway | yes | partly (gateway deployment name unverified) | `istio_requests_total` | as istio, `source_workload=~"<gateway>-.*"` |
| envoy-gateway | yes | verified-from-source (cluster name); stats exposure not run | `envoy_cluster_upstream_rq_total` | `envoy_cluster_name=~"httproute/[^/]+/<route>/rule/.*"` |
| nginx-gateway-fabric | no | unverified | none per route | n/a |
| aws-alb | yes | verified-from-source | `aws_applicationelb_request_count_sum` | `dimension_LoadBalancer=~"app/<lbName>/.*"` |
| aws-nlb | yes | verified-from-source | `aws_networkelb_new_flow_count_sum` | `dimension_LoadBalancer=~"net/<lbName>/.*"` |
| gcp | yes | verified-from-source | `stackdriver_https_lb_rule_loadbalancing_googleapis_com_https_request_count` | `url_map_name` = Ingress annotation `ingress.kubernetes.io/url-map` |
| gke-gateway | no | unverified | same as gcp | needs the Gateway's `networking.gke.io/url-maps` annotation |
| azure | yes (Service LB only) | verified-from-source | `azure_lb_bytecount_total` | `dimension="<LBIP>"` |
| azure-agic | no | unverified | `TotalRequests` | per gateway only, no way to name it |
| hetzner | yes | verified-from-source | `hetzner_load_balancer_connections_per_second` | `hetzner_load_balancer_name` |
| digitalocean | no | n/a | none | no exporter |

## Live: Traefik on k3s (k3d, k3s v1.35.5, Traefik 3.6.13)

- Metrics: container port `metrics` = **9100**, path **`/metrics`** (entrypoint `metrics`, `--metrics.prometheus=true`); the pod carries `prometheus.io/{scrape,port,path}` annotations. Traefik's API/ping is on 8080, web on 8000.
- Ingress backend: `traefik_service_requests_total{code="200",method="GET",protocol="http",service="spike-whoami-80@kubernetes"}` = `<ns>-<svc>-<port>@kubernetes`.
- HTTPRoute backend: `service="spike-whoami-http-80@kubernetesgateway"` and, for a Service with an unnamed port 8081, `spike-plain-http-8081@kubernetesgateway`. So the format is `<ns>-<svc>-<scheme>-<port>@kubernetesgateway`; the `http` is the scheme, not the Service port name. The route and gateway names do not appear.
- A backend never hit has no series at all (empty vector), as the plan assumes.
- Health: `traefik_config_last_reload_success` is a gauge present from start, whatever the traffic.
- GatewayClass `traefik`: `spec.controllerName = traefik.io/gateway-controller` (created by Traefik once the provider is on; `Accepted=True`). HTTPRoute `status.parents[0].controllerName` is the same.
- Enable Gateway API: `HelmChartConfig` `traefik` in `kube-system` with `providers.kubernetesGateway.enabled: true` (Traefik restarts). The Gateway listener port must be **8000** (Traefik's `web` container port), not 80.
- **CRDs: Gateway API `v1.4.0`, standard channel** (`standard-install.yaml`). k3s already ships Gateway API CRDs in its `traefik-crd` chart; applying v1.4.0 reconfigured them in place and everything worked. Gateway got `Programmed=True`, HTTPRoute `Accepted` and `ResolvedRefs`. Traefik 3.6's own docs for the newest line name v1.6.3; v1.4.0 is what was run here.
- Manifests: `test/e2e/manifests/spike/` (`apply.sh` runs them in order).
- Caveats: ports are matched with `.*`, so Service `a` also matches `ns-a-b-80` (errs toward busy). A cluster without a `traefik` IngressClass is unmatched, not an error.

## Per profile

### ingress-nginx
Metric `nginx_ingress_controller_requests`, labels `status method path namespace ingress service canary` (+ `host` unless `--metrics-per-host=false`) plus the controller's own `controller_class controller_namespace controller_pod`. Source: `internal/ingress/metric/collectors/socket.go` (`requestTags`) in kubernetes/ingress-nginx; docs https://kubernetes.github.io/ingress-nginx/user-guide/monitoring/. When Prometheus scrapes with `honor_labels: false`, the ingress's `namespace` collides with the target label and becomes `exported_namespace`, so the query takes both. Health: `nginx_ingress_controller_config_last_reload_successful` (`controller.go`). The project is retired upstream; it is still widespread.

### kong / kong-gateway
Metric `kong_http_requests_total{service,route,code,source,consumer}`, **only when the Prometheus plugin has `status_code_metrics: true`** (https://developer.konghq.com/plugins/prometheus/). KIC names (https://github.com/Kong/kubernetes-ingress-controller, `internal/dataplane/translator/subtranslator/`): Ingress service `<ns>.<svc>.<port>`, Ingress route `<ns>.<ingress>.<svc>.<host>.<port>`; HTTPRoute service `httproute.<routeNS>.svc.<backendNS>.<svc>.<port>[.<weight>]` (backends sorted, joined with `_`, trimmed over 511 chars with `_combined.<sha256>`). Service names cannot contain dots, so `[^.]+` is exact. Health: `kong_datastore_reachable`.

### istio / istio-gateway
Metric `istio_requests_total` (https://istio.io/latest/docs/reference/config/metrics/). Labels: `reporter source_workload source_workload_namespace destination_service destination_service_name destination_service_namespace destination_workload response_code ...`. A gateway reports `reporter="source"` ("source if report is from a client Istio proxy or a gateway"), which also works for workloads without a sidecar. **The plan's `reporter="destination"` is wrong for that case and is changed.** `istio-gateway`: auto-provisioned gateways run as `<gateway>-<gatewayclass>`; that naming was not run, hence the prefix match and the flag. Health `istio_build` (unverified, emitted by every proxy and by istiod).

### envoy-gateway
Metric `envoy_cluster_upstream_rq_total{envoy_cluster_name}`. Cluster names `httproute/<routeNS>/<route>/rule/<n>`, seen in the Envoy Gateway translator test data (https://github.com/envoyproxy/gateway, `internal/xds/translator/testdata`). `Exposure` has no route namespace, so the query wildcards it. Not run live; whether per-cluster stats are on in the default EG proxy bootstrap is unconfirmed. Health `envoy_server_live`.

### nginx-gateway-fabric (unsupported)
The NGF source (https://github.com/nginx/nginx-gateway-fabric) exposes controller metrics (`event_batch_processing_milliseconds`, ...) and server-level NGINX metrics; no per-route or per-upstream request counter was found. Marked `supported: false`.

### aws-alb / aws-nlb (YACE)
Exporter: https://github.com/prometheus-community/yet-another-cloudwatch-exporter. Name rule (`pkg/promutil/migrate.go`): `aws_` + namespace without the `AWS/` prefix, lowercased + snake-cased metric + `_` + statistic. Dimensions become `dimension_<Name>` (so `dimension_LoadBalancer`), plus `name` (ARN), `region`, `account_id`. YACE config: namespace `AWS/ApplicationELB`, metric `RequestCount`, statistics `[Sum]`; and `AWS/NetworkELB`, `NewFlowCount`, `[Sum]`. CloudWatch values are per-period sums that YACE re-exports on every scrape, so they are gauges: `sum_over_time(...) > 0` means traffic; magnitudes are inflated, so the threshold is only meaningful at 0. Health `yace_cloudwatch_requests_total` (a YACE self-metric). Docs: https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-cloudwatch-metrics.html (dimension `LoadBalancer` = `app/<name>/<id>`; NLB `net/<name>/<id>`).

The LB controller annotation `alb.ingress.kubernetes.io/load-balancer-name` sets the name; default is `k8s-<ns>-<ingress>-<10 hex>`.

**`lbName` rule** (status hostname to the name inside `dimension_LoadBalancer`). The ARN's trailing id is not in the hostname, so the query uses `/.*`.

- NLB form `^(?:internal-)?(.+)-[0-9a-f]{16}\.elb\.<region>\.amazonaws\.com(\.cn)?$`: name is group 1.
- ALB / classic form `^(?:internal-|dualstack\.)*(.+)-[0-9]+\.<region>\.elb\.amazonaws\.com(\.cn)?$`: name is group 1.
- Anything else returns `""`; the profile queries then fall back to `vector(1)` (busy).

| Hostname | `lbName` |
|---|---|
| `k8s-default-myingress-0a1b2c3d4e-1234567890.us-east-1.elb.amazonaws.com` | `k8s-default-myingress-0a1b2c3d4e` |
| `internal-k8s-default-myingress-0a1b2c3d4e-1234567890.us-east-1.elb.amazonaws.com` | `k8s-default-myingress-0a1b2c3d4e` |
| `dualstack.k8s-default-myingress-0a1b2c3d4e-1234567890.eu-west-1.elb.amazonaws.com` | `k8s-default-myingress-0a1b2c3d4e` |
| `k8s-default-mysvc-1a2b3c4d5e-0123456789abcdef.elb.us-east-1.amazonaws.com` | `k8s-default-mysvc-1a2b3c4d5e` |
| `fixture-aws-alb-1234567890.us-east-1.elb.amazonaws.com` | `fixture-aws-alb` |

These were run through Go `regexp`. Classic ELBs (metric namespace `AWS/ELB`, dimension `LoadBalancerName`) are not covered. Matching: `aws-alb` matches Ingress class `alb` only, because the same `.elb.amazonaws.com` hostname shows up on an Ingress served by ingress-nginx or Traefik behind an ELB. `aws-nlb` matches a Service carrying a `aws-load-balancer-type` or `-nlb-target-type` annotation; a Service using only `loadBalancerClass: service.k8s.aws/nlb` is unmatched (unknown, safe) until a user adds a profile.

### gcp
Exporter: https://github.com/prometheus-community/stackdriver_exporter. Name rule (README): `stackdriver_<monitored_resource>_<metric_type normalised>`; so `loadbalancing.googleapis.com/https/request_count` on `https_lb_rule` is `stackdriver_https_lb_rule_loadbalancing_googleapis_com_https_request_count` (the README's own example is `..._https_request_bytes_count`; request_count follows the same rule). `https_lb_rule` labels (https://docs.cloud.google.com/monitoring/api/resources): `project_id region url_map_name forwarding_rule_name target_proxy_name matched_url_path_rule backend_target_name backend_target_type backend_name backend_type backend_scope backend_scope_type`; metric labels add `response_code`, `response_code_class`, `cache_result`, `proxy_continent`. Set `--monitoring.metrics-type-prefixes=loadbalancing.googleapis.com/https/request_count` and leave `--monitoring.aggregate-deltas` off, so the delta is exported as a gauge per interval. The Ingress annotation `ingress.kubernetes.io/url-map` is written by ingress-gce (`pkg/annotations/ingress.go`, `UrlMapKey`). One URL map serves the whole Ingress. Health `stackdriver_monitoring_scrapes_total` (`collectors/monitoring_collector.go`).

### gke-gateway (unsupported)
The GKE Gateway controller names its URL maps `gkegw1-<hash>-<ns>-<gateway>-<port>-<hash>` and lists them in the Gateway's `networking.gke.io/url-maps` annotation. That naming comes from blog posts only, and `Exposure` carries no Gateway annotations. To enable: add `GatewayAnnotations map[string]string` to `Exposure` and reuse the gcp metric with `url_map_name`.

### azure
Exporter: https://github.com/webdevops/azure-metrics-exporter. Query `/probe/metrics/list` with `resourceType=Microsoft.Network/loadBalancers`, `metric=ByteCount`, `aggregation=total`, `name=azure_lb`, `template={name}_{metric}_{aggregation}`, `metricFilter=FrontendIPAddress eq '*'`. Metric becomes `azure_lb_bytecount_total` (templated labels are dropped). Labels set in `metrics/insights.target.go`: `resourceID` (lowercased) `resourceName resourceGroup subscriptionID subscriptionName unit interval timespan`; with one dimension, `dimension="<value>"`. Azure reference (https://learn.microsoft.com/azure/azure-monitor/reference/supported-metrics/microsoft-network-loadbalancers-metrics): `ByteCount`, Total, dimensions `FrontendIPAddress FrontendPort Direction Protocol`. Needs a Standard SKU LB. AKS shares one LB per cluster, with one frontend IP per Service, hence the IP join. Health `azurerm_api_ratelimit` (README, only on `/metrics`). The `{metric}` template renders lower-case (README examples); not run.
**azure-agic** is unsupported: `TotalRequests` (dimension `BackendSettingsPool`) is per gateway and the gateway name is not in `Exposure`.

### hetzner
Exporter: https://github.com/wacken89/hetzner-load-balancer-prometheus-exporter (Apache-2.0, pushed 2026-09; `code/exporter.py`). Gauges with labels `hetzner_load_balancer_id`, `hetzner_load_balancer_name`: `hetzner_load_balancer_open_connections`, `_connections_per_second`, `_requests_per_second` (HTTP services only), `_bandwidth_in/out`. Env: `ACCESS_TOKEN`, `LOAD_BALANCER_IDS` (`all` works). Join is the hcloud-CCM annotation `load-balancer.hetzner.cloud/name` (https://github.com/hetznercloud/hcloud-cloud-controller-manager); a Service without it is unmatched. Health `hetzner_load_balancer_open_connections`. Values are the latest 1-minute point, scraped every `SCRAPE_INTERVAL` (30 s default).
Not verified: whether Hetzner health checks count in `connections_per_second` (they run LB to target, so probably not).

### digitalocean (unsupported)
The DO API has `/v2/monitoring/metrics/load_balancer/frontend_http_requests_per_second` (https://docs.digitalocean.com/reference/pydo/reference/monitoring/get_lb_frontend_http_requests_per_second/). Existing exporters only count load balancers (`digitalocean_load_balancers_count` in `digitalocean/digitalocean_exporter`). No maintained exporter publishes per-LB traffic.

## Findings that touch the contracts
See the report back to the lead; summarised there and in the plan's table.
