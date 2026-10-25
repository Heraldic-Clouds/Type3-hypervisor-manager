resource "aws_budgets_budget" "monthly" {
  name         = "${local.prefix}-monthly"
  budget_type  = "COST"
  limit_amount = "150"
  limit_unit   = "USD"
  time_unit    = "MONTHLY"
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 50
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.budget_email]
  }
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.budget_email]
  }
}
resource "aws_cloudwatch_log_metric_filter" "capacity" {
  name           = "${local.prefix}-capacity"
  log_group_name = aws_cloudwatch_log_group.main["worker"].name
  pattern        = "{ $.activeDesktops = * }"
  metric_transformation {
    name          = "ActiveDesktops"
    namespace     = "MEA"
    value         = "$.activeDesktops"
    default_value = 0
  }
}
resource "aws_cloudwatch_metric_alarm" "capacity" {
  alarm_name          = "${local.prefix}-capacity"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "ActiveDesktops"
  namespace           = "MEA"
  period              = 60
  statistic           = "Maximum"
  threshold           = var.desktop_cap
  alarm_description   = "Desktop quota exceeded: investigate and terminate orphans."
  treat_missing_data  = "notBreaching"
}
resource "aws_cloudwatch_metric_alarm" "worker_errors" {
  alarm_name          = "${local.prefix}-watchdog-errors"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  metric_name         = "Errors"
  namespace           = "AWS/Lambda"
  period              = 60
  statistic           = "Sum"
  threshold           = 0
  dimensions          = { FunctionName = aws_lambda_function.worker.function_name }
  treat_missing_data  = "breaching"
}
resource "aws_iam_role" "builder" {
  name               = "${local.prefix}-ami-builder"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_instance_profile" "builder" {
  name = "${local.prefix}-ami-builder"
  role = aws_iam_role.builder.name
}
resource "aws_iam_role_policy_attachment" "builder" {
  role       = aws_iam_role.builder.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
resource "aws_security_group" "builder" {
  name   = "${local.prefix}-ami-builder"
  vpc_id = aws_vpc.main.id
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
output "website" { value = local.origin }
output "cloudfront_domain" { value = aws_cloudfront_distribution.frontend.domain_name }
output "cloudfront_id" { value = aws_cloudfront_distribution.frontend.id }
output "frontend_bucket" { value = aws_s3_bucket.frontend.id }
output "ecr_repository" { value = aws_ecr_repository.guac.repository_url }
output "builder_subnet" { value = aws_subnet.public[0].id }
output "builder_security_group" { value = aws_security_group.builder.id }
output "builder_profile" { value = aws_iam_instance_profile.builder.name }
output "api_endpoint" { value = aws_apigatewayv2_api.http.api_endpoint }
output "database_endpoint" { value = aws_db_instance.database.endpoint }
