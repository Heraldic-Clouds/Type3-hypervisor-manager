# Milestone 2: one persistent Ubuntu LXD GPU host for Power and Eleet, EFS for
# subscriber files, and the Stripe billing function. Paid resources are off
# until enable_gpu_host / enable_billing are set after cost approval.
variable "enable_gpu_host" {
  type        = bool
  default     = false
  description = "Creates EFS, the GPU launch template and permissions. The host itself starts only on demand."
}
variable "enable_billing" {
  type    = bool
  default = false
}
variable "gpu_ami" { default = "" }
variable "gpu_instance_type" {
  default = "g6.4xlarge"
  validation {
    condition     = var.gpu_instance_type == "g6.4xlarge"
    error_message = "Only g6.4xlarge is approved for the staging GPU host."
  }
}
variable "gpu_data_gib" { default = 250 }
variable "budget_limit" { default = "150" }
variable "stripe_price_power" { default = "" }
variable "stripe_price_eleet" { default = "" }
variable "gpu_host_idle_seconds" { default = 900 }
variable "eleet_admission_limit" {
  default     = 2
  description = "Concurrent running Eleet containers. Four per GPU is an unbenchmarked target; start conservative."
  validation {
    condition     = var.eleet_admission_limit >= 1 && var.eleet_admission_limit <= 4
    error_message = "Between one and four Eleet sessions per physical GPU."
  }
}
locals {
  gpu   = var.enable_gpu_host ? 1 : 0
  bill  = var.enable_billing ? 1 : 0
  gpu_tag = { MEAGpuHost = local.prefix }
}

resource "aws_security_group" "gpu_host" {
  count  = local.gpu
  name   = "${local.prefix}-gpu-host"
  vpc_id = aws_vpc.main.id
  ingress {
    description     = "Type3 API from the VPC control plane only"
    from_port       = 7443
    to_port         = 7443
    protocol        = "tcp"
    security_groups = [aws_security_group.lambda.id]
  }
  ingress {
    description     = "Per-container RDP proxies, Guacamole only"
    from_port       = 40000
    to_port         = 40999
    protocol        = "tcp"
    security_groups = [aws_security_group.guac.id]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
# The Lambda security group already allows egress anywhere in the VPC CIDR.
resource "aws_security_group" "efs" {
  count  = local.gpu
  name   = "${local.prefix}-efs"
  vpc_id = aws_vpc.main.id
  ingress {
    from_port       = 2049
    to_port         = 2049
    protocol        = "tcp"
    security_groups = [aws_security_group.gpu_host[0].id]
  }
}
resource "aws_efs_file_system" "users" {
  count            = local.gpu
  creation_token   = "${local.prefix}-users"
  encrypted        = true
  performance_mode = "generalPurpose"
  throughput_mode  = "elastic"
  lifecycle_policy { transition_to_ia = "AFTER_30_DAYS" }
  lifecycle_policy { transition_to_primary_storage_class = "AFTER_1_ACCESS" }
  tags = { Name = "${local.prefix}-users" }
}
resource "aws_efs_backup_policy" "users" {
  count          = local.gpu
  file_system_id = aws_efs_file_system.users[0].id
  backup_policy { status = "DISABLED" }
}
# One mount target, in the host's AZ: no cross-AZ traffic and no Multi-AZ spend.
resource "aws_efs_mount_target" "users" {
  count           = local.gpu
  file_system_id  = aws_efs_file_system.users[0].id
  subnet_id       = aws_subnet.public[0].id
  security_groups = [aws_security_group.efs[0].id]
}

resource "random_password" "type3" {
  length  = 48
  special = false
}
resource "aws_secretsmanager_secret" "type3" {
  count                   = local.gpu
  name                    = "${local.prefix}/type3"
  recovery_window_in_days = 0
}
resource "aws_secretsmanager_secret_version" "type3" {
  count         = local.gpu
  secret_id     = aws_secretsmanager_secret.type3[0].id
  secret_string = jsonencode({ apiKey = random_password.type3.result })
}

resource "aws_iam_role" "gpu_host" {
  count              = local.gpu
  name               = "${local.prefix}-gpu-host"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_instance_profile" "gpu_host" {
  count = local.gpu
  name  = "${local.prefix}-gpu-host"
  role  = aws_iam_role.gpu_host[0].name
}
resource "aws_iam_role_policy_attachment" "gpu_host" {
  for_each   = var.enable_gpu_host ? toset(["arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore", "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy"]) : toset([])
  role       = aws_iam_role.gpu_host[0].name
  policy_arn = each.value
}
resource "aws_iam_role_policy" "gpu_host" {
  count = local.gpu
  role  = aws_iam_role.gpu_host[0].name
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [aws_secretsmanager_secret.type3[0].arn] },
    { Effect = "Allow", Action = ["ssm:PutParameter"], Resource = ["arn:aws:ssm:${var.region}:${local.account}:parameter/${local.prefix}/type3/fingerprint"] }
  ] })
}

