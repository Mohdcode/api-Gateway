variable "cluster_name" {
  description = "kind cluster name."
  type        = string
  default     = "api-gateway"
}

variable "kubeconfig_path" {
  description = "kubeconfig file kind writes to and the Helm provider reads."
  type        = string
  default     = "~/.kube/config"
}

variable "node_image" {
  description = "Optional kindest/node image. Empty uses the kind default."
  type        = string
  default     = ""
}

variable "ingress_http_host_port" {
  description = "Host port mapped to ingress HTTP on the kind control-plane node."
  type        = number
  default     = 18080

  validation {
    condition     = var.ingress_http_host_port >= 1 && var.ingress_http_host_port <= 65535
    error_message = "ingress_http_host_port must be between 1 and 65535."
  }
}

variable "ingress_https_host_port" {
  description = "Host port mapped to ingress HTTPS on the kind control-plane node."
  type        = number
  default     = 18443

  validation {
    condition     = var.ingress_https_host_port >= 1 && var.ingress_https_host_port <= 65535
    error_message = "ingress_https_host_port must be between 1 and 65535."
  }
}

variable "overlay" {
  description = "Kustomize overlay to deploy: dev or prod."
  type        = string
  default     = "dev"

  validation {
    condition     = contains(["dev", "prod"], var.overlay)
    error_message = "overlay must be dev or prod."
  }
}

variable "image_tag" {
  description = "Image tag built and loaded into the kind cluster."
  type        = string
  default     = "dev"
}

variable "registry" {
  description = "Image registry and namespace used for image names."
  type        = string
  default     = "ghcr.io/mohdcode"
}

variable "skip_build" {
  description = "Skip docker build/load and only apply manifests (images must already exist in the cluster)."
  type        = bool
  default     = false
}

variable "install_monitoring" {
  description = "Install kube-prometheus-stack, ServiceMonitor, PrometheusRule and the Grafana dashboard."
  type        = bool
  default     = true
}

variable "monitoring_namespace" {
  description = "Namespace for kube-prometheus-stack."
  type        = string
  default     = "monitoring"
}

variable "ingress_nginx_chart_version" {
  description = "ingress-nginx Helm chart version."
  type        = string
  default     = "4.15.1"
}

variable "metrics_server_chart_version" {
  description = "metrics-server Helm chart version."
  type        = string
  default     = "3.14.0"
}

variable "kube_prometheus_stack_chart_version" {
  description = "kube-prometheus-stack Helm chart version."
  type        = string
  default     = "92.13.1"
}
