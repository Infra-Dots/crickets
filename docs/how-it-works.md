# How it works

One run is one pass: discover, ask Prometheus, decide, write, exit. Source of truth: `internal/discovery`, `internal/providers`, `internal/reconcile`.

```
Deployment / StatefulSet
   |  Service selector matches the pod template labels (same namespace)
   v
Service ----------------------------+
   |                                |
   |  type LoadBalancer?            |  Ingress backend, or
   |                                |  HTTPRoute backendRef (-> Gateway -> GatewayClass)
   v                                v
exposure: Service (LB)       exposure: Ingress / HTTPRoute
              \                  /
               v                v
        provider profile (first supported match)
                      |
        health query at now and at now-window
                      |
        PromQL rendered from the profile + exposure
                      |
        value <= threshold ?  ->  idle / active / unknown
```

## The pipeline

1. **List** Deployments and StatefulSets in all namespaces, filtered by `selector` (a label selector on the workload). Namespace `include`/`exclude` apply.
2. **Skip early.** A workload with the annotation `crickets.io/ignore: "true"` is skipped. A workload younger than `minAge` is skipped. Both are only counted.
3. **Find exposures.** For each Service in the workload's namespace whose selector is non-empty and matches the pod template labels:
   - a `type: LoadBalancer` Service is an exposure;
   - each Ingress with that Service as a rule or default backend is an exposure;
   - each HTTPRoute (`gateway.networking.k8s.io/v1`) with that Service in a `backendRef` is an exposure, one per `parentRef`. A `backendRef` may point to another namespace. ReferenceGrants are not checked, because the route still routes.
   A workload with no exposure is **unexposed** and skipped.
