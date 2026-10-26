SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

CLUSTER_NAME ?= api-gateway
E2E_CLUSTER_NAME ?= api-gateway-e2e
INGRESS_HTTP_HOST_PORT ?= 18080
INGRESS_HTTPS_HOST_PORT ?= 18443
OVERLAY ?= dev
TAG ?= dev
REGISTRY ?= ghcr.io/mohdcode
MONITORING_NAMESPACE ?= monitoring
DOCKER_BUILD_FLAGS ?=

GATEWAY_IMAGE := $(REGISTRY)/api-gateway:$(TAG)
BACKEND_IMAGES := $(REGISTRY)/api-gateway-backend-1:$(TAG) $(REGISTRY)/api-gateway-backend-2:$(TAG) $(REGISTRY)/api-gateway-backend-3:$(TAG)

.PHONY: help fmt fmt-check validate-devops vet lint test cover build clean \
	docker-build docker-push docker-backends compose-up compose-down compose-logs \
	kind-up kind-down deploy monitoring smoke kind-e2e kind-e2e-down \
	tf-fmt tf-init tf-validate tf-apply tf-destroy \
	ansible-deploy ansible-lint

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

fmt: ## Format Go code
	gofmt -w .

fmt-check: ## Fail if Go code is not formatted
	@test -z "$$(gofmt -l .)" || { echo "unformatted files:"; gofmt -l .; exit 1; }

validate-devops: ## Lint YAML, check shell, and render Compose and Kubernetes manifests
	@command -v yamllint >/dev/null || { echo "error: yamllint is required" >&2; exit 1; }
	@command -v kubectl >/dev/null || { echo "error: kubectl is required" >&2; exit 1; }
	@command -v docker >/dev/null || { echo "error: docker is required" >&2; exit 1; }
	yamllint -c .yamllint .github ansible deploy compose.yaml config.yaml prometheus.yml
	@for script in scripts/*.sh; do bash -n "$$script"; done
	@JWT_SECRET="$$(openssl rand -hex 32)" \
		API_KEYS="$$(openssl rand -hex 32)" \
		REDIS_PASSWORD="$$(openssl rand -hex 32)" \
		GF_SECURITY_ADMIN_PASSWORD="$$(openssl rand -hex 32)" \
		docker compose config --quiet
	@for overlay in dev prod; do \
		kubectl kustomize "deploy/k8s/overlays/$$overlay" >/dev/null || exit; \
	done
	kubectl kustomize deploy/k8s/monitoring >/dev/null

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint (install: https://golangci-lint.run)
	golangci-lint run ./...

test: ## Run the race-enabled test suite
	go test -race -count=1 ./...

cover: ## Run tests with a coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

build: ## Build the gateway binary into bin/
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/gateway .

clean: ## Remove local build and coverage artifacts
	rm -rf bin coverage.out

docker-build: ## Build the gateway and backend images
	docker build $(DOCKER_BUILD_FLAGS) -t $(GATEWAY_IMAGE) .
	for n in 1 2 3; do \
		docker build $(DOCKER_BUILD_FLAGS) -f Dockerfile.backend --build-arg BACKEND=backend$$n -t $(REGISTRY)/api-gateway-backend-$$n:$(TAG) .; \
	done

docker-push: ## Push images to $(REGISTRY)
	docker push $(GATEWAY_IMAGE)
	for image in $(BACKEND_IMAGES); do docker push $$image; done

compose-up: ## Start the full local stack (gateway, backends, Redis, Prometheus, Grafana)
	docker compose up -d --build

compose-down: ## Stop the local stack and remove volumes
	docker compose down -v

compose-logs: ## Tail local stack logs
	docker compose logs -f --tail=100

kind-up: ## Create the kind cluster with ingress-nginx and metrics-server
	CLUSTER_NAME=$(CLUSTER_NAME) INGRESS_HTTP_HOST_PORT=$(INGRESS_HTTP_HOST_PORT) INGRESS_HTTPS_HOST_PORT=$(INGRESS_HTTPS_HOST_PORT) ./scripts/kind-up.sh

kind-down: ## Delete the kind cluster
	CLUSTER_NAME=$(CLUSTER_NAME) ./scripts/kind-down.sh

deploy: ## Build, load and deploy the app into kind (OVERLAY=dev|prod TAG=dev)
	CLUSTER_NAME=$(CLUSTER_NAME) INGRESS_HTTP_HOST_PORT=$(INGRESS_HTTP_HOST_PORT) OVERLAY=$(OVERLAY) TAG=$(TAG) REGISTRY=$(REGISTRY) DOCKER_BUILD_FLAGS="$(DOCKER_BUILD_FLAGS)" ./scripts/deploy-kind.sh

monitoring: ## Deploy the app plus kube-prometheus-stack and dashboards
	CLUSTER_NAME=$(CLUSTER_NAME) OVERLAY=$(OVERLAY) TAG=$(TAG) REGISTRY=$(REGISTRY) INSTALL_MONITORING=true ./scripts/deploy-kind.sh

smoke: ## Smoke test the deployed gateway
	NAMESPACE=api-gateway ./scripts/smoke-test.sh

kind-e2e: ## Full local end-to-end on an isolated cluster (E2E_CLUSTER_NAME=api-gateway-e2e)
	$(MAKE) CLUSTER_NAME=$(E2E_CLUSTER_NAME) INGRESS_HTTP_HOST_PORT=$(INGRESS_HTTP_HOST_PORT) INGRESS_HTTPS_HOST_PORT=$(INGRESS_HTTPS_HOST_PORT) kind-up
	$(MAKE) CLUSTER_NAME=$(E2E_CLUSTER_NAME) OVERLAY=$(OVERLAY) TAG=$(TAG) REGISTRY=$(REGISTRY) DOCKER_BUILD_FLAGS="$(DOCKER_BUILD_FLAGS)" deploy
	$(MAKE) CLUSTER_NAME=$(E2E_CLUSTER_NAME) smoke
	curl --fail --silent --show-error --retry 30 --retry-delay 1 --retry-connrefused -H 'Host: gateway.local' -H 'X-API-KEY: abc-123' http://127.0.0.1:$(INGRESS_HTTP_HOST_PORT)/users
	@echo

kind-e2e-down: ## Delete only the isolated end-to-end test cluster
	$(MAKE) CLUSTER_NAME=$(E2E_CLUSTER_NAME) kind-down
	@if [[ "$(CLUSTER_NAME)" != "$(E2E_CLUSTER_NAME)" ]] && kubectl config get-contexts -o name | grep -qx "kind-$(CLUSTER_NAME)"; then \
		kubectl config use-context "kind-$(CLUSTER_NAME)" >/dev/null; \
		echo "restored kubectl context kind-$(CLUSTER_NAME)"; \
	fi

tf-fmt: ## Format Terraform files
	terraform -chdir=terraform fmt -recursive

tf-init: ## Initialise Terraform providers
	terraform -chdir=terraform init

tf-validate: ## Validate Terraform configuration
	terraform -chdir=terraform validate

tf-apply: ## Create the kind cluster and addons with Terraform
	terraform -chdir=terraform apply

tf-destroy: ## Destroy Terraform-managed resources
	terraform -chdir=terraform destroy

ansible-deploy: ## Provision and deploy to hosts in ansible/inventory/hosts.ini
	ansible-playbook -i ansible/inventory/hosts.ini ansible/site.yml

ansible-lint: ## Lint Ansible content
	ansible-lint ansible
