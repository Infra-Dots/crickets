# Crickets

Your load balancer hears crickets. Crickets finds Deployments and StatefulSets whose Ingress, Gateway API HTTPRoute or LoadBalancer Service got no traffic in Prometheus over a window, and labels them `crickets.io/idle=true`. It reports "no data" separately from "zero traffic", so a broken exporter never looks like an idle app. What happens to the label is up to you: a human, kube-janitor, a policy engine, an alert.

## What it does, and what it never does

It does:

- Run as a Kubernetes CronJob: one pass, then exit. No operator, no leader election, no state.
- Walk workload, Service, Ingress / HTTPRoute / LoadBalancer Service, then ask Prometheus about the edge in front of it.
- Add the label `crickets.io/idle=true` and an evidence annotation to a workload only when **every** exposure in front of it is known and quiet.
- Remove both when traffic comes back, the workload is opted out, or it is no longer exposed.

It never:

- deletes anything,
- scales anything,
- restarts a pod or touches the pod template.

It only writes `metadata.labels` and `metadata.annotations`, through server-side apply with field manager `crickets` (`internal/reconcile/writer.go`). It starts in `dryRun: true`: it logs what it would do and writes nothing. If Prometheus errors, it writes nothing and exits 1.

## Supported providers

Confidence comes from [docs/providers-research.md](docs/providers-research.md). "Verified live" means a query ran against a real cluster. "From source" means the metric and label names come from the exporter or controller source and docs. Cloud profiles are tested against fixture metrics with real label shapes, not live load balancers.

| Provider | Exposure | Confidence |
|---|---|---|
| Traefik (k3s default) | Ingress | verified live |
| Traefik Gateway API | HTTPRoute | verified live |
| ingress-nginx | Ingress | from source |
| Kong | Ingress | from source |
| Kong Gateway API | HTTPRoute | from source |
| Istio | Ingress | from source |
| Istio Gateway API | HTTPRoute | from source, gateway deployment name unverified |
| Envoy Gateway | HTTPRoute | from source, not run live |
| AWS ALB (YACE) | Ingress, class `alb` | from source |
| AWS NLB (YACE) | Service | from source |
| GCP HTTP(S) LB (stackdriver_exporter) | Ingress | from source |
| Azure Load Balancer | Service | from source |
| Hetzner Load Balancer | Service | from source |
| NGINX Gateway Fabric | HTTPRoute | unsupported |
| GKE Gateway | HTTPRoute | unsupported |
| Azure Application Gateway (AGIC) | Ingress | unsupported |
| DigitalOcean Load Balancer | Service | unsupported |

An exposure with no matching supported profile makes the workload **unknown**, never idle. Details, metrics and queries are in [docs/providers.md](docs/providers.md).

## Quickstart

You need a cluster, a Prometheus that scrapes your ingress controller (or the cloud exporter for your load balancer), and Helm 3.8 or later (for OCI charts).

Install the chart from GHCR. It starts in dry run.

```sh
helm install crickets oci://ghcr.io/infra-dots/charts/crickets \
  --namespace crickets --create-namespace \
  --set config.prometheus.url=http://prometheus.monitoring:9090
```

Each `v*` tag publishes the image `ghcr.io/infra-dots/crickets:<version>` (amd64 and arm64) and the chart at the same version. To run a local build instead, `make image TAG=dev`, load it into the cluster (`k3d image import`, `kind load docker-image`), and install `./charts/crickets` with `--set image.tag=dev`.

Trigger a run now and read the logs (JSON, on stderr).

```sh
kubectl -n crickets create job --from=cronjob/crickets crickets-manual
kubectl -n crickets logs job/crickets-manual
```

Look for `would label` lines (each carries its evidence) and the closing `pass complete` line with the counts. Lines starting `unknown:` tell you what to fix.

When the logs look right, turn dry run off and run again.

```sh
helm upgrade crickets ./charts/crickets -n crickets --reuse-values --set config.dryRun=false
kubectl -n crickets delete job crickets-manual
kubectl -n crickets create job --from=cronjob/crickets crickets-manual
```

List what got labeled.

```sh
kubectl get deploy,sts -A -l crickets.io/idle=true
```

Defaults: a 7 day window (`168h`), and workloads younger than the window are skipped. Everything else is in [docs/configuration.md](docs/configuration.md).

## How it compares

Nothing else we found does this specific join (workload, exposure, LB or ingress metric, label). Checked 2026-11-08.

| Tool | What it does | How it differs |
|---|---|---|
| [kor](https://github.com/yonahd/kor) | Lists unused ConfigMaps, Secrets, Services, Deployments and more; can delete with `--delete` | Looks at references, not traffic |
| [useless-operator](https://pkg.go.dev/github.com/Nastradamus/useless-operator) | Uses Prometheus to find pods without traffic | Small and early-stage, pod level, no LB or ingress providers |
| [Kubecost Abandoned Workloads](https://docs.kubecost.com/using-kubecost/navigating-the-kubecost-ui/savings/abandoned-workloads) | Flags workloads below a bytes/s threshold | Commercial, reports only, uses pod network bytes (health checks and scrapes count as traffic), no label |
| [K8sPurger](https://github.com/yogeshkk/K8sPurger) | Exports unused resources as metrics | No traffic signal |
| [kube-janitor](https://codeberg.org/hjacobs/kube-janitor) | Deletes by TTL | Not idle detection. A good consumer of our label, see [acting on the label](docs/acting-on-the-label.md) |

kube-green and the KEDA HTTP add-on sleep or scale to zero on a schedule or on traffic; they are not idle detection either.

## Docs

- [How it works](docs/how-it-works.md): the decision pipeline, verdicts, why unknown is never idle.
- [Configuration](docs/configuration.md): every key, the Helm values, a Prometheus bearer token.
- [Providers](docs/providers.md): per-provider setup, metrics, queries, and how to write your own profile.
- [Acting on the label](docs/acting-on-the-label.md): list, alert, clean up, review by hand.
- [Development](docs/development.md): layout, tests, e2e.
- [Provider research](docs/providers-research.md): the evidence behind each profile.

## License

Apache License 2.0. See [LICENSE](LICENSE).
