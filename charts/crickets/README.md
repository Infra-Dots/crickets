# crickets chart

CronJob that labels idle Deployments/StatefulSets (`crickets.io/idle=true`). It never deletes.
Starts in `dryRun`. See the [project README](https://github.com/Infra-Dots/crickets).

```sh
helm install crickets ./charts/crickets --set config.prometheus.url=http://prometheus:9090
```

| Value | Default | Description |
|---|---|---|
| `schedule` | `0 */6 * * *` | CronJob schedule |
| `activeDeadlineSeconds` | `null` | Hard limit for one run |
| `successfulJobsHistoryLimit` / `failedJobsHistoryLimit` | `3` / `3` | Jobs kept |
| `image.repository` / `image.tag` / `image.pullPolicy` | `ghcr.io/infra-dots/crickets` / appVersion / `IfNotPresent` | Image |
| `imagePullSecrets` | `[]` | Pull secrets |
| `nameOverride` / `fullnameOverride` | `""` | Name overrides |
| `serviceAccount.create` / `serviceAccount.name` | `true` / `""` | ServiceAccount |
| `config.prometheus.url` | `http://prometheus:9090` | Prometheus base URL |
| `config.prometheus.bearerTokenFile` | `""` | Path to a token file (mount it with `extraVolumes`) |
| `config.prometheus.timeout` | `30s` | Query timeout |
| `config.window` | `168h` | Lookback for traffic |
| `config.minAge` | `168h` | Skip workloads younger than this |
| `config.threshold` | `0` | Idle if traffic <= this over the window |
| `config.dryRun` | `true` | Log only, write nothing |
| `config.namespaces.include` / `.exclude` | `[]` / `[kube-system, kube-public]` | Namespace filters |
| `config.selector` | `""` | Optional opt-in label selector |
| `config.label` | `crickets.io/idle` | Label applied to idle workloads |
| `config.providers` | `[]` | Extra or overriding provider profiles |
| `resources` | req 50m/64Mi, limit 128Mi memory | Container resources |
| `podSecurityContext` / `securityContext` | non-root 65532, read-only FS, drop ALL, seccomp RuntimeDefault | Hardening |
| `nodeSelector` / `tolerations` / `affinity` | empty | Scheduling |
| `podAnnotations` / `podLabels` | `{}` | Pod metadata |
| `extraEnv` / `extraVolumes` / `extraVolumeMounts` | empty | Extras, e.g. a Prometheus token Secret |

RBAC is a ClusterRole: `get,list` on deployments, statefulsets, services, ingresses, ingressclasses,
httproutes, gateways, gatewayclasses; `patch` on deployments and statefulsets.

Tests: `charts/crickets/tests/check.sh` (lint + golden diff; `UPDATE=1` rewrites goldens).
