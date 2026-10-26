# API Gateway (Go)

A config-driven HTTP API gateway with distributed rate limiting, a distributed
circuit breaker, least-connections load balancing, active health checks,
Prometheus metrics, pprof profiling, and hot config reload.

```
Client -> API Gateway (:8080) -> Backend 1 (:8081)
                              -> Backend 2 (:8082)
                              -> Backend 3 (:8083)
```

## Features

- **Authentication** — API keys (`X-API-KEY`) or JWT Bearer tokens. JWTs are
  pinned to **HS256**; other algorithms are rejected. Invalid bearer tokens can
  fall back to a valid API key.
- **Distributed rate limiting** — Redis-backed token bucket implemented in a Lua
  script, so the limit is enforced atomically across every gateway instance.
  Millisecond refill, per-identity/per-route buckets, and a TTL on every key.
  Client identities use keyed HMAC-SHA-256 hashes, so raw API keys are never
  written to Redis and hashes cannot be precomputed without the JWT signing
  secret. Fails **open** if Redis is unavailable and increments
  `gateway_rate_limited_total{reason="redis_error"}`.
- **Distributed circuit breaker** — Redis-backed state machine (closed / open /
  half-open) with atomic Lua transitions and a single in-flight probe when
  half-open. Configurable failure threshold and reset timeout. Fails open on
  Redis errors.
- **Least-connections load balancing** — routes to the healthy backend with the
  fewest active connections; ties rotate round-robin so equally loaded backends
  share traffic. Unhealthy backends are skipped; if none remain the gateway
  returns `503`.
- **Active health checks** — polls each backend's `/health` every 10s and only
  logs when a backend changes state.
- **Observability** — Prometheus counters, histograms and gauges, plus pprof on
  `:6060`.
- **Hot config reload** — watches `config.yaml`, validates the new file, and
  atomically swaps the routing/auth snapshot. Added routes appear, removed routes
  disappear, and an invalid file is rejected without disturbing the running
  configuration.
- **Operational endpoints** — `/healthz` (liveness), `/readyz` (readiness), and
  `/version` (build metadata). `SIGTERM` triggers a graceful drain with a
  configurable `SHUTDOWN_DELAY`, so Kubernetes removes the pod from endpoints
  before connections are closed.
- **Container-native** — multi-stage distroless images, non-root runtime,
  read-only root filesystem, Docker Compose stack, Kustomize manifests, Helm
  monitoring, Terraform, and Ansible.
- **Connection pooling** — one shared transport with large idle pools and tuned
  timeouts.

## Request flow

Middleware is applied in the order listed in `config.yaml`, where the **first
entry is the outermost wrapper**. The shipped configuration is:

```
metrics -> auth_apikey -> rate_limit -> circuit_breaker -> least-conn proxy
```

Putting `metrics` first means rejected requests (`401`, `429`, `503`) are counted
too. Only after all middleware allows a request does the gateway pick a backend,
so unauthenticated callers cannot observe backend health.

## Requirements

- **Go 1.26+** (see `go.mod`)
- **Redis** — required for distributed rate limiting and circuit breaking; the
  gateway still starts and serves traffic (fail-open) if Redis is down
- **Prometheus** — optional
- `curl` for the examples below
- **Deployment (optional):** Docker + Docker Compose, `kubectl`, `kind`,
  `helm`; `terraform` and `ansible-playbook` for the infrastructure paths

For a reproducible Linux-based toolchain, open this repository with
[the Dev Container](docs/DEVCONTAINER.md). It installs the development and
DevOps CLIs but does not provision infrastructure automatically.

## Docker Compose (fastest)

Runs the gateway, three backends, Redis, Prometheus, and Grafana with health
checks and a provisioned dashboard:

```bash
export JWT_SECRET="$(openssl rand -hex 32)"
export API_KEYS="$(openssl rand -hex 32)"
export REDIS_PASSWORD="$(openssl rand -hex 32)"
export GF_SECURITY_ADMIN_PASSWORD="$(openssl rand -hex 32)"
docker compose up -d --build

curl -H "X-API-KEY: ${API_KEYS%%,*}" http://localhost:8080/users
open http://localhost:9090   # Prometheus
open http://localhost:3000   # Grafana (admin / $GF_SECURITY_ADMIN_PASSWORD)
```

Compose requires explicit credentials; generate fresh local-only values as
shown above instead of relying on checked-in defaults.

## Quick start

### 1. Start Redis

```bash
redis-server --daemonize yes
# or: brew services start redis
# or: docker run -d -p 6379:6379 redis:7
```

