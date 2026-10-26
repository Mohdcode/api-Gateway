#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-api-gateway}"
INGRESS_HTTP_HOST_PORT="${INGRESS_HTTP_HOST_PORT:-18080}"
OVERLAY="${OVERLAY:-dev}"
REGISTRY="${REGISTRY:-ghcr.io/mohdcode}"
TAG="${TAG:-dev}"
INSTALL_MONITORING="${INSTALL_MONITORING:-false}"
MONITORING_NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
KUBE_PROM_STACK_VERSION="${KUBE_PROM_STACK_VERSION:-92.13.1}"
SKIP_BUILD="${SKIP_BUILD:-false}"
DOCKER_BUILD_FLAGS="${DOCKER_BUILD_FLAGS:-}"

GIT_SHA="$(git -C "${ROOT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "error: $1 is required but not installed" >&2
    exit 1
  }
}

require docker
require kind
require kubectl

if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  "${ROOT_DIR}/scripts/kind-up.sh"
fi

GATEWAY_IMAGE="${REGISTRY}/api-gateway:${TAG}"
BACKEND_IMAGES=()
for n in 1 2 3; do
  BACKEND_IMAGES+=("${REGISTRY}/api-gateway-backend-${n}:${TAG}")
done

if [[ "${OVERLAY}" == "prod" ]] && ! kubectl -n api-gateway get secret gateway-secrets >/dev/null 2>&1; then
  echo "error: Secret api-gateway/gateway-secrets is required for the prod overlay." >&2
  echo "create it with: kubectl create namespace api-gateway && kubectl -n api-gateway create secret generic gateway-secrets \\" >&2
  echo "  --from-literal=jwt-secret=... --from-literal=api-keys=... --from-literal=redis-password=..." >&2
  exit 1
fi

if [[ "${OVERLAY}" == "dev" ]]; then
  provided_credentials=0
  for value in "${JWT_SECRET:-}" "${API_KEYS:-}" "${REDIS_PASSWORD:-}"; do
    if [[ -n "${value}" ]]; then
      provided_credentials=$((provided_credentials + 1))
    fi
  done
  if [[ "${provided_credentials}" -ne 0 && "${provided_credentials}" -ne 3 ]]; then
    echo "error: set JWT_SECRET, API_KEYS, and REDIS_PASSWORD together, or leave all unset" >&2
    exit 1
  fi

  kubectl apply -f "${ROOT_DIR}/deploy/k8s/base/namespace.yaml"
  if [[ "${provided_credentials}" -eq 0 ]] && kubectl -n api-gateway get secret gateway-secrets >/dev/null 2>&1; then
    echo "using existing development Secret api-gateway/gateway-secrets"
  else
    require openssl
    JWT_SECRET="${JWT_SECRET:-$(openssl rand -hex 32)}"
    API_KEYS="${API_KEYS:-$(openssl rand -hex 32)}"
    REDIS_PASSWORD="${REDIS_PASSWORD:-$(openssl rand -hex 32)}"
    kubectl -n api-gateway create secret generic gateway-secrets \
      --from-literal=jwt-secret="${JWT_SECRET}" \
      --from-literal=api-keys="${API_KEYS}" \
      --from-literal=redis-password="${REDIS_PASSWORD}" \
      --dry-run=client -o yaml | kubectl apply -f -
  fi
fi

docker_build() {
  if [[ -n "${DOCKER_BUILD_FLAGS}" ]]; then
    # shellcheck disable=SC2086
    docker build ${DOCKER_BUILD_FLAGS} "$@"
  else
    docker build "$@"
  fi
}

if [[ "${SKIP_BUILD}" != "true" ]]; then
  echo "building ${GATEWAY_IMAGE}"
  docker_build \
    --build-arg VERSION="${TAG}" \
    --build-arg COMMIT="${GIT_SHA}" \
    --build-arg BUILD_DATE="${BUILD_DATE}" \
    -t "${GATEWAY_IMAGE}" \
    "${ROOT_DIR}"

  for n in 1 2 3; do
    image="${REGISTRY}/api-gateway-backend-${n}:${TAG}"
    echo "building ${image}"
    docker_build \
      -f "${ROOT_DIR}/Dockerfile.backend" \
      --build-arg BACKEND="backend${n}" \
      -t "${image}" \
      "${ROOT_DIR}"
  done

  echo "loading images into kind cluster ${CLUSTER_NAME}"
  kind load docker-image --name "${CLUSTER_NAME}" "${GATEWAY_IMAGE}" "${BACKEND_IMAGES[@]}"
fi

echo "applying deploy/k8s/overlays/${OVERLAY}"
kubectl apply -k "${ROOT_DIR}/deploy/k8s/overlays/${OVERLAY}"

set_image_if_needed() {
  local resource="$1"
  local name="$2"
  local container="$3"
  local image="$4"
  local current_image

  current_image="$(kubectl -n api-gateway get "${resource}" "${name}" \
    -o jsonpath='{.spec.template.spec.containers[0].image}')"
  if [[ "${current_image}" != "${image}" ]]; then
    kubectl -n api-gateway set image "${resource}/${name}" "${container}=${image}"
  fi
}

set_image_if_needed deployment gateway gateway "${GATEWAY_IMAGE}"
for n in 1 2 3; do
  set_image_if_needed deployment "backend-${n}" backend "${REGISTRY}/api-gateway-backend-${n}:${TAG}"
done

kubectl -n api-gateway rollout status statefulset/redis --timeout=180s
for n in 1 2 3; do
  kubectl -n api-gateway rollout status "deployment/backend-${n}" --timeout=180s
done
kubectl -n api-gateway rollout status deployment/gateway --timeout=180s

if [[ "${INSTALL_MONITORING}" == "true" ]]; then
  require helm
  echo "installing kube-prometheus-stack ${KUBE_PROM_STACK_VERSION}"
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
  helm repo update prometheus-community >/dev/null
  helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
    --namespace "${MONITORING_NAMESPACE}" \
    --create-namespace \
    --version "${KUBE_PROM_STACK_VERSION}" \
    --set grafana.sidecar.dashboards.enabled=true \
    --set grafana.sidecar.dashboards.searchNamespace=ALL \
    --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
    --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false \
    --wait --timeout 10m

  kubectl apply -k "${ROOT_DIR}/deploy/k8s/monitoring"
  echo "Grafana admin password:"
  echo "  kubectl -n ${MONITORING_NAMESPACE} get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d"
fi

echo
echo "deployment complete"
echo "  gateway service: kubectl -n api-gateway port-forward svc/gateway 8080:8080"
echo "  ingress:         curl -H 'Host: gateway.local' -H 'X-API-KEY: <API_KEY>' http://127.0.0.1:${INGRESS_HTTP_HOST_PORT}/users"
echo "  smoke test:      ./scripts/smoke-test.sh"
if [[ "${INSTALL_MONITORING}" == "true" ]]; then
  echo "  prometheus:      kubectl -n ${MONITORING_NAMESPACE} port-forward svc/kube-prometheus-stack-prometheus 9090:9090"
  echo "  grafana:         kubectl -n ${MONITORING_NAMESPACE} port-forward svc/kube-prometheus-stack-grafana 3000:80"
fi