4. **Pick a profile.** The Ingress class is `spec.ingressClassName`, else the legacy `kubernetes.io/ingress.class` annotation, else the cluster's default IngressClass (annotation `ingressclass.kubernetes.io/is-default-class: "true"`). For an HTTPRoute, the parentRef is followed to the Gateway, its `gatewayClassName`, and that GatewayClass's `controllerName`. The first supported profile that matches wins (see [match semantics](providers.md#match-semantics)). No match means unknown.
5. **Check health** of the profile's source, once per profile per pass: run the profile's `health` query at `now` and at `now - window`. Both must return a sample.
6. **Render and run** the profile's `query`. An empty result from a healthy source counts as 0. Series are summed.
7. **Decide** per workload (below), then **write**.

If the Gateway API CRDs are not installed, there are simply no HTTPRoutes. That is not an error.

## Verdicts

| Verdict | When | Label |
|---|---|---|
| skipped | namespace not selected, `crickets.io/ignore: "true"`, younger than `minAge`, or no exposure | not touched, even if already labeled |
| active | at least one exposure has `value > threshold` | removed (label and evidence) if present |
| unknown | no exposure is busy, and at least one could not be evaluated | not touched |
| idle | every exposure is known and `value <= threshold` | applied |

Busy beats unknown. If one exposure shows traffic, the workload is active, whatever the others did. A workload is idle only when there is nothing unknown and nothing busy.

An idle workload that is already labeled gets its evidence refreshed when it changes. `since` stays the time of the first idle pass.

## Unknown is never idle

An empty answer can mean "no traffic" or "no data". Crickets treats "no data" as unknown. An exposure is unknown when:

- **No profile.** No supported profile matches: an unrecognised ingress class, a Service without the cloud annotations, an unsupported provider (NGINX Gateway Fabric, GKE Gateway, AGIC, DigitalOcean), or an HTTPRoute whose Gateway or GatewayClass cannot be found or that has no `parentRefs`. Log: `unknown: no provider profile`.
- **Source unhealthy now.** The profile's `health` query has no sample at `now`: exporter down, scrape broken, wrong job name. Log: `unknown: source unhealthy or retention shorter than window`.
- **Retention shorter than the window.** The `health` query has no sample at `now - window`. Prometheus cannot answer a question about a week ago if it keeps three days. The same happens for a source installed less than one window ago. It is the same log line as above.
- **Missing join key.** The query wraps its join key in `need`; the key is empty, so the render fails. Examples: an ALB Ingress with no status hostname yet, a Hetzner Service without the name annotation, an Azure Service without an IP. An empty key would match no series, which reads as zero traffic, so the render fails instead.
- **Query render error.** Any other template error in a profile (typo in a field name, bad `index`). Log: `unknown: query does not render`, at error level.

An unknown workload keeps whatever label it has. Crickets neither adds nor removes it.

## Mixed exposures

A workload behind several exposures needs all of them quiet.

| Ingress | HTTPRoute | Result |
|---|---|---|
| quiet | quiet | idle |
| quiet | busy | active |
| quiet | unknown | unknown |
| busy | unknown | active |

The `idle-evidence` annotation lists every exposure that was evaluated. Exposures that were unknown are not in it.

## Evidence

An idle workload gets the label and an annotation next to it. The annotation key is the label's prefix plus `idle-evidence` (`crickets.io/idle-evidence` by default).

```yaml
metadata:
  labels:
    crickets.io/idle: "true"
  annotations:
    crickets.io/idle-evidence: '{"since":"2026-11-08T06:00:00.123456Z","window":"604800s","threshold":0,"exposures":[{"kind":"Ingress","name":"web","provider":"traefik","query":"sum(increase(traefik_service_requests_total{service=~\"shop-web-.*@kubernetes\"}[604800s]))","value":0}]}'
```

| Field | Meaning |
|---|---|
| `since` | When this pass first found the workload idle. Kept across passes while it stays labeled. |
| `window` | The lookback as whole seconds, a valid PromQL duration. |
| `threshold` | The configured threshold. |
| `exposures[].kind` | `Ingress`, `HTTPRoute` or `Service`. |
| `exposures[].name` | The Ingress, route or Service name. |
| `exposures[].provider` | The profile name that matched. |
| `exposures[].query` | The exact PromQL that ran. Paste it into Prometheus to check. |
| `exposures[].value` | The summed result. |

## When the label is removed

Crickets applies an empty `metadata` body under its field manager, which releases the label and the evidence annotation, when:

- the verdict is **active**: some exposure got more than `threshold` over the window;
- the workload now has `crickets.io/ignore: "true"`, so the opt-out also stops a cleanup that was already queued;
- the workload lost its last exposure.

The label stays as it is when the workload is **unknown** (no data is not a reason to write), younger than `minAge`, or outside the selected namespaces or selector (Crickets doesn't look at those). Only fields Crickets owns are released. A label someone set by hand stays.

## Safety properties

- **Prometheus error: zero writes.** Every decision is made before any write. A failed health or traffic query aborts the pass, logs `pass failed`, exits 1. Nothing is labeled or unlabeled. A Kubernetes list error does the same.
- **Dry run is the default.** `dryRun: true` logs `would label` / `would unlabel` and writes nothing. The config parser sets it before reading your file, so omitting the key keeps it on.
- **Metadata only.** Server-side apply sends `apiVersion`, `kind`, `metadata.name`, `namespace`, labels and annotations. The pod template is untouched, so nothing rolls out.
- **Per-write errors don't stop the pass.** If one write fails (RBAC, admission webhook), the rest still run, the failures are counted in `writeErrors`, and the exit code is 1.
- **Never deletes.** The RBAC is `get,list,patch` on Deployments and StatefulSets, and `get,list` on the rest (`charts/crickets/templates/rbac.yaml`). There is no `delete` verb.

## Exit codes and logs

| Code | Meaning |
|---|---|
| 0 | Pass finished (dry or not). |
| 1 | Config or providers invalid, Prometheus or Kubernetes error, or at least one write failed. |

Logs are JSON on stderr. The last line is `pass complete` with `dryRun`, `workloads`, `idle`, `active`, `unknown`, `unexposed`, `ignored`, `young`, `labeled`, `unlabeled`, `writeErrors`. `workloads` counts only exposed workloads that were evaluated.