### 2. Start the backends

```bash
go run ./cmd/backend1   # :8081
go run ./cmd/backend2   # :8082
go run ./cmd/backend3   # :8083
```

### 3. Start the gateway

```bash
go run ./cmd/gateway                       # uses ./config.yaml
go run ./cmd/gateway /path/to/custom.yaml  # custom config
```

### 4. Make requests

```bash
# API key
curl -H "X-API-KEY: abc-123" http://localhost:8080/users

# JWT (HS256, signed with jwt_secret from config.yaml)
curl -H "Authorization: Bearer <token>" http://localhost:8080/users

# Missing/invalid credentials
curl http://localhost:8080/users          # -> 401 Authentication Failed
```

With the shipped config (`:8081` and `:8083` behind `/users`), six sequential
requests alternate between the two backends because both report zero active
connections:

```
Hello from the backend-1 running on port 8081
Hello from the backend-3 running on port 8083
Hello from the backend-1 running on port 8081
...
```

## Testing

The suite uses `miniredis` for Redis and `httptest` for backends, so it needs no
external services:

```bash
go test ./...
go test -race -count=1 -cover ./...
```

Current state: **46 test functions, ~83% statement coverage for the gateway
package**, race-clean. Coverage includes a full-stack HTTP test that runs the
shipped middleware chain and asserts the resulting Prometheus series, tests for
auth, rate limiting, circuit breaking, load balancing, health checks, config
reload, the new operational endpoints, env overrides, and the proxy error paths,
plus a process-level test that builds the real binary and verifies
`/healthz`, `-healthcheck`, and graceful shutdown on `SIGTERM`.

`.github/workflows/ci.yml` runs deployment configuration validation, `gofmt`,
`go vet`, `golangci-lint`, `go build`, and `go test -race -cover` on every push
and pull request. Run `make validate-devops` locally to lint YAML, check shell
syntax, validate Compose, and render the Kubernetes overlays without changing a
cluster. Separate workflows cover CodeQL/govulncheck/Trivy, kind deployment and
monitoring E2E, Dev Container build/tests, Terraform validation and disposable
kind apply/destroy, Ansible lint plus disposable-host idempotence, and image
publishing to GHCR.
Run the main CI checks locally with `make ci` (requires Go, Docker, kubectl,
yamllint, and golangci-lint). This does not create a Kubernetes cluster. To
separately test deployment to a disposable local kind cluster, use
`make cd`; remove that cluster afterward with `make cd-down`. These are local
commands and do not run the remote GitHub deployment workflow.

## Configuration

```yaml
port: ":8080"

# Validates HS256 Bearer tokens. Change this in production.
jwt_secret: "dev-secret-change-me"

redis:
  addr: "localhost:6379"
  password: ""
  db: 0

middleware_config:
  auth_apikey:
    valid_keys:
      - "abc-123"
      - "dwe-569"

services:
  - name: users
    route_pattern: "/users"
    urls:
      - "http://localhost:8081"
      - "http://localhost:8083"

    # First entry = outermost wrapper.
    middleware:
      - "metrics"
      - "auth_apikey"
      - "rate_limit"
      - "circuit_breaker"

    rate_limit_rps: 200            # sustained tokens per second
    rate_limit_burst: 400          # bucket capacity
    cb_failure_threshold: 6        # consecutive 5xx before opening
    cb_reset_timeout_seconds: 15   # open -> half-open delay
```

| Parameter | Description | Default |
|-----------|-------------|---------|
| `port` | Listen address | `:8080` in the shipped config |
| `jwt_secret` | HS256 signing secret | insecure `default_secret_key` (logged warning) |
| `redis.addr` | Redis address | `localhost:6379` |
| `redis.password` / `redis.db` | Redis auth / database | empty / `0` |
| `valid_keys` | Accepted `X-API-KEY` values | none |
| `route_pattern` | Path prefix (longest prefix wins) | required |
| `urls` | Backend base URLs | required, at least one |
| `middleware` | Middleware names, outermost first | none |
| `rate_limit_rps` | Token refill rate | `100` |
| `rate_limit_burst` | Bucket capacity | `200` |
| `cb_failure_threshold` | Consecutive failures before opening | `5` |
| `cb_reset_timeout_seconds` | Open-to-half-open delay | `10` |

Available middleware names: `metrics`, `auth_apikey`, `rate_limit`,
`circuit_breaker`.

### Environment overrides

