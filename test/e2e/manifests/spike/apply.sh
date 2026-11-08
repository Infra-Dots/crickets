#!/bin/sh
# Verified on k3d (k3s v1.35.5, Traefik 3.6.13). k3s already ships Gateway API CRDs; this pins the version.
set -eu
GATEWAY_API_VERSION=v1.4.0   # standard channel; worked with Traefik 3.6.13
kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
d=$(dirname "$0")
kubectl apply -f "$d/00-app.yaml"
kubectl apply -f "$d/01-traefik-gateway-config.yaml"   # traefik restarts; wait for it
kubectl -n kube-system rollout status deploy/traefik --timeout=180s
kubectl wait --for=condition=Established crd/gateways.gateway.networking.k8s.io --timeout=60s
kubectl apply -f "$d/02-gateway-httproute.yaml" -f "$d/03-unnamed-port-route.yaml"
# Traefik pod: metrics on :9100/metrics, web entrypoint :8000 (Gateway listener port must be 8000).
