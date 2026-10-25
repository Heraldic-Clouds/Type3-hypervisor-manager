resource "aws_iam_role" "lambda" {
  for_each           = toset(["api", "control", "migrate", "worker"])
  name               = "${local.prefix}-${each.key}"
  assume_role_policy = local.lambda_trust
}
resource "aws_iam_role_policy" "lambda_logs" {
  for_each = aws_iam_role.lambda
  role     = each.value.name
  policy   = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = ["${aws_cloudwatch_log_group.main[each.key].arn}:*"] }] })
}
resource "aws_iam_role_policy_attachment" "lambda_vpc" {
  for_each   = toset(["api", "control", "migrate"])
  role       = aws_iam_role.lambda[each.key].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
}
resource "aws_iam_role_policy" "lambda_secret" {
  for_each = toset(["api", "control", "migrate"])
  role     = aws_iam_role.lambda[each.key].name
  policy   = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [aws_secretsmanager_secret.database[each.key == "migrate" ? "owner" : "app"].arn] }] })
}
resource "aws_iam_role" "desktop" {
  name               = "${local.prefix}-desktop"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_instance_profile" "desktop" {
  name = "${local.prefix}-desktop"
  role = aws_iam_role.desktop.name
}
resource "aws_iam_role_policy_attachment" "desktop_ssm" {
  role       = aws_iam_role.desktop.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
resource "aws_iam_role_policy" "desktop_secret" {
  role   = aws_iam_role.desktop.name
  policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = "arn:aws:secretsmanager:${var.region}:${local.account}:secret:${local.prefix}/desktop/*", Condition = { StringEquals = { "secretsmanager:ResourceTag/InstanceArn" = "$${ec2:SourceInstanceARN}" } } }] })
}
resource "aws_iam_role_policy" "worker" {
  role = aws_iam_role.lambda["worker"].name
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Sid = "DatabaseControl", Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = [aws_lambda_function.database["control"].arn] },
    { Sid = "Workflow", Effect = "Allow", Action = ["states:StartExecution"], Resource = ["arn:aws:states:${var.region}:${local.account}:stateMachine:${local.prefix}"] },
    { Sid = "WorkflowStatus", Effect = "Allow", Action = ["states:DescribeExecution"], Resource = ["arn:aws:states:${var.region}:${local.account}:execution:${local.prefix}:*"] },
    { Sid = "Inventory", Effect = "Allow", Action = ["ec2:DescribeInstances"], Resource = "*" },
    { Sid = "CheckBootstrap", Effect = "Allow", Action = ["ssm:SendCommand"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*"], Condition = { StringEquals = { "ssm:resourceTag/MEAEnvironment" = local.prefix } } },
    { Sid = "ReadinessDocument", Effect = "Allow", Action = ["ssm:SendCommand"], Resource = ["arn:aws:ssm:${var.region}::document/AWS-RunShellScript"] },
    { Sid = "ReadinessResult", Effect = "Allow", Action = ["ssm:GetCommandInvocation"], Resource = "*" },
    { Sid = "LaunchTypeRestriction", Effect = "Allow", Action = ["ec2:RunInstances"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*"], Condition = { StringEquals = { "ec2:InstanceType" = var.desktop_type, "aws:RequestTag/MEAEnvironment" = local.prefix } } },
    { Sid = "LaunchResources", Effect = "Allow", Action = ["ec2:RunInstances"], Resource = ["arn:aws:ec2:${var.region}::image/${var.desktop_ami}", "arn:aws:ec2:${var.region}:${local.account}:subnet/${aws_subnet.public[0].id}", "arn:aws:ec2:${var.region}:${local.account}:security-group/${aws_security_group.desktop.id}", "arn:aws:ec2:${var.region}:${local.account}:network-interface/*", "arn:aws:ec2:${var.region}:${local.account}:volume/*"] },
    { Sid = "TagAtLaunch", Effect = "Allow", Action = ["ec2:CreateTags"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*", "arn:aws:ec2:${var.region}:${local.account}:volume/*"], Condition = { StringEquals = { "ec2:CreateAction" = "RunInstances" } } },
    { Sid = "TerminateOwned", Effect = "Allow", Action = ["ec2:TerminateInstances"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*"], Condition = { StringEquals = { "ec2:ResourceTag/MEAEnvironment" = local.prefix } } },
    { Sid = "DesktopRole", Effect = "Allow", Action = ["iam:PassRole"], Resource = [aws_iam_role.desktop.arn], Condition = { StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" } } },
    { Sid = "DesktopCredentials", Effect = "Allow", Action = ["secretsmanager:CreateSecret", "secretsmanager:GetSecretValue", "secretsmanager:DeleteSecret", "secretsmanager:TagResource"], Resource = ["arn:aws:secretsmanager:${var.region}:${local.account}:secret:${local.prefix}/desktop/*"] },
    { Sid = "TransactionalMail", Effect = "Allow", Action = ["ses:SendEmail"], Resource = [aws_sesv2_email_identity.domain.arn], Condition = { StringEquals = { "ses:FromAddress" = "hello@${var.domain}" } } }
  ] })
}
resource "aws_iam_role_policy" "smtp_secret" {
  count  = var.smtp_secret_arn == "" ? 0 : 1
  role   = aws_iam_role.lambda["worker"].name
  policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [var.smtp_secret_arn] }] })
}
resource "aws_lambda_function" "database" {
  for_each                       = toset(["api", "control", "migrate"])
  function_name                  = "${local.prefix}-${each.key}"
  role                           = aws_iam_role.lambda[each.key].arn
  filename                       = "${path.module}/../build/api.zip"
  source_code_hash               = filebase64sha256("${path.module}/../build/api.zip")
  runtime                        = "provided.al2023"
  handler                        = "bootstrap"
  architectures                  = ["x86_64"]
  memory_size                    = 512
  timeout                        = 30
  reserved_concurrent_executions = each.key == "api" ? 5 : 2
  vpc_config {
    subnet_ids         = [aws_subnet.private[0].id]
    security_group_ids = [aws_security_group.lambda.id]
  }
  environment {
    variables = {
      MODE       = each.key, DB_SECRET_ARN = aws_secretsmanager_secret.database[each.key == "migrate" ? "owner" : "app"].arn,
      APP_ORIGIN = local.origin, DESKTOP_CAP = tostring(var.desktop_cap), IDLE_SECONDS = tostring(var.idle_seconds), WARNING_SECONDS = tostring(var.warning_seconds), LIFETIME_SECONDS = tostring(var.lifetime_seconds), GRACE_SECONDS = tostring(var.grace_seconds), SECURE_COOKIES = "true", REQUIRE_VERIFICATION = tostring(var.require_verification), GUAC_SCHEMA = "/var/task/schema/guacamole.sql"
    }
  }
  logging_config {
    log_format = "Text"
    log_group  = aws_cloudwatch_log_group.main[each.key].name
  }
  depends_on = [aws_iam_role_policy_attachment.lambda_vpc, aws_iam_role_policy.lambda_secret, aws_secretsmanager_secret_version.owner, aws_secretsmanager_secret_version.app]
}
resource "aws_lambda_function" "worker" {
  function_name                  = "${local.prefix}-worker"
  role                           = aws_iam_role.lambda["worker"].arn
  filename                       = "${path.module}/../build/worker.zip"
  source_code_hash               = filebase64sha256("${path.module}/../build/worker.zip")
  runtime                        = "provided.al2023"
  handler                        = "bootstrap"
  architectures                  = ["x86_64"]
  memory_size                    = 256
  timeout                        = 120
  reserved_concurrent_executions = 2
  environment {
    variables = merge({
      CONTROL_FUNCTION = aws_lambda_function.database["control"].function_name, RESOURCE_PREFIX = local.prefix, ACCOUNT_ID = local.account,
      DESKTOP_AMI      = var.desktop_ami, DESKTOP_SUBNET = aws_subnet.public[0].id, DESKTOP_SG = aws_security_group.desktop.id,
      DESKTOP_PROFILE  = aws_iam_instance_profile.desktop.name, DESKTOP_INSTANCE_TYPE = var.desktop_type,
      IDLE_SECONDS     = tostring(var.idle_seconds), LIFETIME_SECONDS = tostring(var.lifetime_seconds), STATE_MACHINE_NAME = local.prefix, STATE_MACHINE_ARN = "arn:aws:states:${var.region}:${local.account}:stateMachine:${local.prefix}", APP_ORIGIN = local.origin, EMAIL_FROM = "hello@${var.domain}"
    }, var.smtp_secret_arn == "" ? {} : { SMTP_SECRET_ARN = var.smtp_secret_arn })
  }
  logging_config {
    log_format = "Text"
    log_group  = aws_cloudwatch_log_group.main["worker"].name
  }
  depends_on = [aws_iam_role_policy.worker, aws_iam_role_policy.smtp_secret]
}
resource "aws_apigatewayv2_api" "http" {
  name          = local.prefix
  protocol_type = "HTTP"
}
resource "aws_apigatewayv2_integration" "http" {
  api_id                 = aws_apigatewayv2_api.http.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.database["api"].invoke_arn
  payload_format_version = "2.0"
  timeout_milliseconds   = 29000
}
resource "aws_apigatewayv2_route" "http" {
  api_id    = aws_apigatewayv2_api.http.id
  route_key = "ANY /api/{proxy+}"
  target    = "integrations/${aws_apigatewayv2_integration.http.id}"
}
resource "aws_apigatewayv2_stage" "http" {
  api_id      = aws_apigatewayv2_api.http.id
  name        = "$default"
  auto_deploy = true
  default_route_settings {
    throttling_burst_limit = 20
    throttling_rate_limit  = 10
  }
  access_log_settings {
    destination_arn = aws_cloudwatch_log_group.main["http"].arn
    format          = jsonencode({ requestId = "$context.requestId", status = "$context.status", route = "$context.routeKey", latency = "$context.responseLatency" })
  }
}
resource "aws_lambda_permission" "http" {
  statement_id  = "APIGateway"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.database["api"].function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.http.execution_arn}/*/*"
}
resource "aws_iam_role" "workflow" {
  name               = "${local.prefix}-workflow"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "states.amazonaws.com" }, Action = "sts:AssumeRole", Condition = { StringEquals = { "aws:SourceAccount" = local.account }, ArnEquals = { "aws:SourceArn" = "arn:aws:states:${var.region}:${local.account}:stateMachine:${local.prefix}" } } }] })
}
resource "aws_iam_role_policy" "workflow" {
  role = aws_iam_role.workflow.name
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = [aws_lambda_function.worker.arn] },
  { Effect = "Allow", Action = ["logs:CreateLogDelivery", "logs:GetLogDelivery", "logs:UpdateLogDelivery", "logs:DeleteLogDelivery", "logs:ListLogDeliveries", "logs:PutResourcePolicy", "logs:DescribeResourcePolicies", "logs:DescribeLogGroups"], Resource = "*" }] })
}
resource "aws_sfn_state_machine" "desktop" {
  name     = local.prefix
  role_arn = aws_iam_role.workflow.arn
  logging_configuration {
    level                  = "ERROR"
    include_execution_data = false
    log_destination        = "${aws_cloudwatch_log_group.main["workflow"].arn}:*"
  }
  definition = jsonencode({
    StartAt = "Launch", TimeoutSeconds = 840,
    States = {
      Launch  = { Type = "Task", Resource = aws_lambda_function.worker.arn, Parameters = { action = "launch", "id.$" = "$.id" }, ResultPath = "$.launch", Next = "Wait", Retry = [{ ErrorEquals = ["States.TaskFailed"], IntervalSeconds = 5, MaxAttempts = 2, BackoffRate = 2 }], Catch = [{ ErrorEquals = ["States.ALL"], ResultPath = "$.error", Next = "Cleanup" }] },
      Wait    = { Type = "Wait", Seconds = 10, Next = "Probe" },
      Probe   = { Type = "Task", Resource = aws_lambda_function.worker.arn, Parameters = { action = "probe", "id.$" = "$.id" }, ResultPath = "$.probe", Next = "IsReady", Retry = [{ ErrorEquals = ["States.TaskFailed"], IntervalSeconds = 5, MaxAttempts = 2, BackoffRate = 2 }], Catch = [{ ErrorEquals = ["States.ALL"], ResultPath = "$.error", Next = "Cleanup" }] },
      IsReady = { Type = "Choice", Choices = [{ Variable = "$.probe.ready", BooleanEquals = true, Next = "Ready" }], Default = "Wait" },
      Ready   = { Type = "Succeed" },
      Cleanup = { Type = "Task", Resource = aws_lambda_function.worker.arn, Parameters = { action = "failure", "id.$" = "$.id" }, ResultPath = null, Next = "Failed", Retry = [{ ErrorEquals = ["States.ALL"], IntervalSeconds = 5, MaxAttempts = 3 }] },
      Failed  = { Type = "Fail", Error = "DesktopProvisioningFailed" }
    }
  })
  depends_on = [aws_iam_role_policy.workflow]
}
resource "aws_iam_role" "scheduler" {
  name               = "${local.prefix}-scheduler"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "scheduler.amazonaws.com" }, Action = "sts:AssumeRole", Condition = { StringEquals = { "aws:SourceAccount" = local.account }, ArnLike = { "aws:SourceArn" = "arn:aws:scheduler:${var.region}:${local.account}:schedule-group/default" } } }] })
}
resource "aws_iam_role_policy" "scheduler" {
  role   = aws_iam_role.scheduler.name
  policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = [aws_lambda_function.worker.arn] }] })
}
resource "aws_scheduler_schedule" "watchdog" {
  name                = local.prefix
  schedule_expression = "rate(1 minute)"
  flexible_time_window { mode = "OFF" }
  target {
    arn      = aws_lambda_function.worker.arn
    role_arn = aws_iam_role.scheduler.arn
    input    = jsonencode({ action = "watchdog" })
    retry_policy {
      maximum_event_age_in_seconds = 60
      maximum_retry_attempts       = 1
    }
  }
}
