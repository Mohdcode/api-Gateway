locals {
  kubeconfig_path = pathexpand(var.kubeconfig_path)
  deploy_root     = "${path.module}/.."
}

provider "kind" {}

resource "kind_cluster" "this" {
  name            = var.cluster_name
  node_image      = var.node_image != "" ? var.node_image : null
  wait_for_ready  = true
  kubeconfig_path = local.kubeconfig_path

  kind_config {
    kind        = "Cluster"
    api_version = "kind.x-k8s.io/v1alpha4"

    node {
      role = "control-plane"
      kubeadm_config_patches = [
        "kind: InitConfiguration\nnodeRegistration:\n  kubeletExtraArgs:\n    node-labels: \"ingress-ready=true\"\n"
      ]
      extra_port_mappings {
        container_port = 80
        host_port      = var.ingress_http_host_port
      }
      extra_port_mappings {
        container_port = 443
        host_port      = var.ingress_https_host_port
      }
    }

    node {
      role = "worker"
    }

    node {
      role = "worker"
    }

    networking {
      api_server_address = "127.0.0.1"
    }
  }
}

provider "helm" {
  kubernetes = {
    config_path    = local.kubeconfig_path
    config_context = kind_cluster.this.context_name
  }
}

resource "helm_release" "ingress_nginx" {
  name             = "ingress-nginx"
  repository       = "https://kubernetes.github.io/ingress-nginx"
  chart            = "ingress-nginx"
  version          = var.ingress_nginx_chart_version
  namespace        = "ingress-nginx"
  create_namespace = true
  wait             = true
  timeout          = 600

  values = [
    yamlencode({
      controller = {
        hostPort = {
          enabled = true
        }
        tolerations = [
          {
            key      = "node-role.kubernetes.io/control-plane"
            operator = "Exists"
            effect   = "NoSchedule"
          }
        ]
        service = {
          type = "NodePort"
        }
        nodeSelector = {
          "ingress-ready" = "true"
        }
        admissionWebhooks = {
          patch = {
            nodeSelector = {
              "ingress-ready" = "true"
            }
            tolerations = [
              {
                key      = "node-role.kubernetes.io/control-plane"
                operator = "Exists"
                effect   = "NoSchedule"
              }
            ]
          }
        }
      }
    })
  ]
}

resource "helm_release" "metrics_server" {
  name             = "metrics-server"
  repository       = "https://kubernetes-sigs.github.io/metrics-server"
  chart            = "metrics-server"
  version          = var.metrics_server_chart_version
  namespace        = "kube-system"
  create_namespace = false
  wait             = true
  timeout          = 600

  values = [
    yamlencode({
      args = ["--kubelet-insecure-tls"]
    })
  ]
}

resource "helm_release" "kube_prometheus_stack" {
  count = var.install_monitoring ? 1 : 0

  name             = "kube-prometheus-stack"
  repository       = "https://prometheus-community.github.io/helm-charts"
  chart            = "kube-prometheus-stack"
  version          = var.kube_prometheus_stack_chart_version
  namespace        = var.monitoring_namespace
  create_namespace = true
  wait             = true
  timeout          = 1200
  atomic           = true

  values = [
    yamlencode({
      grafana = {
        sidecar = {
          dashboards = {
            enabled         = true
            searchNamespace = "ALL"
          }
        }
      }
      prometheus = {
        prometheusSpec = {
          serviceMonitorSelectorNilUsesHelmValues = false
          ruleSelectorNilUsesHelmValues           = false
        }
      }
    })
  ]
}

resource "null_resource" "app" {
  depends_on = [
    kind_cluster.this,
    helm_release.ingress_nginx,
    helm_release.metrics_server,
  ]

  triggers = {
    cluster    = kind_cluster.this.id
    overlay    = var.overlay
    tag        = var.image_tag
    registry   = var.registry
    skip_build = tostring(var.skip_build)
    manifest_sha = sha1(join("", [
      for f in sort(fileset("${local.deploy_root}/deploy/k8s", "**/*.yaml")) :
      filesha1("${local.deploy_root}/deploy/k8s/${f}")
    ]))
  }

  provisioner "local-exec" {
    command = "${local.deploy_root}/scripts/deploy-kind.sh"
    environment = {
      CLUSTER_NAME       = var.cluster_name
      KUBECONFIG         = local.kubeconfig_path
      OVERLAY            = var.overlay
      TAG                = var.image_tag
      REGISTRY           = var.registry
      INSTALL_MONITORING = "false"
      SKIP_BUILD         = tostring(var.skip_build)
    }
  }
}

resource "null_resource" "monitoring_manifests" {
  count = var.install_monitoring ? 1 : 0

  depends_on = [
    null_resource.app,
    helm_release.kube_prometheus_stack,
  ]

  triggers = {
    cluster = kind_cluster.this.id
    manifest_sha = sha1(join("", [
      for f in sort(fileset("${local.deploy_root}/deploy/k8s/monitoring", "**/*.yaml")) :
      filesha1("${local.deploy_root}/deploy/k8s/monitoring/${f}")
    ]))
  }

  provisioner "local-exec" {
    command = "kubectl apply -k ${local.deploy_root}/deploy/k8s/monitoring"
    environment = {
      KUBECONFIG = local.kubeconfig_path
    }
  }
}
