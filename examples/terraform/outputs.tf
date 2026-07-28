output "cipherlake_endpoint" {
  description = "CipherLake S3 API endpoint"
  value       = "http://${aws_lb.cipherlake.dns_name}:9000"
}

output "cipherlake_admin_endpoint" {
  description = "CipherLake Admin API endpoint"
  value       = "http://${aws_lb.cipherlake.dns_name}:9001"
}

output "ecs_cluster_name" {
  description = "ECS cluster name"
  value       = aws_ecs_cluster.cipherlake.name
}

output "load_balancer_arn" {
  description = "Load balancer ARN"
  value       = aws_lb.cipherlake.arn
}

output "security_group_id" {
  description = "Security group ID"
  value       = aws_security_group.cipherlake.id
}

output "vpc_id" {
  description = "VPC ID"
  value       = var.create_vpc ? aws_vpc.cipherlake[0].id : var.vpc_id
}
