#!/usr/bin/env bash
set -euo pipefail

export PIPX_HOME="$HOME/.local/pipx"
export PIPX_BIN_DIR="$HOME/.local/bin"
export PATH="$PIPX_BIN_DIR:$PATH"

go install sigs.k8s.io/kind@v0.33.0

pipx install --python "$(command -v python3)" ansible-core==2.21.5
pipx inject --include-apps ansible-core ansible-lint==26.10.1 yamllint==1.38.0

helm repo add --force-update ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo add --force-update metrics-server https://kubernetes-sigs.github.io/metrics-server
helm repo add --force-update prometheus-community https://prometheus-community.github.io/helm-charts

make validate-devops