resource "aws_launch_template" "gpu_host" {
  count         = local.gpu
  name          = "${local.prefix}-gpu-host"
  image_id      = var.gpu_ami
  instance_type = var.gpu_instance_type
  # Termination protection: the host is only ever stopped and started.
  disable_api_termination              = true
  instance_initiated_shutdown_behavior = "stop"
  iam_instance_profile { name = aws_iam_instance_profile.gpu_host[0].name }
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }
  network_interfaces {
    device_index                = 0
    subnet_id                   = aws_subnet.public[0].id
    security_groups             = [aws_security_group.gpu_host[0].id]
    associate_public_ip_address = true
    delete_on_termination       = true
  }
  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      volume_size           = 60
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }
  # Persistent LXD pool: container root filesystems and installed software.
  block_device_mappings {
    device_name = "/dev/sdf"
    ebs {
      volume_size           = var.gpu_data_gib
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = false
    }
  }
  user_data = base64encode(jsonencode({
    prefix = local.prefix, efs = try(aws_efs_file_system.users[0].id, ""), type3Secret = try(aws_secretsmanager_secret.type3[0].arn, ""),
    controlCidr = "${cidrhost(aws_subnet.private[0].cidr_block, 0)}/24", guacCidr = "${cidrhost(aws_subnet.public[0].cidr_block, 0)}/24", dataGiB = var.gpu_data_gib
  }))
  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.gpu_tag, { Name = "MEA GPU host", Project = "MiseEnAbyme" })
  }
  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.gpu_tag, { Project = "MiseEnAbyme", MEAGpuData = local.prefix })
  }
  lifecycle {
    precondition {
      condition     = !var.enable_gpu_host || startswith(var.gpu_ami, "ami-")
      error_message = "Build the GPU host image with Packer and set gpu_ami first."
    }
  }
}

