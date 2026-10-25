resource "aws_ecr_repository" "guac" {
  name         = "${local.prefix}-guacamole"
  force_delete = true
  image_scanning_configuration { scan_on_push = true }
}
resource "aws_ecr_lifecycle_policy" "guac" {
  repository = aws_ecr_repository.guac.name
  policy     = jsonencode({ rules = [{ rulePriority = 1, description = "Keep five releases", selection = { tagStatus = "any", countType = "imageCountMoreThan", countNumber = 5 }, action = { type = "expire" } }] })
}
resource "aws_ecs_cluster" "guac" { name = local.prefix }
resource "aws_iam_role" "ecs_execution" {
  name               = "${local.prefix}-ecs-execution"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ecs-tasks.amazonaws.com" }, Action = "sts:AssumeRole", Condition = { StringEquals = { "aws:SourceAccount" = local.account }, ArnLike = { "aws:SourceArn" = "arn:aws:ecs:${var.region}:${local.account}:*" } } }] })
}
resource "aws_iam_role_policy_attachment" "ecs_execution" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}
resource "aws_iam_role_policy" "ecs_secrets" {
  role   = aws_iam_role.ecs_execution.name
  policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [aws_secretsmanager_secret.database["guac"].arn] }] })
}
resource "aws_ecs_task_definition" "guac" {
  family                   = local.prefix
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = "512"
  memory                   = "1024"
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  container_definitions = jsonencode([
    { name = "guacd", image = "guacamole/guacd:1.6.0@sha256:8974eaa9ba32f713daf311e7cc8cd7e4cdfba1edea39eed75524e78ef4b08f4f", essential = true, memoryReservation = 192,
    logConfiguration = { logDriver = "awslogs", options = { "awslogs-group" = aws_cloudwatch_log_group.main["guacd"].name, "awslogs-region" = var.region, "awslogs-stream-prefix" = "guacd" } } },
    { name         = "guacamole", image = "${aws_ecr_repository.guac.repository_url}:${var.guac_image_tag}", essential = true, memoryReservation = 640,
      portMappings = [{ containerPort = 8080, protocol = "tcp" }],
      environment = [for k, v in {
        GUACD_HOSTNAME                  = "127.0.0.1", GUACD_PORT = "4822", POSTGRESQL_ENABLED = "true", POSTGRESQL_HOSTNAME = aws_db_instance.database.address, POSTGRESQL_PORT = "5432", POSTGRESQL_DATABASE = "mea", POSTGRESQL_USERNAME = "mea_guac",
        POSTGRESQL_SSL_MODE             = "verify-full", POSTGRESQL_SSL_ROOT_CERT_FILE = "/opt/guacamole/rds-ca.pem",
        JSON_ENABLED                    = "true", EXTENSION_PRIORITY = "json,postgresql", REMOTE_IP_VALVE_ENABLED = "true",
        POSTGRESQL_AUTO_CREATE_ACCOUNTS = "false", API_SESSION_TIMEOUT = "5", MEA_JDBC_URL = "jdbc:postgresql://${aws_db_instance.database.endpoint}/mea?sslmode=verify-full&sslrootcert=/opt/guacamole/rds-ca.pem", MEA_DB_USER = "mea_guac",
        MEA_ORIGIN                      = local.origin
      } : { name = k, value = v }],
      secrets     = [{ name = "POSTGRESQL_PASSWORD", valueFrom = "${aws_secretsmanager_secret.database["guac"].arn}:password::" }, { name = "MEA_DB_PASSWORD", valueFrom = "${aws_secretsmanager_secret.database["guac"].arn}:password::" }, { name = "JSON_SECRET_KEY", valueFrom = "${aws_secretsmanager_secret.database["guac"].arn}:jsonKey::" }],
      healthCheck = { command = ["CMD-SHELL", "curl -fsS http://localhost:8080/guacamole/ >/dev/null || exit 1"], interval = 30, timeout = 5, retries = 3, startPeriod = 90 },
    logConfiguration = { logDriver = "awslogs", options = { "awslogs-group" = aws_cloudwatch_log_group.main["guacamole"].name, "awslogs-region" = var.region, "awslogs-stream-prefix" = "guacamole" } } }
  ])
}
resource "aws_lb" "guac" {
  name               = local.prefix
  load_balancer_type = "application"
  internal           = false
  subnets            = aws_subnet.public[*].id
  security_groups    = [aws_security_group.alb.id]
  idle_timeout       = 3600
}
resource "aws_lb_target_group" "guac" {
  name                 = local.prefix
  port                 = 8080
  protocol             = "HTTP"
  vpc_id               = aws_vpc.main.id
  target_type          = "ip"
  deregistration_delay = 300
  health_check {
    path                = "/guacamole/"
    matcher             = "200"
    interval            = 30
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }
  stickiness {
    type            = "lb_cookie"
    enabled         = true
    cookie_duration = 14400
  }
}
resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.guac.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = aws_acm_certificate_validation.desktop.certificate_arn
  default_action {
    type = "fixed-response"
    fixed_response {
      content_type = "text/plain"
      status_code  = "404"
      message_body = "Mise En Abyme"
    }
  }
}
resource "aws_lb_listener_rule" "tunnel" {
  listener_arn = aws_lb_listener.https.arn
  priority     = 10
  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.guac.arn
  }
  condition {
    path_pattern { values = ["/guacamole/api/tokens*", "/guacamole/websocket-tunnel", "/guacamole/tunnel", "/guacamole/guacamole-common-js/*"] }
  }
}
resource "aws_route53_record" "desktop" {
  provider = aws.dns
  zone_id  = var.zone_id
  name     = "desktop.${var.domain}"
  type     = "A"
  alias {
    name                   = aws_lb.guac.dns_name
    zone_id                = aws_lb.guac.zone_id
    evaluate_target_health = false
  }
}
resource "aws_ecs_service" "guac" {
  name                               = local.prefix
  cluster                            = aws_ecs_cluster.guac.id
  task_definition                    = aws_ecs_task_definition.guac.arn
  desired_count                      = var.guac_desired_count
  launch_type                        = "FARGATE"
  platform_version                   = "1.4.0"
  enable_ecs_managed_tags            = true
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200
  health_check_grace_period_seconds  = 120
  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
  network_configuration {
    subnets          = [aws_subnet.public[0].id]
    security_groups  = [aws_security_group.guac.id]
    assign_public_ip = true
  }
  load_balancer {
    target_group_arn = aws_lb_target_group.guac.arn
    container_name   = "guacamole"
    container_port   = 8080
  }
  depends_on = [aws_lb_listener.https]
}
resource "aws_appautoscaling_target" "guac" {
  max_capacity       = 1
  min_capacity       = var.guac_desired_count
  resource_id        = "service/${aws_ecs_cluster.guac.name}/${aws_ecs_service.guac.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