Environment variables take precedence over the config file, which is what the
container and Kubernetes manifests use to inject secrets without baking them
into a ConfigMap.

| Variable | Overrides | Example |
|----------|-----------|---------|
| `CONFIG_PATH` | Config file path | `/etc/gateway/config.yaml` |
| `PORT` | `port` | `:8080` |
| `JWT_SECRET` | `jwt_secret` | random 32-byte hex |
| `API_KEYS` | `valid_keys` (comma-separated) | `abc-123,dwe-569` |
| `REDIS_ADDR` | `redis.addr` | `redis:6379` |
| `REDIS_PASSWORD` | `redis.password` | from a Secret |
| `REDIS_DB` | `redis.db` | `0` |
| `PPROF_ADDR` | pprof listen address (`off` disables) | `:6060` |
| `SHUTDOWN_DELAY` | delay before draining on `SIGTERM` | `5s` |
| `HEALTHCHECK_ADDR` | address used by `-healthcheck` | `127.0.0.1:8080` |

### Reload behavior

The gateway checks the config file every 2 seconds and reloads when its
modification time changes. A reload:

- rebuilds the service set (new proxies, connection counters start at zero for
  newly built backends),
- replaces the API-key/JWT-secret snapshot,
- rejects duplicate routes, missing URLs, and malformed YAML,
- keeps the previous configuration if anything is invalid.

In-flight requests finish on the old snapshot; new requests use the new one.

## Metrics

`GET /metrics` (Prometheus text format):

| Metric | Type | Labels |
|--------|------|--------|
| `http_request_counter` | counter | `method`, `status`, `service` |
| `http_request_timer` | histogram | `method`, `service` |
| `gateway_rate_limited_total` | counter | `service`, `reason` (`token_bucket`, `redis_error`) |
| `gateway_circuit_breaker_state` | gauge | `service` (`0` closed, `1` open, `2` half-open) |
| `gateway_proxy_canceled_total` | counter | — |

Scrape config is in `prometheus.yml` and alert rules are in
`deploy/monitoring/alerts.yml`:

```bash
docker run -d -p 9090:9090 \
  -v "$(pwd)/prometheus.yml:/etc/prometheus/prometheus.yml" \
  -v "$(pwd)/deploy/monitoring/alerts.yml:/etc/prometheus/alerts.yml" \
  prom/prometheus
```

The Docker Compose and Kubernetes paths provision the same alerts plus a
Grafana dashboard (`deploy/k8s/monitoring/grafana-dashboard.json`) with request
rate, error rate, latency quantiles, throttling, and circuit-breaker panels.

Useful queries:

```promql
rate(http_request_counter[1m])
histogram_quantile(0.95, rate(http_request_timer_bucket[5m]))
sum(rate(gateway_rate_limited_total[1m])) by (service, reason)
gateway_circuit_breaker_state
```

**Security note:** `/metrics` and pprof (`:6060`) are unauthenticated. Bind them
to a private interface or put them behind your infrastructure's access controls.

## Profiling

