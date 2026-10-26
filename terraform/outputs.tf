output "cluster_name" {
  description = "kind cluster name."
  value       = kind_cluster.this.name
}

output "context_name" {
  description = "kubectl context for the cluster."
  value       = kind_cluster.this.context_name
}

output "endpoint" {
  description = "Kubernetes API endpoint."
  value       = kind_cluster.this.endpoint
  sensitive   = true
}

output "kubeconfig_path" {
  description = "kubeconfig written by kind."
  value       = local.kubeconfig_path
}

output "gateway_url" {
  description = "Ingress URL after deploy/hosts entry."
  value       = var.ingress_http_host_port == 80 ? "http://gateway.local" : "http://gateway.local:${var.ingress_http_host_port}"
}

output "next_steps" {
  description = "Useful commands after apply."
  value = {
    smoke      = "KUBECONFIG=${local.kubeconfig_path} ${local.deploy_root}/scripts/smoke-test.sh"
    ingress    = "curl -H 'Host: gateway.local' -H 'X-API-KEY: abc-123' http://127.0.0.1:${var.ingress_http_host_port}/users"
    prometheus = "kubectl -n ${var.monitoring_namespace} port-forward svc/kube-prometheus-stack-prometheus 9090:9090"
    grafana    = "kubectl -n ${var.monitoring_namespace} port-forward svc/kube-prometheus-stack-grafana 3000:80"
    destroy    = "terraform -chdir=terraform destroy"
  }
}