# Daily snapshots of the LXD data volume: recovery if the host is replaced.
resource "aws_iam_role" "dlm" {
  count              = local.gpu
  name               = "${local.prefix}-dlm"
  assume_role_policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "dlm.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
resource "aws_iam_role_policy_attachment" "dlm" {
  count      = local.gpu
  role       = aws_iam_role.dlm[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSDataLifecycleManagerServiceRole"
}
resource "aws_dlm_lifecycle_policy" "gpu_data" {
  count              = local.gpu
  description        = "MEA GPU host LXD pool daily snapshots"
  execution_role_arn = aws_iam_role.dlm[0].arn
  state              = "ENABLED"
  policy_details {
    resource_types = ["VOLUME"]
    target_tags    = { MEAGpuData = local.prefix }
    schedule {
      name      = "daily"
      copy_tags = true
      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = ["10:00"]
      }
      retain_rule { count = 7 }
    }
  }
}

# The worker can launch exactly this template's instance type, start and stop
# the tagged host, and nothing else. It has no permission to terminate it.
resource "aws_iam_role_policy" "worker_gpu" {
  count = local.gpu
  role  = aws_iam_role.lambda["worker"].name
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Sid = "LaunchGPUHost", Effect = "Allow", Action = ["ec2:RunInstances"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*"],
    Condition = { StringEquals = { "ec2:InstanceType" = var.gpu_instance_type, "aws:RequestTag/MEAGpuHost" = local.prefix }, ArnLike = { "ec2:LaunchTemplate" = aws_launch_template.gpu_host[0].arn } } },
    { Sid = "LaunchGPUResources", Effect = "Allow", Action = ["ec2:RunInstances"], Resource = ["arn:aws:ec2:${var.region}::image/${var.gpu_ami}", "arn:aws:ec2:${var.region}:${local.account}:launch-template/${aws_launch_template.gpu_host[0].id}", "arn:aws:ec2:${var.region}:${local.account}:subnet/${aws_subnet.public[0].id}", "arn:aws:ec2:${var.region}:${local.account}:security-group/${aws_security_group.gpu_host[0].id}", "arn:aws:ec2:${var.region}:${local.account}:network-interface/*", "arn:aws:ec2:${var.region}:${local.account}:volume/*"] },
    { Sid = "TagGPUAtLaunch", Effect = "Allow", Action = ["ec2:CreateTags"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*", "arn:aws:ec2:${var.region}:${local.account}:volume/*"], Condition = { StringEquals = { "ec2:CreateAction" = "RunInstances" } } },
    { Sid = "StartStopGPUHost", Effect = "Allow", Action = ["ec2:StartInstances", "ec2:StopInstances"], Resource = ["arn:aws:ec2:${var.region}:${local.account}:instance/*"], Condition = { StringEquals = { "ec2:ResourceTag/MEAGpuHost" = local.prefix } } },
    { Sid = "GPUHostRole", Effect = "Allow", Action = ["iam:PassRole"], Resource = [aws_iam_role.gpu_host[0].arn], Condition = { StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" } } },
    { Sid = "Type3Fingerprint", Effect = "Allow", Action = ["ssm:GetParameter"], Resource = ["arn:aws:ssm:${var.region}:${local.account}:parameter/${local.prefix}/type3/fingerprint"] }
  ] })
}
resource "aws_iam_role_policy" "type3_secret" {
  for_each = var.enable_gpu_host ? toset(["api", "control"]) : toset([])
  role     = aws_iam_role.lambda[each.key].name
  policy   = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [aws_secretsmanager_secret.type3[0].arn] }] })
}

resource "aws_cloudwatch_log_metric_filter" "gpu_hosts" {
  count          = local.gpu
  name           = "${local.prefix}-gpu-hosts"
  log_group_name = aws_cloudwatch_log_group.main["worker"].name
  pattern        = "{ $.gpuHosts = * }"
  metric_transformation {
    name          = "GPUHosts"
    namespace     = "MEA"
    value         = "$.gpuHosts"
    default_value = 0
  }
}
resource "aws_cloudwatch_metric_alarm" "gpu_hosts" {
  count               = local.gpu
  alarm_name          = "${local.prefix}-more-than-one-gpu-host"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 1
  metric_name         = "GPUHosts"
  namespace           = "MEA"
  period              = 60
  statistic           = "Maximum"
  threshold           = 1
  alarm_description   = "More than one MEA GPU host exists. Stop the extra instance immediately."
  treat_missing_data  = "notBreaching"
}
resource "aws_cloudwatch_log_metric_filter" "gpu_running" {
  count          = local.gpu
  name           = "${local.prefix}-gpu-running"
  log_group_name = aws_cloudwatch_log_group.main["worker"].name
  pattern        = "{ $.gpuRunning = * }"
  metric_transformation {
    name          = "GPUHostRunning"
    namespace     = "MEA"
    value         = "$.gpuRunning"
    default_value = 0
  }
}
resource "aws_cloudwatch_metric_alarm" "gpu_long_running" {
  count               = local.gpu
  alarm_name          = "${local.prefix}-gpu-host-running-12h"
  comparison_operator = "GreaterThanOrEqualToThreshold"
  evaluation_periods  = 720
  datapoints_to_alarm = 720
  metric_name         = "GPUHostRunning"
  namespace           = "MEA"
  period              = 60
  statistic           = "Maximum"
  threshold           = 1
  alarm_description   = "The GPU host has been running for 12 hours (~$16). Check sessions and keep-alive use."
  treat_missing_data  = "notBreaching"
}

# Stripe test-mode billing. The secret value is imported from the owner's
# local env file by scripts/configure-stripe.ps1, never written by Terraform.
resource "aws_secretsmanager_secret" "stripe" {
  count                   = local.bill
  name                    = "${local.prefix}/stripe"
  recovery_window_in_days = 0
}
resource "aws_iam_role" "billing" {
  count              = local.bill
  name               = "${local.prefix}-billing"
  assume_role_policy = local.lambda_trust
}
resource "aws_cloudwatch_log_group" "billing" {
  count             = local.bill
  name              = "/mea/${var.environment}/billing"
  retention_in_days = 7
}
resource "aws_iam_role_policy" "billing" {
  count = local.bill
  role  = aws_iam_role.billing[0].name
  policy = jsonencode({ Version = "2012-10-17", Statement = [
    { Effect = "Allow", Action = ["logs:CreateLogStream", "logs:PutLogEvents"], Resource = ["${aws_cloudwatch_log_group.billing[0].arn}:*"] },
    { Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [aws_secretsmanager_secret.stripe[0].arn] },
    { Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = [aws_lambda_function.database["control"].arn] }
  ] })
}
resource "aws_lambda_function" "billing" {
  count                          = local.bill
  function_name                  = "${local.prefix}-billing"
  role                           = aws_iam_role.billing[0].arn
  filename                       = "${path.module}/../build/billing.zip"
  source_code_hash               = filebase64sha256("${path.module}/../build/billing.zip")
  runtime                        = "provided.al2023"
  handler                        = "bootstrap"
  architectures                  = ["x86_64"]
  memory_size                    = 256
  timeout                        = 25
  reserved_concurrent_executions = 3
  environment {
    variables = {
      CONTROL_FUNCTION = aws_lambda_function.database["control"].function_name, APP_ORIGIN = local.origin,
      STRIPE_SECRET_ARN = aws_secretsmanager_secret.stripe[0].arn, STRIPE_PRICE_POWER = var.stripe_price_power, STRIPE_PRICE_ELEET = var.stripe_price_eleet
    }
  }
  logging_config {
    log_format = "Text"
    log_group  = aws_cloudwatch_log_group.billing[0].name
  }
  depends_on = [aws_iam_role_policy.billing]
}
resource "aws_apigatewayv2_integration" "billing" {
  count                  = local.bill
  api_id                 = aws_apigatewayv2_api.http.id
  integration_type       = "AWS_PROXY"
  integration_uri        = aws_lambda_function.billing[0].invoke_arn
  payload_format_version = "2.0"
  timeout_milliseconds   = 29000
}
resource "aws_apigatewayv2_route" "billing" {
  for_each  = var.enable_billing ? toset(["POST /api/billing/{proxy+}", "POST /api/stripe/webhook"]) : toset([])
  api_id    = aws_apigatewayv2_api.http.id
  route_key = each.value
  target    = "integrations/${aws_apigatewayv2_integration.billing[0].id}"
}
resource "aws_lambda_permission" "billing" {
  count         = local.bill
  statement_id  = "APIGateway"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.billing[0].function_name
  principal     = "apigateway.amazonaws.com"
  source_arn    = "${aws_apigatewayv2_api.http.execution_arn}/*/*"
}

output "gpu_launch_template" { value = try(aws_launch_template.gpu_host[0].id, "") }
output "efs_id" { value = try(aws_efs_file_system.users[0].id, "") }
output "stripe_secret_arn" { value = try(aws_secretsmanager_secret.stripe[0].arn, "") }
output "stripe_webhook_url" { value = "${local.origin}/api/stripe/webhook" }
