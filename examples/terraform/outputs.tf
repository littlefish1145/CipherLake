output "nexus_endpoint" {
  description = "Nexus S3 API endpoint"
  value       = "http://${aws_lb.nexus.dns_name}:9000"
}

output "nexus_admin_endpoint" {
  description = "Nexus Admin API endpoint"
  value       = "http://${aws_lb.nexus.dns_name}:9001"
}

output "ecs_cluster_name" {
  description = "ECS cluster name"
  value       = aws_ecs_cluster.nexus.name
}

output "load_balancer_arn" {
  description = "Load balancer ARN"
  value       = aws_lb.nexus.arn
}

output "security_group_id" {
  description = "Security group ID"
  value       = aws_security_group.nexus.id
}

output "vpc_id" {
  description = "VPC ID"
  value       = var.create_vpc ? aws_vpc.nexus[0].id : var.vpc_id
}
