variable "name" {
  description = "Name prefix for all resources"
  type        = string
  default     = "nexus"
}

variable "region" {
  description = "AWS region"
  type        = string
  default     = "us-east-1"
}

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}

# VPC
variable "create_vpc" {
  description = "Create a new VPC for Nexus"
  type        = bool
  default     = true
}

variable "vpc_id" {
  description = "Existing VPC ID (when create_vpc is false)"
  type        = string
  default     = ""
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

variable "subnet_ids" {
  description = "Existing subnet IDs (when create_vpc is false)"
  type        = list(string)
  default     = []
}

variable "availability_zones" {
  description = "Availability zones for subnets"
  type        = list(string)
  default     = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

# Network
variable "allowed_cidr" {
  description = "CIDR block allowed to access Nexus"
  type        = string
  default     = "10.0.0.0/8"
}

variable "internal" {
  description = "Whether the load balancer is internal"
  type        = bool
  default     = true
}

# ECS
variable "container_image" {
  description = "Docker image for Nexus"
  type        = string
  default     = "ghcr.io/nexus/nexus:1.0.0"
}

variable "task_cpu" {
  description = "CPU units for the ECS task"
  type        = string
  default     = "1024"
}

variable "task_memory" {
  description = "Memory for the ECS task"
  type        = string
  default     = "2048"
}

variable "service_count" {
  description = "Desired count of Nexus instances"
  type        = number
  default     = 2
}

# Storage
variable "storage_backend" {
  description = "Storage backend type"
  type        = string
  default     = "local"
}

# Credentials
variable "access_key" {
  description = "Nexus access key"
  type        = string
  sensitive   = true
}

variable "secret_key" {
  description = "Nexus secret key"
  type        = string
  sensitive   = true
}

# Observability
variable "log_level" {
  description = "Log level"
  type        = string
  default     = "info"
}

variable "log_retention_days" {
  description = "CloudWatch log retention in days"
  type        = number
  default     = 30
}

# Auto scaling
variable "min_capacity" {
  description = "Minimum number of Nexus instances"
  type        = number
  default     = 2
}

variable "max_capacity" {
  description = "Maximum number of Nexus instances"
  type        = number
  default     = 10
}
