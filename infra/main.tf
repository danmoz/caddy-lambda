terraform {
  required_version = "= 1.12.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}

provider "aws" {
  # AWS_REGION from infra/mise.toml selects the deployment region.
}

variable "lambda_name" {
  description = "Name of the deployed Lambda function"
  type        = string
  default     = "caddy-lambda-fixture"
}

variable "stack_name" {
  description = "Prefix used for AWS resource names"
  type        = string
  default     = "caddy-lambda-test"
}

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_vpc" "testing" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "caddy-lambda-testing-vpc"
  }
}

resource "aws_internet_gateway" "testing" {
  vpc_id = aws_vpc.testing.id
}

resource "aws_subnet" "public" {
  count                   = 2
  vpc_id                  = aws_vpc.testing.id
  cidr_block              = cidrsubnet(aws_vpc.testing.cidr_block, 4, count.index)
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true

  tags = {
    Name = "caddy-lambda-testing-public-${count.index + 1}"
  }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.testing.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.testing.id
  }
}

resource "aws_route_table_association" "public" {
  count          = 2
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

data "aws_iam_policy_document" "lambda_assume_role" {
  statement {
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }

    actions = ["sts:AssumeRole"]
  }
}

resource "aws_iam_role" "lambda" {
  name               = "${var.lambda_name}-execution"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume_role.json
}

resource "aws_iam_role_policy_attachment" "lambda_logs" {
  role       = aws_iam_role.lambda.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

resource "aws_lambda_function" "fixture" {
  function_name    = var.lambda_name
  filename         = "${path.root}/.build/lambda.zip"
  source_code_hash = filebase64sha256("${path.root}/.build/lambda.zip")
  role             = aws_iam_role.lambda.arn
  runtime          = "python3.12"
  handler          = "app.handler"
  timeout          = 10
  memory_size      = 256

  depends_on = [aws_iam_role_policy_attachment.lambda_logs]
}

resource "aws_ecr_repository" "caddy" {
  name                 = var.stack_name
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }
}

data "aws_iam_policy_document" "ecs_assume_role" {
  statement {
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    actions = ["sts:AssumeRole"]
  }
}

resource "aws_iam_role" "ecs_execution" {
  name               = "${var.stack_name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume_role.json
}

resource "aws_iam_role_policy_attachment" "ecs_execution" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role" "ecs_task" {
  name               = "${var.stack_name}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume_role.json
}

data "aws_iam_policy_document" "ecs_task_lambda" {
  statement {
    effect    = "Allow"
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.fixture.arn]
  }
}

resource "aws_iam_role_policy" "ecs_task_lambda" {
  name   = "invoke-fixture-lambda"
  role   = aws_iam_role.ecs_task.id
  policy = data.aws_iam_policy_document.ecs_task_lambda.json
}

resource "aws_cloudwatch_log_group" "caddy" {
  name              = "/ecs/${var.stack_name}"
  retention_in_days = 1
}

resource "aws_ecs_cluster" "test" {
  name = var.stack_name
}

resource "aws_ecs_task_definition" "caddy" {
  family                   = var.stack_name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "X86_64"
  }

  container_definitions = jsonencode([
    {
      name      = "caddy"
      image     = "${aws_ecr_repository.caddy.repository_url}:latest"
      essential = true

      portMappings = [
        {
          containerPort = 80
          hostPort      = 80
          protocol      = "tcp"
        }
      ]

      environment = [
        {
          name  = "LAMBDA_FUNCTION"
          value = aws_lambda_function.fixture.function_name
        },
        {
          name  = "AWS_REGION"
          value = data.aws_region.current.name
        }
      ]

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.caddy.name
          awslogs-region        = data.aws_region.current.region
          awslogs-stream-prefix = "caddy"
        }
      }
    }
  ])

  depends_on = [aws_iam_role_policy_attachment.ecs_execution]
}

resource "aws_security_group" "caddy" {
  name        = "${var.stack_name}-caddy"
  description = "Public access to the Caddy test task"
  vpc_id      = aws_vpc.testing.id

  ingress {
    protocol    = "tcp"
    from_port   = 80
    to_port     = 80
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    protocol    = "-1"
    from_port   = 0
    to_port     = 0
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_ecs_service" "caddy" {
  name            = var.stack_name
  cluster         = aws_ecs_cluster.test.id
  task_definition = aws_ecs_task_definition.caddy.arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = aws_subnet.public[*].id
    security_groups  = [aws_security_group.caddy.id]
    assign_public_ip = true
  }

}

output "ecs_cluster" {
  value = aws_ecs_cluster.test.name
}

output "ecs_service" {
  value = aws_ecs_service.caddy.name
}

output "ecr_repository_url" {
  value = aws_ecr_repository.caddy.repository_url
}

output "lambda_name" {
  value = aws_lambda_function.fixture.function_name
}
