#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="${NAMESPACE:-api-gateway}"
API_KEY="${API_KEY:-}"
LOCAL_PORT="${LOCAL_PORT:-18081}"
BASE_URL="http://127.0.0.1:${LOCAL_PORT}"

command -v kubectl >/dev/null 2>&1 || { echo "error: kubectl is required" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "error: curl is required" >&2; exit 1; }
if [[ -z "${API_KEY}" ]]; then
  command -v openssl >/dev/null 2>&1 || { echo "error: openssl is required to read the API key from Kubernetes" >&2; exit 1; }
  encoded_api_keys="$(kubectl -n "${NAMESPACE}" get secret gateway-secrets -o jsonpath='{.data.api-keys}')"
  api_keys="$(printf '%s' "${encoded_api_keys}" | openssl base64 -d -A)"
  API_KEY="${api_keys%%,*}"
  if [[ -z "${API_KEY}" ]]; then
    echo "error: Secret ${NAMESPACE}/gateway-secrets has no API key" >&2
    exit 1
  fi
fi

kubectl -n "${NAMESPACE}" port-forward svc/gateway "${LOCAL_PORT}:8080" >/tmp/api-gateway-portforward.log 2>&1 &
PF_PID=$!
trap 'kill "${PF_PID}" >/dev/null 2>&1 || true' EXIT

ready=false
for _ in $(seq 1 60); do
  if curl -fsS "${BASE_URL}/healthz" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "${ready}" != "true" ]]; then
  echo "error: gateway did not become reachable" >&2
  cat /tmp/api-gateway-portforward.log >&2 || true
  exit 1
fi

echo "healthz:  $(curl -fsS "${BASE_URL}/healthz")"
echo "readyz:   $(curl -fsS "${BASE_URL}/readyz")"
echo "version:  $(curl -fsS "${BASE_URL}/version")"

unauthorized="$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/users")"
if [[ "${unauthorized}" != "401" ]]; then
  echo "error: unauthenticated request returned ${unauthorized}, expected 401" >&2
  exit 1
fi
echo "auth:     unauthenticated request rejected with 401"

for _ in $(seq 1 30); do
  body="$(curl -fsS -H "X-API-KEY: ${API_KEY}" "${BASE_URL}/users" || true)"
  if [[ "${body}" == *"Hello from the backend"* ]]; then
    echo "proxy:    ${body}"
    curl -fsS "${BASE_URL}/metrics" | grep -q "http_request_counter" || {
      echo "error: /metrics does not expose http_request_counter" >&2
      exit 1
    }
    echo "metrics:  http_request_counter present"
    echo "smoke test passed"
    exit 0
  fi
  sleep 1
done

echo "error: no successful proxied response from /users" >&2
exit 1