```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
go tool pprof http://localhost:6060/debug/pprof/heap
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

## Load testing

```bash
wrk -t4 -c100 -d60s -H "X-API-KEY: abc-123" http://localhost:8080/users
```

Note that the shipped config limits each identity to `200 rps` with a `400`
burst, so a load test will mostly see `429`s. Raise `rate_limit_rps` /
`rate_limit_burst` for throughput runs. `wrk_60s.txt` is a historical run from
the author's machine (52k req/s); reproduce it yourself and expect different
numbers.

## Project structure

```
.
├── cmd/
│   ├── gateway/                  # Gateway executable entrypoint
│   ├── backend1/                 # Demo backend executable
│   ├── backend2/                 # Demo backend executable
│   └── backend3/                 # Demo backend executable
├── internal/gateway/             # Gateway implementation and tests
│   ├── config.go                 # Config loading and environment overrides
│   ├── middleware.go             # Auth, metrics, rate limit, circuit breaker
│   ├── operations.go             # Health, readiness, version, metrics handlers
│   ├── runtime.go                # Server lifecycle and health-check command
│   ├── service.go                # Service setup, routing, backend health
│   └── types.go                  # Gateway and configuration types
├── config.yaml                   # Example configuration
├── Dockerfile                    # Multi-stage distroless gateway image
├── Dockerfile.backend            # Demo backend image (BACKEND=backend1|2|3)
├── compose.yaml                  # Gateway + backends + Redis + Prometheus + Grafana
├── Makefile                      # Local, Docker, kind, Terraform, Ansible targets
├── deploy/
│   ├── docker/                   # Container config and Prometheus scrape config
│   ├── kind/kind-config.yaml     # 3-node kind cluster with ingress ports
│   ├── k8s/base/                 # Kustomize base (gateway, backends, Redis, HPA, PDB, policies)
│   ├── k8s/overlays/dev|prod/    # Environment overlays (replicas, images, resources)
│   ├── k8s/monitoring/           # ServiceMonitor, PrometheusRule, Grafana dashboard
│   └── monitoring/               # Standalone Prometheus alerts + Grafana provisioning
├── scripts/                      # kind up/down, deploy, smoke test
├── terraform/                    # kind cluster + ingress/metrics/monitoring addons
├── ansible/                      # Host provisioning + deployment roles
├── prometheus.yml                # Standalone Prometheus scrape config
├── .github/workflows/            # CI, security, e2e, image publish, deploy, IaC
└── go.mod / go.sum
```

## Deployment (DevOps)

### Kubernetes with kind

```bash
make kind-up     # 3-node kind cluster, ingress-nginx, metrics-server
make deploy      # build images, load them, apply the dev overlay
make smoke       # port-forward and verify health, auth, proxying, metrics
make monitoring  # deploy + kube-prometheus-stack + dashboard
make kind-down   # delete the cluster
```

The kind ingress uses host ports `18080` and `18443` by default. Add
`gateway.local` to `/etc/hosts` (`127.0.0.1 gateway.local`) and use
`curl -H "X-API-KEY: abc-123" http://gateway.local:18080/users`, or port-forward:

```bash
kubectl -n api-gateway port-forward svc/gateway 8080:8080
```

`make kind-e2e` uses a separate `api-gateway-e2e` cluster so it does not reuse
or replace the regular `api-gateway` cluster. Run `make kind-e2e-down` to
delete only that test cluster.

For a clear local CI/CD split, run `make ci` for code, config, and image-build
checks, then `make cd` for the Kubernetes deployment and smoke test.
The kind test validates deployment but does not run the remote GitHub `deploy`
workflow or leave a hosted environment running.

Overlays:

| Overlay | Replicas | Image tags | Notes |
|---------|----------|------------|-------|
| `dev` | 1 gateway, 1 per backend | `dev` | Generates a random Secret during deployment |
| `prod` | 3 gateway, 2 per backend | immutable version or SHA tag | Requires a pre-created Secret, larger resources, HPA 3–10 |

Production secrets are intentionally **not** committed. Create them before a
`prod` deploy (or use External Secrets / Sealed Secrets / SOPS):

```bash
kubectl create namespace api-gateway
kubectl -n api-gateway create secret generic gateway-secrets \
  --from-literal=jwt-secret="$(openssl rand -hex 32)" \
  --from-literal=api-keys="$(openssl rand -hex 16)" \
  --from-literal=redis-password="$(openssl rand -hex 24)"
```

The gateway needs no cloud credentials. The manifests include a non-root,
read-only-root security context, Restricted Pod Security labels, NetworkPolicies,
HPA, PDB, rolling updates, startup/readiness/liveness probes, and a Redis
StatefulSet with a PVC. See [`docs/DEVOPS.md`](docs/DEVOPS.md) for the full
runbook.

Use [`docs/MANUAL_TEST_CHECKLIST.md`](docs/MANUAL_TEST_CHECKLIST.md) to
manually verify backend behavior, Compose, kind, monitoring, and optional
Terraform, Ansible, and GitHub Actions paths.

### CI/CD workflows

| Workflow | Trigger | What it does |
|----------|---------|--------------|
| `ci.yml` | push, PR | deployment config validation, gofmt, vet, golangci-lint, race tests + coverage, build, Docker build |
| `security.yml` | push, PR, weekly | CodeQL, govulncheck, Trivy SARIF, gitleaks |
| `e2e-kind.yml` | push, PR | kind cluster → deploy → smoke, Prometheus scrape, Grafana health |
| `devcontainer.yml` | relevant paths, PR | build Dev Container, run Go tests and DevOps validation in Linux |
| `docker-publish.yml` | main, `v*` tags | multi-arch GHCR images, SBOM/provenance, Trivy image scan |
| `deploy.yml` | successful image publish, manual | automatically deploys `main` to staging and version tags to production; supports manual deployment, applies secrets/manifests, waits for rollout, and smoke tests |
| `terraform.yml` | `terraform/**` | `fmt -check`, `init`, `validate`, tflint |
| `terraform-kind.yml` | Terraform/deploy paths, PR | apply to disposable kind, API/ingress smoke, targeted destroy |
| `ansible.yml` | `ansible/**` | yamllint, syntax check, ansible-lint |
| `ansible-idempotence.yml` | Ansible/deploy paths, PR | provision disposable Linux runner twice using its Docker daemon; assert second run has zero changes |

