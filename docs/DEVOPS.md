# DevOps Runbook

For a reproducible Linux-based toolchain, see
[`DEVCONTAINER.md`](DEVCONTAINER.md). Its post-create step validates
configuration only; cluster creation and infrastructure changes remain
explicit operator actions.

This repository ships three deployment paths for the same gateway:

1. **Docker Compose** — one command on a laptop.
2. **Kubernetes (kind + Kustomize)** — the local/CI path, also valid for any
   Kubernetes cluster.
3. **Terraform and Ansible** — reproducible cluster bootstrap and host
   provisioning.

```
             ┌──────────────┐
  client ──▶ │ ingress-nginx│ ──▶ gateway Service (8080)
             └──────────────┘            │
                                         ├──▶ backend-1 Service (8081)
                                         ├──▶ backend-2 Service (8082)
                                         ├──▶ backend-3 Service (8083)
                                         └──▶ redis Service (6379)

  Prometheus ──scrape──▶ gateway /metrics
  Grafana    ──query───▶ Prometheus
```

## Prerequisites

| Tool | Used for | Version |
|------|----------|---------|
| Go | build/tests | 1.26+ |
| Docker + Compose | images, Compose, kind | recent |
| kubectl | cluster access | 1.30+ |
| kind | local Kubernetes | 0.30+ |
| helm | monitoring addons | 3.14+ / 4.x |
| terraform | cluster bootstrap | 1.9+ |
| ansible-core | host provisioning | 2.16+ |

## 1. Docker Compose

```bash
export JWT_SECRET="$(openssl rand -hex 32)"
export API_KEYS="$(openssl rand -hex 32)"
export REDIS_PASSWORD="$(openssl rand -hex 32)"
export GF_SECURITY_ADMIN_PASSWORD="$(openssl rand -hex 32)"
docker compose up -d --build
docker compose ps

curl -H "X-API-KEY: ${API_KEYS%%,*}" http://localhost:8080/users
curl http://localhost:8080/healthz
curl http://localhost:8080/version
```

| Service | URL |
|---------|-----|
| Gateway | http://localhost:8080 |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 (admin / `$GF_SECURITY_ADMIN_PASSWORD`) |
| pprof | http://127.0.0.1:6060 (loopback only) |

Compose requires explicit values for `JWT_SECRET`, `API_KEYS`,
`REDIS_PASSWORD`, and `GF_SECURITY_ADMIN_PASSWORD`; `VERSION`, `GIT_SHA`, and
`BUILD_DATE` remain optional.

Stop and remove volumes with `docker compose down -v`.

## 2. Kubernetes on kind

```bash
make kind-up        # kind cluster + ingress-nginx + metrics-server
make deploy         # build/load images + apply deploy/k8s/overlays/dev
make smoke          # port-forward + health/auth/proxy/metrics checks
make monitoring     # kube-prometheus-stack + ServiceMonitor + rules + dashboard
make kind-down
```

`make kind-e2e` creates an isolated `api-gateway-e2e` cluster, deploys the
application, runs the port-forward smoke test, and checks the ingress endpoint.
It defaults to host ports `18080` and `18443`, so it can run alongside a
cluster using ports `80` and `443`. Override `E2E_CLUSTER_NAME`,
`INGRESS_HTTP_HOST_PORT`, and `INGRESS_HTTPS_HOST_PORT` as needed. Remove only
the test cluster with `make kind-e2e-down`; cleanup restores the default
`kind-api-gateway` context when it is present in the active kubeconfig.

### Access

The `dev` overlay creates random development credentials on first deploy and
retains them on later deploys. `make smoke` reads the API key from the
Kubernetes Secret automatically. Add the ingress host once:

```bash
echo "127.0.0.1 gateway.local" | sudo tee -a /etc/hosts
curl -H "X-API-KEY: <API_KEY>" http://gateway.local/users
```

For the default kind setup, use
`curl -H "X-API-KEY: <API_KEY>" http://gateway.local:18080/users`. Set
`INGRESS_HTTP_HOST_PORT=80` to use the standard HTTP port when it is available.
Or port-forward: `kubectl -n api-gateway port-forward svc/gateway 8080:8080`.

### Overlays

| Overlay | Replicas | Tags | Notes |
|---------|----------|------|-------|
| `dev` | gateway 1, backend 1 each | `dev` | Generates a random Secret during deployment |
| `prod` | gateway 3, backend 2 each | `stable` | Requires managed secrets; HPA 3–10; larger requests/limits |

`kubectl kustomize deploy/k8s/overlays/dev` renders exactly what is applied.
Apply a different overlay with `OVERLAY=prod TAG=v1.2.3 make deploy`.

### Secrets

The `prod` overlay deliberately does not include a Secret. Create it first:

