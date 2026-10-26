#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-api-gateway}"
INGRESS_HTTP_HOST_PORT="${INGRESS_HTTP_HOST_PORT:-18080}"
INGRESS_HTTPS_HOST_PORT="${INGRESS_HTTPS_HOST_PORT:-18443}"
INGRESS_NGINX_VERSION="${INGRESS_NGINX_VERSION:-1.12.0}"
METRICS_SERVER_VERSION="${METRICS_SERVER_VERSION:-0.7.2}"

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: $1 is required but not installed" >&2
    exit 1
  }
}

require kind
require kubectl

for port in "${INGRESS_HTTP_HOST_PORT}" "${INGRESS_HTTPS_HOST_PORT}"; do
  if ! [[ "${port}" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
    echo "error: ingress host ports must be integers between 1 and 65535" >&2
    exit 1
  fi
done
if [[ "${INGRESS_HTTP_HOST_PORT}" == "${INGRESS_HTTPS_HOST_PORT}" ]]; then
  echo "error: HTTP and HTTPS ingress host ports must be different" >&2
  exit 1
fi

if kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  echo "kind cluster '${CLUSTER_NAME}' already exists"
else
  echo "creating kind cluster '${CLUSTER_NAME}'"
  kind_config="$(mktemp)"
  trap 'rm -f "${kind_config}"' EXIT
  sed \
    -e "s/hostPort: 18080 # HTTP_INGRESS_HOST_PORT/hostPort: ${INGRESS_HTTP_HOST_PORT} # HTTP_INGRESS_HOST_PORT/" \
    -e "s/hostPort: 18443 # HTTPS_INGRESS_HOST_PORT/hostPort: ${INGRESS_HTTPS_HOST_PORT} # HTTPS_INGRESS_HOST_PORT/" \
    "${ROOT_DIR}/deploy/kind/kind-config.yaml" >"${kind_config}"
  kind create cluster --name "${CLUSTER_NAME}" --config "${kind_config}" --wait 180s
fi

kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null

echo "installing ingress-nginx controller v${INGRESS_NGINX_VERSION}"
kubectl apply -f "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v${INGRESS_NGINX_VERSION}/deploy/static/provider/kind/deploy.yaml"
kubectl wait --namespace ingress-nginx \
  --for=condition=Ready pod \
  --selector=app.kubernetes.io/component=controller \
  --timeout=300s

echo "installing metrics-server v${METRICS_SERVER_VERSION}"
kubectl apply -f "https://github.com/kubernetes-sigs/metrics-server/releases/download/v${METRICS_SERVER_VERSION}/components.yaml"
if ! kubectl get deployment metrics-server -n kube-system -o yaml | grep -q -- "--kubelet-insecure-tls"; then
  kubectl patch deployment metrics-server -n kube-system --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
fi
kubectl rollout status deployment/metrics-server -n kube-system --timeout=180s

echo
echo "cluster ready: kind-${CLUSTER_NAME}"
echo "ingress: http://gateway.local:${INGRESS_HTTP_HOST_PORT} (HTTPS host port ${INGRESS_HTTPS_HOST_PORT})"
echo "next: make deploy   (or scripts/deploy-kind.sh)"
