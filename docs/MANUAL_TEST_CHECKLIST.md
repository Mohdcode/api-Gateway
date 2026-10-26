# Manual Test Checklist

Use this checklist to verify the API Gateway from the application layer outward
through containers, Kubernetes, monitoring, and automation. Record the date,
commit, environment, and result for each run; a local pass does not prove a
production deployment.

## Before testing

- [ ] Confirm the working tree and branch:

  ```bash
  git status --short
  git rev-parse --short HEAD
  ```

- [ ] Confirm required tools for the path being tested:

  ```bash
  go version
  docker compose version
  kubectl version --client
  kind version
  ```

- [ ] Check cluster contexts before running Kubernetes commands:

  ```bash
  kind get clusters
  kubectl config get-contexts
  kubectl config current-context
  ```

- [ ] Do not run Terraform, Ansible, and `make kind-e2e` against the same
  cluster at the same time.
- [ ] Do not use production credentials or a production cluster for the
  development checks below.

## 1. Backend and Go checks

- [ ] Run formatting, static analysis, and the race-enabled test suite:

  ```bash
  make fmt-check
  make vet
  make test
  ```

  **Expected:** all commands exit successfully; tests pass for the gateway and
  all backend packages.

- [ ] Build the gateway:

  ```bash
  make build
  ```

  **Expected:** `bin/gateway` is created.

- [ ] Run unit tests with coverage if you want a recorded coverage result:

  ```bash
  make cover
  ```

  **Expected:** tests pass and the last line reports total statement coverage.

- [ ] Check behavior represented by the test suite: auth, rate limiting,
  circuit breaker transitions, load balancing, health checks, config reload,
  operational endpoints, proxy errors, and graceful shutdown.

## 2. Docker Compose checks

- [ ] Validate Compose configuration and start the complete local stack:

  ```bash
  docker compose config --quiet
  make compose-up
  docker compose ps
  ```

  **Expected:** gateway is healthy; Redis is healthy; all three backends,
  Prometheus, and Grafana are running.

- [ ] Check unauthenticated access is rejected:

  ```bash
  curl -i http://127.0.0.1:8080/users
  ```

  **Expected:** HTTP `401`.

- [ ] Check authenticated proxying:

  ```bash
  curl -i -H 'X-API-KEY: abc-123' http://127.0.0.1:8080/users
  ```

  **Expected:** HTTP `200` and a response from one of the demo backends.

- [ ] Check operations endpoints:

  ```bash
  curl -i http://127.0.0.1:8080/healthz
  curl -i http://127.0.0.1:8080/readyz
  curl -i http://127.0.0.1:8080/version
  ```

  **Expected:** health and readiness return HTTP `200`; version returns build
  metadata.

- [ ] Check Prometheus metrics and service scrape:

  ```bash
  curl -fsS http://127.0.0.1:8080/metrics | grep http_request_counter
  curl -fsS http://127.0.0.1:9090/api/v1/targets
  ```

  **Expected:** gateway request metrics are present and the gateway target is
  `"health":"up"`.

- [ ] Open Grafana at `http://127.0.0.1:3000` and confirm the API Gateway
  dashboard is available. The local Compose defaults are `admin` / `admin`
  unless overridden.

- [ ] Check that web-facing Compose ports are bound to loopback:

  ```bash
  docker compose ps
  ```

  **Expected:** gateway, Prometheus, Grafana, Redis, and pprof host bindings
  show `127.0.0.1`.

- [ ] Stop Compose only when finished. This removes its volumes and persisted
  local Redis/Prometheus/Grafana data:

  ```bash
  make compose-down
  ```

## 3. Kubernetes and kind checks

- [ ] Confirm no conflicting project cluster exists, then run the full
  development end-to-end path:

  ```bash
  kind get clusters
  DOCKER_BUILD_FLAGS=--network=host make kind-e2e
  ```

  **Expected:** a three-node `api-gateway-e2e` cluster is ready, ingress-nginx
  and metrics-server install, images build/load, deployments roll out, and
  both port-forward and ingress smoke checks pass. The ingress uses host ports
  `18080` and `18443` by default. The host-network build flag may be omitted if
  Docker can download Go modules without it.

- [ ] Verify context, nodes, and workload readiness:

  ```bash
  kubectl config current-context
  kubectl get nodes
  kubectl -n api-gateway get deployments,statefulsets,pods,services
  kubectl -n api-gateway get hpa,pdb,networkpolicies,ingress
  ```

  **Expected:** current context is `kind-api-gateway-e2e`; all three nodes are
  `Ready`; gateway, three backend deployments, and Redis are ready; HPA,
  PDB, policies, and ingress are present.

- [ ] Check rollouts:

  ```bash
  kubectl -n api-gateway rollout status deployment/gateway
  kubectl -n api-gateway rollout status statefulset/redis
  for n in 1 2 3; do
    kubectl -n api-gateway rollout status "deployment/backend-${n}"
  done
  ```

  **Expected:** every rollout completes successfully.

