# Configuration

Crickets reads one YAML file, `--config` (default `/etc/crickets/config.yaml`). The Helm chart renders `.Values.config` into that file with `toYaml`, so every key below is a Helm value under `config.`. Source: `internal/config/config.go`.

Unknown keys are an error (strict parse). A bad config makes the run exit 1 before it touches the cluster.

```yaml
prometheus:
  url: http://prometheus.monitoring:9090
  bearerTokenFile: ""
  timeout: 30s
window: 168h
minAge: 168h
threshold: 0
dryRun: true
namespaces:
  include: []
  exclude: [kube-system, kube-public]
selector: ""
label: crickets.io/idle
providers: []
```

## Keys

| Key | Type | Default | Validation |
|---|---|---|---|
| `prometheus.url` | string | none | Required. |
| `prometheus.bearerTokenFile` | string | `""` | None. Empty means no auth header. |
| `prometheus.timeout` | duration | `30s` | Must parse as a Go duration. Applies to each query. |
| `window` | duration | `168h` | Go duration, at least `1s`. |
| `minAge` | duration | same as `window` | Go duration, not negative. |
| `threshold` | number | `0` | Not negative. |
| `dryRun` | bool | `true` | None. |
| `namespaces.include` | list of strings | `[]` (all) | None. |
| `namespaces.exclude` | list of strings | `[kube-system, kube-public]` | None. |
| `selector` | string | `""` | Kubernetes label selector syntax. Checked when the run lists workloads, so an error exits 1 at that point. |
| `label` | string | `crickets.io/idle` | None. |
| `providers` | list of profiles | `[]` | Each profile is compiled at start; see [Write your own profile](providers.md#write-your-own-profile). Duplicate names inside `providers` are not rejected; the first one wins. |

Durations are Go durations: `30s`, `90m`, `168h`. There is **no `d` unit**: write `168h`, not `7d`.

### `prometheus.url`

Base URL of Prometheus or a compatible API (Thanos, Mimir, VictoriaMetrics). Crickets calls the instant query endpoint only.

```yaml
prometheus:
  url: http://prometheus-operated.monitoring.svc:9090
```

### `window`

How far back to look for traffic. It is also how far back the health query must still have data. Retention must be at least `window`, or every exposure is unknown (see [how it works](how-it-works.md#unknown-is-never-idle)).

```yaml
window: 336h   # 14 days
```

### `minAge`

Workloads younger than this are skipped. The default is the window: a workload is judged only after it has existed for a full window. Set `minAge: 0s` to judge new workloads (the e2e tests do).

### `threshold`

A workload is busy when an exposure's value is **greater than** the threshold. The unit depends on the profile (`unit` in the profile: requests, flows, bytes, connections-per-second). Keep it at `0` for cloud exporters; see [providers](providers.md#cloud-exporters-are-gauges).

### `dryRun`

`true` logs `would label` and `would unlabel`, and writes nothing. Omit the key and it is `true`.

### `namespaces`

Include empty means every namespace. Exclude is checked after include. When `exclude` is omitted, the default is `[kube-system, kube-public]`. An explicit `exclude: []` excludes nothing.

```yaml
namespaces:
  include: [shop, staging]
  exclude: []
```

### `selector`

A label selector on the Deployment or StatefulSet itself, for opt-in rollout.

```yaml
selector: "team in (web, search),env!=prod"
```

### `label`

The label key set on idle workloads, with value `true`. The evidence annotation sits in the same prefix: everything up to the last `/`, plus `idle-evidence`. `label: acme.io/unused` gives `acme.io/idle-evidence`. A label without a `/` gets the prefix `crickets.io/`.

The key is not validated by Crickets; the API server rejects a malformed one, and every write fails.

### Workload opt-out

Not a config key. Annotate a workload to exclude it forever.

```sh
kubectl annotate deploy/web -n shop crickets.io/ignore=true
```

The annotation name is fixed (`discovery.IgnoreAnnotation`) and does not follow `label`.

### `providers`

Extra or overriding provider profiles. Same schema as the built-ins. See [providers.md](providers.md#write-your-own-profile).

## Helm values

`config.*` maps one to one as above. The rest of the chart (`charts/crickets/values.yaml`):

| Value | Default | Purpose |
|---|---|---|
| `schedule` | `0 */6 * * *` | CronJob schedule. `concurrencyPolicy` is `Forbid`, `backoffLimit` is `0`. |
| `activeDeadlineSeconds` | `null` | Hard limit for one run. |
| `successfulJobsHistoryLimit`, `failedJobsHistoryLimit` | `3`, `3` | Jobs kept. |
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/infra-dots/crickets`, chart `appVersion`, `IfNotPresent` | Image. |
| `imagePullSecrets` | `[]` | Pull secrets. |
| `serviceAccount.create`, `serviceAccount.name` | `true`, `""` | ServiceAccount. |
| `resources` | req `50m`/`64Mi`, limit `128Mi` memory | Container resources. |
| `podSecurityContext`, `securityContext` | non-root 65532, read-only root FS, drop ALL, seccomp RuntimeDefault | Hardening. |
| `nodeSelector`, `tolerations`, `affinity` | empty | Scheduling. |
| `podAnnotations`, `podLabels` | `{}` | Pod metadata. |
| `extraEnv`, `extraVolumes`, `extraVolumeMounts` | empty | Extras, such as a token Secret. |
| `nameOverride`, `fullnameOverride` | `""` | Names. |

`values.schema.json` rejects unknown top-level values and wrong types. The pod gets a `checksum/config` annotation, so a config change shows up in the next scheduled run.

RBAC is a ClusterRole: `get,list,patch` on `deployments` and `statefulsets`; `get,list` on `services`, `ingresses`, `ingressclasses`, `httproutes`, `gateways`, `gatewayclasses`. The Gateway API group is listed even when its CRDs are absent.

## Prometheus bearer token

`bearerTokenFile` is read on every request, so a rotated or projected token keeps working. Mount a Secret and point at the file.

```sh
kubectl -n crickets create secret generic prometheus-token --from-file=token=./token
```

```yaml
# values.yaml
config:
  prometheus:
    url: https://prometheus.example.com
    bearerTokenFile: /var/run/prometheus/token
extraVolumes:
  - name: prometheus-token
    secret:
      secretName: prometheus-token
extraVolumeMounts:
  - name: prometheus-token
    mountPath: /var/run/prometheus
    readOnly: true
```

```sh
helm upgrade --install crickets ./charts/crickets -n crickets -f values.yaml
```

A missing or unreadable token file makes every query fail, so the pass exits 1 and writes nothing. Crickets sends only a bearer token; there is no basic auth or custom CA option.

## Example: weekly, cautious

```yaml
schedule: "0 3 * * *"
config:
  prometheus:
    url: http://prometheus.monitoring:9090
  window: 336h
  namespaces:
    include: [shop, search]
  selector: "crickets=enabled"
  dryRun: false
```
