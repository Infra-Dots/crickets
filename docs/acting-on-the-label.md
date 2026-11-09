# Acting on the label

Crickets only labels. This page shows how to see the labels and some ways to use them. The cleanup examples are examples: adapt them, and test them on a scratch cluster first. They have not been tested with Crickets.

## List what is labeled

```sh
kubectl get deploy,sts -A -l crickets.io/idle=true
```

Read the evidence for one workload.

```sh
kubectl -n shop get deploy web -o jsonpath='{.metadata.annotations.crickets\.io/idle-evidence}' | jq
```

Find how long each has been idle (`since` is the first idle pass).

```sh
kubectl get deploy,sts -A -l crickets.io/idle=true -o json \
  | jq -r '.items[] | [.kind, .metadata.namespace, .metadata.name,
      (.metadata.annotations["crickets.io/idle-evidence"] | fromjson | .since)] | @tsv'
```

If you changed `label` in the config, use that key. The evidence annotation uses the label's prefix.

## Alert on it with kube-state-metrics

kube-state-metrics exposes workload labels only if you allow them. Add this to its arguments.

```
--metric-labels-allowlist=deployments=[crickets.io/idle],statefulsets=[crickets.io/idle]
```

The label then appears on `kube_deployment_labels` and `kube_statefulset_labels`, with the key sanitised to `label_crickets_io_idle`.

kube-state-metrics reads a single `--metric-labels-allowlist`. If you already pass one (kube-prometheus-stack sets `nodes=[...]`), append to it rather than adding a second flag: `--metric-labels-allowlist=nodes=[...],deployments=[crickets.io/idle],statefulsets=[crickets.io/idle]`. Checked against kube-state-metrics v2.17.0: both metrics are `[STABLE]` gauges, and `kubernetes.io/arch` comes out as `label_kubernetes_io_arch`.

```promql
kube_deployment_labels{label_crickets_io_idle="true"}
or
kube_statefulset_labels{label_crickets_io_idle="true"}
```

An example alert rule (Prometheus rules file):

```yaml
groups:
  - name: crickets
    rules:
      - alert: WorkloadIdle
        expr: |
          kube_deployment_labels{label_crickets_io_idle="true"}
          or
          kube_statefulset_labels{label_crickets_io_idle="true"}
        for: 24h
        labels:
          severity: info
        annotations:
          summary: "{{ $labels.namespace }}/{{ $labels.deployment }}{{ $labels.statefulset }} had no edge traffic for the Crickets window"
```

The label is removed when traffic comes back, the workload is opted out, or it loses its last exposure, so the alert resolves by itself. A `for` longer than the CronJob period avoids flapping around a single pass.

## Delete after a grace period (examples)

Pick the consumer you already run. Deleting is the consumer's job and its RBAC, not Crickets'. Keep a grace period after the label appears so owners can object. Crickets does not record who was told.

### kube-janitor

[kube-janitor](https://codeberg.org/hjacobs/kube-janitor) takes rules with `id`, `resources`, `jmespath` and `ttl`, from `--rules-file`. It also has `--dry-run`, which prints what it would do. Start with that.

```yaml
rules:
  - id: delete-crickets-idle
    resources:
      - deployments
      - statefulsets
    jmespath: 'metadata.labels."crickets.io/idle" == `"true"`'
    ttl: 14d
```

Check before use: kube-janitor's TTL is counted from the object's creation time as far as we know, not from when the label appeared. If so, an old workload would be deleted as soon as it is labeled, and `ttl: 14d` gives no grace period. Read its README on `janitor/ttl` and `janitor/expires`, and test with `--dry-run`.

### Kyverno

Kyverno's cleanup policies delete on a schedule. This is the `ClusterCleanupPolicy` shape from the Kyverno docs; it is deprecated from Kyverno 1.18 in favour of `DeletingPolicy`, so check the docs for your version.

```yaml
apiVersion: kyverno.io/v2
kind: ClusterCleanupPolicy
metadata:
  name: delete-crickets-idle
spec:
  match:
    any:
      - resources:
          kinds:
            - Deployment
          selector:
            matchLabels:
              crickets.io/idle: "true"
  schedule: "0 4 * * 0"
```

Kyverno's cleanup controller needs `delete` on the target kind (here `deployments` in the `apps` group), through a ClusterRole you add. There is no grace period in this policy either. A `conditions` block on the evidence annotation can add one; see the Kyverno docs on cleanup conditions.

## Review by hand before deleting

For each labeled workload:

1. Read the evidence: `kubectl get ... -o jsonpath=...` as above. Check `since`, `window`, and that `exposures[].value` is 0 or under the threshold.
2. Paste each `exposures[].query` into Prometheus. Confirm it returns what the evidence says, and look at a longer range.
3. Ask whether the window covers the workload's real cycle. A monthly batch UI or a quarter-end tool is idle for weeks.
4. Check what Crickets cannot see: traffic that skips the load balancer, such as in-cluster calls straight to the ClusterIP Service, port-forwards, queue consumers, cron-style workers, gRPC through a path you do not scrape.
5. Check whether the workload has other duties: a StatefulSet holding data, a Deployment other services depend on, a webhook backend.
6. Look at the owner: labels, the Git history, the team channel. Ask before you delete.
7. Scale to zero first and wait a cycle, so a mistake is a `kubectl scale` away rather than a restore.
8. Delete from Git if the workload is managed there, so the next sync does not bring it back.
