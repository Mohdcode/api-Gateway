# Dev Container

The Dev Container gives the project a consistent Linux-based development
toolchain without automatically changing cluster or application state.

## Included tools

- Go 1.27.3 and Go development tools, including `golangci-lint`
- Docker CLI, Buildx, and Compose v2 connected to the host Docker engine
- kind 0.33.0
- kubectl v1.37.0 and Helm
- Terraform 1.13.5 and TFLint
- Python 3.12, Ansible Core, ansible-lint, and yamllint
- shellcheck

Ansible packages are installed with pipx into an isolated environment. Helm
repositories used by the Terraform configuration are registered for the
container user. On first creation, the container also runs
`make validate-devops`.

## Open the container

1. Install Docker Desktop and the VS Code Dev Containers extension.
2. Open the repository in VS Code.
3. Run **Dev Containers: Reopen in Container** from the Command Palette.
4. Wait for the image build and post-create checks to finish.

The post-create step installs local CLI tooling and validates configuration. It
does **not** create a kind cluster, start Compose, apply Kubernetes resources,
or run Terraform/Ansible provisioning.

## Run project workflows

From the VS Code terminal inside the container:

```bash
make test
make validate-devops
make compose-up
```

For the Kubernetes demo, kind creates Docker containers using the mounted host
Docker socket:

```bash
DOCKER_BUILD_FLAGS=--network=host make kind-e2e
```

Use the repository's explicit cleanup commands when finished:

```bash
CLUSTER_NAME=api-gateway make kind-down
make compose-down
```

`make compose-down` removes Compose volumes, including persisted local data.
`make kind-down` deletes the named kind cluster. Confirm the cluster name and
current kubectl context before cleanup.

Terraform and Ansible remain explicit workflows. Inspect Terraform plans before
applying, and check the Ansible inventory before running a playbook; the default
Ansible inventory targets localhost.

## Docker socket access

The Docker-outside-of-Docker feature mounts the host Docker socket into the
The container connects directly to the mounted host socket rather than using
the feature's non-root socket proxy. This avoids kind bootstrap failures seen
through the proxy. The container is added to the socket's root group because
Docker Desktop exposes this socket as `root:root`; Docker access effectively
grants control over the host's containers and can provide host-level access.
Only reopen this Dev Container for a trusted checkout.

The workspace is mounted at its original host path so Docker Desktop can
resolve project-relative bind mounts used by Compose. The container also uses
host networking so package and tool downloads work in environments where
Docker's default bridge network cannot reach external registries. This lets
the container use host-network services. The container does not mount kubeconfig
or cloud credentials automatically; provide cluster access deliberately when
you need it.