```bash
kubectl create namespace api-gateway

kubectl -n api-gateway create secret generic gateway-secrets \
  --from-literal=jwt-secret="$(openssl rand -hex 32)" \
  --from-literal=api-keys="$(openssl rand -hex 16)" \
  --from-literal=redis-password="$(openssl rand -hex 24)"
```

In a real cluster, use External Secrets, Sealed Secrets, SOPS, or your cloud
secret manager, and do not keep plaintext secrets in git. The dev overlay's
Secret is clearly marked development-only.

### Configuration and secrets flow

- `ConfigMap/gateway-config` holds the non-secret YAML (routes, middleware,
  limits, Redis address).
- The Deployment injects `JWT_SECRET`, `API_KEYS`, `REDIS_PASSWORD`,
  `REDIS_ADDR`, `PORT`, `PPROF_ADDR`, and `SHUTDOWN_DELAY` as environment
  variables; they override the ConfigMap.
- ConfigMap changes are picked up by the gateway's 2-second config watcher and
  hot-reloaded. A secret value change is rolled out by restarting the Deployment.

### Kubernetes production building blocks

- Non-root UID 65532, read-only root filesystem, all capabilities dropped,
  `RuntimeDefault` seccomp, and Restricted Pod Security labels.
- Startup, readiness, and liveness probes on `/healthz` and `/readyz`.
- `SHUTDOWN_DELAY=5s`, 30s termination grace, `maxUnavailable: 0` rolling
  updates, and a PDB (`minAvailable: 1`).
- HPA on CPU 70% and memory 80%, with a 5-minute scale-down stabilization.
- Redis StatefulSet with a 1Gi PVC and AOF enabled.
- NetworkPolicies: default-deny ingress; only ingress-nginx and the
  monitoring/observability namespaces can reach the gateway; only the gateway can
  reach backends/Redis.
- The Ingress exposes only `/users` (the demo route), so `/metrics`, pprof, and
  the operational endpoints are not reachable from outside the cluster. Add a
  path for each new route; never route `/metrics`.
- Resource requests/limits on every container.

### Monitoring

`make monitoring` installs kube-prometheus-stack into `monitoring`, then applies
`deploy/k8s/monitoring`:

- `ServiceMonitor` scraping `gateway:8080/metrics` every 15s.
- `PrometheusRule` with alerts for target down/absent, 5xx rate > 5%, p95
  latency > 1s, rate-limit spikes, open circuit breakers, and 503s.
- A Grafana dashboard config map picked up by the Grafana sidecar.

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
kubectl -n monitoring get secret kube-prometheus-stack-grafana \
  -o jsonpath='{.data.admin-password}' | base64 -d
```

## 3. CI/CD

### Run checks locally

Use separate commands for the main CI checks and the local Kubernetes
deployment test:

```bash
make ci                 # formatting, vet, lint, race/coverage tests, config validation, builds
make cd                 # deploy to an isolated kind cluster and run smoke checks
make cd-down            # delete the test cluster when finished
```

`make ci` requires Go, Docker, kubectl, yamllint, and golangci-lint. It does not
create a cluster. `make cd` tests deployment locally; it is not a run of
the remote GitHub `deploy` workflow.

### GitHub Actions

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yml` | push / PR | gofmt, vet, golangci-lint, race tests, coverage, build, Docker build |
| `security.yml` | push / PR / weekly | CodeQL, govulncheck, Trivy SARIF, gitleaks |
| `e2e-kind.yml` | push / PR | kind cluster → deploy → smoke, Prometheus scrape, Grafana health |
| `devcontainer.yml` | relevant paths / PR | build Dev Container; run Go tests and DevOps validation in Linux |
| `docker-publish.yml` | main / `v*` tags | multi-arch GHCR images, SBOM, provenance, Trivy scan |
| `deploy.yml` | successful image publish / manual | deploy `main` to staging, version tags to production, or manually choose an overlay; wait for rollouts and smoke test |
| `terraform.yml` | `terraform/**` | fmt, init, validate, tflint |
| `terraform-kind.yml` | Terraform/deploy paths / PR | apply Terraform to disposable kind, smoke test, destroy |
| `ansible.yml` | `ansible/**` | yamllint, syntax check, ansible-lint |
| `ansible-idempotence.yml` | Ansible/deploy paths / PR | provision ephemeral Linux runner twice using its Docker daemon; require zero second-run changes |

### Image tags

- `main` → `main`, `latest`, `dev`, `sha-<sha>`
- `vX.Y.Z` → `X.Y.Z`, `X.Y`, `stable`, `sha-<sha>`

### Deploy workflow

