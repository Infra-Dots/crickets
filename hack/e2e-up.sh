#!/usr/bin/env bash
# Bring up the e2e cluster. Idempotent. Never touches your current kubectl context.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=crickets-e2e
K3S_IMAGE=${K3S_IMAGE:-rancher/k3s:v1.35.5-k3s1}
GATEWAY_API_VERSION=v1.4.0
E2E_PORT=${E2E_PORT:-18080}
TAG=${TAG:-e2e}
IMAGE=${IMAGE:-ghcr.io/infra-dots/crickets}
export KUBECONFIG=${KUBECONFIG:-$PWD/.e2e/kubeconfig}
mkdir -p "$(dirname "$KUBECONFIG")"
k="kubectl --kubeconfig $KUBECONFIG"

if ! k3d cluster list "$CLUSTER" >/dev/null 2>&1; then
  k3d cluster create "$CLUSTER" --image "$K3S_IMAGE" --wait \
    --kubeconfig-update-default=false --kubeconfig-switch-context=false \
    -p "${E2E_PORT}:80@loadbalancer"
fi
k3d kubeconfig get "$CLUSTER" >"$KUBECONFIG"

docker build -t "$IMAGE:$TAG" .
k3d image import -c "$CLUSTER" "$IMAGE:$TAG"

# Gateway API CRDs, then the Traefik gateway provider.
# From the Go module proxy, not GitHub release downloads (flaky in CI).
gw=$(go mod download -json "sigs.k8s.io/gateway-api@${GATEWAY_API_VERSION}" | jq -r .Dir)
$k apply -f "$gw/config/crd/standard/"
$k wait --for=condition=Established crd/gateways.gateway.networking.k8s.io crd/httproutes.gateway.networking.k8s.io --timeout=60s
until $k -n kube-system get deploy traefik >/dev/null 2>&1; do sleep 2; done
$k apply -f test/e2e/infra/traefik-gateway-config.yaml
# The helm controller rolls Traefik; wait until the new spec is in before waiting on the rollout.
until $k -n kube-system get deploy traefik -o jsonpath='{.spec.template.spec.containers[0].args}' | grep -q kubernetesgateway; do sleep 2; done
$k -n kube-system rollout status deploy/traefik --timeout=180s

# Fixture exporter serves the fixtures concatenated; shared HELP/TYPE lines would break the scrape.
$k create namespace crickets-system --dry-run=client -o yaml | $k apply -f -
for f in test/e2e/fixtures/*.prom; do cat "$f"; echo; done | awk '!seen[$0]++' >"$(dirname "$KUBECONFIG")/metrics"
$k -n crickets-system create configmap fixtures --from-file=metrics="$(dirname "$KUBECONFIG")/metrics" --dry-run=client -o yaml | $k apply -f -
out=$($k apply -f test/e2e/infra/prometheus.yaml)
echo "$out"
# A changed scrape config needs a new pod.
if grep -q 'configmap/prometheus configured' <<<"$out"; then $k -n crickets-system rollout restart deploy/prometheus; fi
$k -n crickets-system rollout status deploy/prometheus deploy/fixtures --timeout=180s

helm upgrade --install crickets charts/crickets --kubeconfig "$KUBECONFIG" -n crickets-system \
  --set schedule='0 0 31 2 *' --set image.repository="$IMAGE" --set image.tag="$TAG" --set image.pullPolicy=Never \
  --set config.prometheus.url=http://prometheus.crickets-system.svc:9090 \
  --set config.window=2m --set config.minAge=0s --set config.dryRun=false \
  --set 'config.namespaces.include={crickets-e2e}' --wait

# Health checks query now-window, so need more than one window (2m) of data from both sources.
q='count(up{job="traefik"} offset 130s == 1) and count(up{job="fixtures"} offset 130s == 1)'
for _ in $(seq 120); do
  out=$($k get --raw "/api/v1/namespaces/crickets-system/services/prometheus:9090/proxy/api/v1/query?query=$(printf %s "$q" | jq -sRr @uri)" || true)
  case $out in *'"result":[{'*) echo "prometheus has enough history"; exit 0;; esac
  sleep 3
done
echo "prometheus never accumulated a full window" >&2
exit 1
