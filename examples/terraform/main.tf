terraform {
  required_version = ">= 1.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

# VPC and networking
resource "aws_vpc" "nexus" {
  count                = var.create_vpc ? 1 : 0
  cidr_block           = var.vpc_cidr
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = merge(var.tags, {
    Name = "${var.name}-vpc"
  })
}

resource "aws_subnet" "nexus" {
  count             = var.create_vpc ? length(var.availability_zones) : 0
  vpc_id            = aws_vpc.nexus[0].id
  cidr_block        = cidrsubnet(var.vpc_cidr, 4, count.index)
  availability_zone = var.availability_zones[count.index]

  tags = merge(var.tags, {
    Name = "${var.name}-subnet-${count.index}"
  })
}

# Security group
resource "aws_security_group" "nexus" {
  name_prefix = "${var.name}-"
  description = "Security group for Nexus Object Storage"
  vpc_id      = var.create_vpc ? aws_vpc.nexus[0].id : var.vpc_id

  ingress {
    from_port   = 9000
    to_port     = 9000
    protocol    = "tcp"
    cidr_blocks = [var.allowed_cidr]
    description = "S3 API"
  }

  ingress {
    from_port   = 9001
    to_port     = 9001
    protocol    = "tcp"
    cidr_blocks = [var.allowed_cidr]
    description = "Admin API"
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.name}-sg"
  })
}

# ECS task definition
resource "aws_ecs_task_definition" "nexus" {
  family                   = var.name
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = var.task_cpu
  memory                   = var.task_memory

  container_definitions = jsonencode([
    {
      name      = "nexus"
      image     = var.container_image
      essential = true
      portMappings = [
        { containerPort = 9000, protocol = "tcp" },
        { containerPort = 9001, protocol = "tcp" }
      ]
      environment = [
        { name = "NEXUS_GATEWAY_ADDRESS", value = ":9000" },
        { name = "NEXUS_GATEWAY_ADMIN_ADDRESS", value = ":9001" },
        { name = "NEXUS_STORAGE_BACKEND", value = var.storage_backend },
        { name = "NEXUS_OBSERVABILITY_LOGGING_LEVEL", value = var.log_level }
      ]
      secrets = [
        { name = "NEXUS_GATEWAY_ACCESS_KEY", valueFrom = aws_secretsmanager_secret.access_key.arn },
        { name = "NEXUS_GATEWAY_SECRET_KEY", valueFrom = aws_secretsmanager_secret.secret_key.arn }
      ]
      healthCheck = {
        command     = ["CMD-SHELL", "curl -f http://localhost:9000/health || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 10
      }
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.nexus.name
          awslogs-region        = var.region
          awslogs-stream-prefix = "nexus"
        }
      }
    }
  ])

  tags = var.tags
}

# ECS service
resource "aws_ecs_service" "nexus" {
  name            = var.name
  cluster         = aws_ecs_cluster.nexus.id
  task_definition = aws_ecs_task_definition.nexus.arn
  desired_count   = var.service_count
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = var.create_vpc ? aws_subnet.nexus[*].id : var.subnet_ids
    security_groups = [aws_security_group.nexus.id]
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.nexus.arn
    container_name   = "nexus"
    container_port   = 9000
  }

  depends_on = [aws_lb_listener.nexus]
}

# ECS cluster
resource "aws_ecs_cluster" "nexus" {
  name = "${var.name}-cluster"
  tags = var.tags
}

# Load balancer
resource "aws_lb" "nexus" {
  name               = "${var.name}-alb"
  internal           = var.internal
  load_balancer_type = "application"
  subnets            = var.create_vpc ? aws_subnet.nexus[*].id : var.subnet_ids
  security_groups    = [aws_security_group.nexus.id]

  tags = merge(var.tags, {
    Name = "${var.name}-alb"
  })
}

resource "aws_lb_target_group" "nexus" {
  name        = "${var.name}-tg"
  port        = 9000
  protocol    = "HTTP"
  vpc_id      = var.create_vpc ? aws_vpc.nexus[0].id : var.vpc_id
  target_type = "ip"

  health_check {
    path = "/health"
  }

  tags = var.tags
}

resource "aws_lb_listener" "nexus" {
  load_balancer_arn = aws_lb.nexus.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.nexus.arn
  }
}

# Secrets
resource "aws_secretsmanager_secret" "access_key" {
  name                    = "${var.name}-access-key"
  recovery_window_in_days = 7
  tags                    = var.tags
}

resource "aws_secretsmanager_secret_version" "access_key" {
  secret_id     = aws_secretsmanager_secret.access_key.id
  secret_string = var.access_key
}

resource "aws_secretsmanager_secret" "secret_key" {
  name                    = "${var.name}-secret-key"
  recovery_window_in_days = 7
  tags                    = var.tags
}

resource "aws_secretsmanager_secret_version" "secret_key" {
  secret_id     = aws_secretsmanager_secret.secret_key.id
  secret_string = var.secret_key
}

# CloudWatch logs
resource "aws_cloudwatch_log_group" "nexus" {
  name              = "/nexus/${var.name}"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

# Auto scaling
resource "aws_appautoscaling_target" "nexus" {
  max_capacity       = var.max_capacity
  min_capacity       = var.min_capacity
  resource_id        = "service/${aws_ecs_cluster.nexus.name}/${aws_ecs_service.nexus.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

resource "aws_appautoscaling_policy" "nexus_cpu" {
  name               = "${var.name}-cpu-autoscaling"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.nexus.resource_id
  scalable_dimension = aws_appautoscaling_target.nexus.scalable_dimension
  service_namespace  = aws_appautoscaling_target.nexus.service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value = 70.0
  }
}