After `docker-publish` succeeds, `main` automatically deploys its immutable
`sha-<commit>` image to the `staging` environment. A `vX.Y.Z` tag deploys the
same immutable image to `production`; configure required reviewers on that
environment to gate the production rollout. Both environments need a
`KUBE_CONFIG` secret containing access to the corresponding cluster.

The production environment additionally needs:

| Secret | Value |
|--------|-------|
| `GATEWAY_JWT_SECRET` | HS256 signing secret |
| `GATEWAY_API_KEYS` | comma-separated API keys |
| `GATEWAY_REDIS_PASSWORD` | Redis password |

The workflow creates/updates `gateway-secrets` for production, applies the
selected overlay, pins images to the immutable SHA tag, waits for rollouts, and
runs the smoke test. GHCR images must be public or the target cluster must
already have credentials to pull them. Manual deployment remains available
through the `deploy` workflow's **Run workflow** action.

### Forking

- Set `REGISTRY` for `make deploy` (defaults to `ghcr.io/mohdcode`).
- Set the Terraform `registry` variable.
- Override image names in `deploy/k8s/overlays/*/kustomization.yaml` if you use
  a different registry.
- GHCR image owners must be lowercase; the workflows handle this automatically.

## 4. Terraform

`terraform/` is local-kind based and creates:

1. A 3-node kind cluster with ingress ports.
2. ingress-nginx (hostPort on the ingress-ready node).
3. metrics-server with `--kubelet-insecure-tls` (kind certificate).
4. kube-prometheus-stack (optional, enabled by default).
5. The app images and Kustomize overlay via `scripts/deploy-kind.sh`.
6. ServiceMonitor, PrometheusRule, and the Grafana dashboard.

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars
terraform init
terraform validate
terraform apply
```

Pin `node_image` and chart versions in `terraform.tfvars` for reproducible
clusters. The ingress host ports default to `18080` and `18443`; override
`ingress_http_host_port` and `ingress_https_host_port` to choose other available
ports.
`terraform destroy` deletes the kind cluster.

## 5. Ansible

The playbook is split into two plays:

1. Provision base packages, Docker, kind, kubectl, and Helm.
2. Copy the project to `/opt/api-gateway`, create the cluster, build/deploy the
   images, install monitoring, and run the smoke test.

```bash
ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml

# only redeploy after the host is provisioned
ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml --tags deploy
```

Variables (`ansible/group_vars/all.yml`):

```yaml
kind_cluster_name: api-gateway
gateway_deploy_dir: /opt/api-gateway
gateway_registry: ghcr.io/mohdcode
gateway_image_tag: dev
gateway_overlay: dev
gateway_install_monitoring: true
```

The default inventory targets localhost with `ansible_connection=local` and uses
`become: true`; it expects a Linux host with a running Docker daemon. For a
remote VM, uncomment the example host in `ansible/inventory/hosts.ini`.

## 6. Production checklist

- [ ] Replace the dev Secret with a managed secret source.
- [ ] Pin image tags by digest for production deploys.
- [ ] Terminate TLS at the ingress/load balancer and redirect HTTP to HTTPS.
- [ ] Restrict `/metrics` (NetworkPolicy is included) or add auth at ingress.
- [ ] Set `PPROF_ADDR=off` unless you need profiling.
- [ ] Replace the single Redis StatefulSet with HA/managed Redis.
- [ ] Set `JWT_SECRET` to a random 32-byte value and rotate API keys.
- [ ] Configure backup/retention for Redis and Prometheus.
- [ ] Set resource requests based on load testing and keep HPA targets.
- [ ] Add PodDisruptionBudgets for backends if they run multiple replicas.
- [ ] Review NetworkPolicy namespaces if your ingress/monitoring names differ.

## 7. Troubleshooting

| Symptom | Check |
|---------|-------|
| Pods `ImagePullBackOff` | Private GHCR packages: add `imagePullSecrets` or make packages public |
| Gateway starts but Redis warnings | Redis startup ordering; requests fail open, then reconnect. `kubectl -n api-gateway logs statefulset/redis` |
| 429s from smoke test | `rate_limit_rps` / `rate_limit_burst` in the ConfigMap |
| 503s | `gateway_circuit_breaker_state` or all backends unhealthy |
| Ingress 404 | `kubectl -n ingress-nginx get pods`; check Host header/`gateway.local` |
| HPA `<unknown>` | metrics-server not installed; `make kind-up` installs it |
| PVC pending | No default StorageClass; install local-path-provisioner or set `storageClassName` |
| Secret missing on prod deploy | Create `api-gateway/gateway-secrets` (see above) |
| kind port conflict | Change `extraPortMappings` in `deploy/kind/kind-config.yaml` |
| Docker build TLS timeout to `proxy.golang.org` | `DOCKER_BUILD_FLAGS=--network=host make deploy` (Docker Desktop/VM networking) |