Image tags: `main` produces `latest`, `dev` and `sha-<sha>`; a `vX.Y.Z` tag also
produces `stable` and semver tags. Deploy by immutable `sha-<commit>` or
semantic-version tag rather than mutable `dev`, `latest`, or `stable` tags.
After a successful image publish, `main` deploys its immutable `sha-<commit>`
image to `staging`, while a `vX.Y.Z` tag deploys the same immutable SHA image
to `production`. Configure each GitHub `staging` and `production` environment
with `KUBE_CONFIG`; `production` also needs `GATEWAY_JWT_SECRET`,
`GATEWAY_API_KEYS`, and `GATEWAY_REDIS_PASSWORD`. The workflow selects
`staging` for the dev overlay and `production` for the prod overlay. Protect the
`production` environment with required reviewers and deployment branch rules.
GHCR images must be public or the target cluster must already be able to pull
them. Manual deployment remains available from the Actions tab.

### Terraform

`terraform/` creates a kind cluster, ingress-nginx, metrics-server, optionally
kube-prometheus-stack, then builds/loads the app images and applies the chosen
overlay:

```bash
cd terraform
cp terraform.tfvars.example terraform.tfvars
terraform init
terraform apply

terraform destroy
```

### Ansible

`ansible/` provisions Docker, kind, kubectl, Helm, then copies the project,
builds images, deploys into kind, installs monitoring, and runs the smoke test:

```bash
ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml
ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml --tags deploy
```

Set `gateway_install_monitoring`, `gateway_overlay`, and `gateway_image_tag` in
`ansible/group_vars/all.yml`. The default inventory deploys to localhost;
uncomment the remote host example for a Linux VM.

## Troubleshooting

- **Redis down** — the gateway logs a warning at startup and keeps serving;
  rate limiting and circuit breaking fail open. If you prefer to reject traffic
  during a Redis outage, change the policy in `rateLimitMiddleware` (the
  `redis_error` metric makes the current behavior observable).
- **429 responses** — the identity hit its bucket. Buckets are keyed by hashed
  API key / JWT subject / client IP plus route; check `rate_limit_rps` and
  `rate_limit_burst`.
- **503 responses** — the circuit is open, or every backend for the route is
  unhealthy. Check `gateway_circuit_breaker_state` and the backend `/health`
  endpoints.
- **502 responses** — the selected backend could not be reached; see the proxy
  error in the logs.
- **JWT rejected** — only HS256 is accepted, `exp` must be valid, and the secret
  must match `jwt_secret`.
- **Config change ignored** — reload runs every 2s and only on modification-time
  changes; check the log for a validation error. The old config stays active.
- **Port in use** — `lsof -i :8080`.

## Limitations

- Active-connection counts are per gateway instance; least-connections is a local
  decision even though rate limiting and circuit breaking are shared in Redis.
- Circuit-breaker state is tracked per route, not per individual backend.
- JWTs are symmetric HS256 only; there is no JWKS/RS256 support.
- The gateway does not terminate TLS; terminate it at the ingress, load balancer,
  or service mesh.
- `/metrics` and pprof have no authentication. In Kubernetes, pprof is not
  exposed by the Service, and the Ingress routes only `/users`, so `/metrics` is
  reachable only from inside the cluster (ServiceMonitor or port-forward). Add a
  path to the Ingress when you add routes, and do not add `/metrics`. In Docker
  Compose pprof binds to `127.0.0.1`. Restrict or disable pprof
  (`PPROF_ADDR=off`) in production.
- The shipped Redis is a single-replica StatefulSet for local/kind use. Use a
  managed or HA Redis for production; rate limiting and circuit breaking fail
  open during a Redis outage.
- The dev overlay generates random credentials on first deployment and keeps
  them in the cluster Secret. The prod overlay requires a pre-created Secret
  or an external secret manager.

## Contributing

1. Fork the repository.
2. Create a feature branch.
3. Make your changes.
4. Run `make fmt-check vet lint test`, and validate manifests with
   `kubectl kustomize deploy/k8s/overlays/dev`.
5. Open a pull request.

## Author

Built by [Mohd Kamaal](https://github.com/Mohdcode).