- [ ] Check ingress from the host:

  ```bash
  curl -i -H 'Host: gateway.local' \
    -H 'X-API-KEY: abc-123' \
    http://127.0.0.1:18080/users
  ```

  **Expected:** HTTP `200` and a demo backend response.

- [ ] Remove only the dedicated test cluster when finished:

  ```bash
  make kind-e2e-down
  ```

  **Expected:** `api-gateway-e2e` is deleted, the `kind-api-gateway` context is
  restored when present in the active kubeconfig, and other kind clusters are
  unchanged.

- [ ] Check autoscaler metrics:

  ```bash
  kubectl -n api-gateway get hpa gateway
  kubectl top nodes
  kubectl -n api-gateway top pods
  ```

  **Expected:** resource metrics are reported. Newly installed metrics-server
  may need a short time before metrics are available.

- [ ] Optional: install the Kubernetes monitoring stack only when you want to
  test it. This installs Helm charts and creates resources in the project
  cluster:

  ```bash
  DOCKER_BUILD_FLAGS=--network=host make monitoring
  kubectl -n monitoring get pods
  kubectl -n monitoring get servicemonitor,prometheusrule
  ```

  **Expected:** kube-prometheus-stack becomes ready and the gateway
  ServiceMonitor and alert rules exist. Check Prometheus targets and the Grafana
  dashboard after port-forwarding, as documented in `docs/DEVOPS.md`.

- [ ] Remove only the project kind cluster when finished:

  ```bash
  CLUSTER_NAME=api-gateway make kind-down
  ```

  **Warning:** this deletes the cluster and its Kubernetes resources/PVC data.
  Confirm the cluster name before running it. It does not stop Docker Compose.

## 4. Deployment configuration checks

- [ ] Run static DevOps configuration validation:

  ```bash
  make validate-devops
  ```

  **Expected:** YAML lint, shell syntax, Compose rendering, and Kustomize
  rendering for dev, prod, and monitoring all pass. This does not deploy
  resources to a cluster.

- [ ] Render the dev and prod manifests for inspection:

  ```bash
  kubectl kustomize deploy/k8s/overlays/dev
  kubectl kustomize deploy/k8s/overlays/prod
  ```

  **Expected:** both render without errors. The dev overlay contains
  development-only credentials; do not use those credentials in production.

- [ ] Review the security and availability settings in the rendered manifests:
  non-root/read-only containers, resource requests and limits, probes,
  NetworkPolicies, HPA, PDB, and Redis PVC.

## 5. Terraform checks (optional; Terraform required)

- [ ] Run the non-destructive validation steps:

  ```bash
  make tf-fmt
  make tf-init
  make tf-validate
  ```

  **Expected:** formatting, provider initialization, and configuration
  validation pass.

- [ ] Inspect a plan before applying:

  ```bash
  terraform -chdir=terraform plan
  ```

  **Expected:** plan shows only the intended local kind cluster, add-ons, and
  app resources.

- [ ] Only apply when you intend to create a separate Terraform-managed cluster:

  ```bash
  terraform -chdir=terraform apply
  ```

  **Warning:** Terraform can create and later destroy infrastructure.
  Review the plan and cluster name first; do not apply against a shared or
  production environment.

- [ ] Destroy only resources created by that Terraform state after verifying
  the plan:

  ```bash
  terraform -chdir=terraform destroy
  ```

## 6. Ansible checks (optional; Ansible required)

- [ ] Run syntax and lint checks before provisioning:

  ```bash
  ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml --syntax-check
  ansible-lint ansible
  ```

  **Expected:** playbook parses and lint passes.

- [ ] Review `ansible/inventory/hosts.ini` before running the playbook. The
  default target is localhost and the playbook can install packages, create a
  kind cluster, and deploy the stack.

- [ ] For a disposable local environment, preview changes first:

  ```bash
  ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml --check --diff
  ```

  **Expected:** Ansible reports proposed changes without applying them. Some
  command tasks cannot fully simulate changes in check mode.

- [ ] Apply only after confirming the target inventory and intended effects:

  ```bash
  ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml
  ```

## 7. GitHub Actions checks

- [ ] Push a branch or open a pull request and inspect the Actions tab.
- [ ] Confirm `ci.yml`, `e2e-kind.yml`, and `security.yml` complete successfully.
- [ ] For Terraform or Ansible changes, confirm their path-filtered workflow runs.
- [ ] For image publishing, use a branch or version-tag event intended to publish
  images; verify GHCR tags, scan result, SBOM, and provenance.
- [ ] Do not run the manual deployment workflow until GitHub staging/production
  environments and their secrets are configured and the selected image tag
  exists.
- [ ] For production deployment, require environment approval and verify the
  resulting rollout and smoke test.

## 8. Results log

| Date / commit | Layer | Command or check | Result / notes |
|---------------|-------|-----------------|----------------|
|               |       |                 |                |
|               |       |                 |                |
|               |       |                 |                |
