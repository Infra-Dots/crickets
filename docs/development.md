# Development

## Layout

```
cmd/crickets/        flags (--config, --kubeconfig), wiring, exit codes
internal/config/     load, defaults, validate
internal/discovery/  workloads, Services, Ingresses, HTTPRoutes -> exposures
internal/providers/  embedded profiles.yaml, matching, template render, lbName, need
internal/prom/       instant query at time t -> number or empty
internal/reconcile/  verdicts, evidence, server-side apply
charts/crickets/     Helm chart (CronJob, ServiceAccount, ClusterRole, ConfigMap)
test/e2e/            end-to-end tests, fixtures (*.prom), spike manifests
hack/                e2e cluster scripts
docs/                these docs
tasks/               plan and journal
```

Go dependencies are `client-go`, `sigs.k8s.io/gateway-api`, `prometheus/client_golang` and `sigs.k8s.io/yaml`.

## Unit tests and lint

```sh
make test    # go test -race ./...
make lint    # gofmt check, go vet, golangci-lint
make build   # bin/crickets, CGO off
make image   # docker build, IMAGE and TAG overridable
```

Discovery tests use the client-go fake clientset and the Gateway API fake. Prom tests run against an `httptest` server. Provider tests cover every built-in profile.

To run the binary against a cluster, in dry run:

```sh
./bin/crickets --config ./config.yaml --kubeconfig ~/.kube/config
```

## Changing a built-in profile

Edit `internal/providers/profiles.yaml` and add a row to the test in `internal/providers/providers_test.go`. Update [providers.md](providers.md) and the evidence in [providers-research.md](providers-research.md). A change to a metric name needs a source link.

## Chart golden check

The chart has `helm lint --strict` plus a diff of `helm template` output against stored goldens, for default values and for `charts/crickets/ci/override-values.yaml`.

```sh
charts/crickets/tests/check.sh            # lint and diff
UPDATE=1 charts/crickets/tests/check.sh   # rewrite the goldens after an intended change
```

Review the golden diff in the PR.

## End-to-end tests

See the `Makefile` for the exact targets; this section only names them. The e2e suite runs the real binary image in a k3d cluster (k3s ships Traefik), with a small Prometheus that scrapes Traefik and a fixture exporter serving `test/e2e/fixtures/*.prom` for the cloud profiles.

```sh
make e2e-up     # create the k3d cluster, build and import the image, install Prometheus and the chart
make e2e        # run the scenarios
make e2e-down   # delete the cluster
```

The scenarios cover busy and idle Ingresses and HTTPRoutes, skipped workloads, unknown classes, a mixed busy and idle workload, recovery after traffic, and Prometheus down (no labels changed). Each run is triggered with `kubectl create job --from=cronjob/...` and checked through client-go.

You need Docker, k3d, kubectl and helm.
