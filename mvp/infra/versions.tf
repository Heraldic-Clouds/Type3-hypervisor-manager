terraform {
  required_version = "= 1.17.0"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "= 6.39.0" }
    random = { source = "hashicorp/random", version = "= 3.8.1" }
  }
}
provider "aws" {
  region              = var.region
  profile             = var.profile
  allowed_account_ids = ["681773521032"]
  default_tags { tags = { Project = "MiseEnAbyme", Environment = var.environment, ManagedBy = "Terraform", CostCenter = "Newbie" } }
}
provider "aws" {
  alias               = "edge"
  region              = "us-east-1"
  profile             = var.profile
  allowed_account_ids = ["681773521032"]
}
provider "aws" {
  alias               = "dns"
  region              = "us-east-1"
  profile             = var.dns_profile
  allowed_account_ids = ["434925282829"]
}
variable "profile" { default = "stratus2-tantalus" }
variable "dns_profile" { default = "default" }
variable "region" { default = "us-west-2" }
variable "environment" { default = "staging" }
variable "domain" { default = "miseenaby.me" }
variable "zone_id" { default = "Z2VQV7OZ3AMOWY" }
variable "desktop_ami" { default = "ami-00000000000000000" }
variable "desktop_type" {
  default = "t3a.medium"
  validation {
    condition     = contains(["t3a.medium", "t3a.small"], var.desktop_type)
    error_message = "Only t3a.medium and t3a.small are approved."
  }
}
variable "desktop_cap" {
  default = 2
  validation {
    condition     = var.desktop_cap >= 1 && var.desktop_cap <= 2
    error_message = "The alpha safety cap is one or two desktops."
  }
}
variable "guac_desired_count" { default = 0 }
variable "guac_image_tag" { default = "newbie-v1" }
variable "enable_dns" { default = false }
variable "require_verification" { default = true }
variable "smtp_secret_arn" {
  type        = string
  default     = ""
  description = "Existing Secrets Manager SMTP JSON secret; empty selects SES. Credentials stay outside Terraform."
  validation {
    condition     = var.smtp_secret_arn == "" || startswith(var.smtp_secret_arn, "arn:aws:secretsmanager:${var.region}:681773521032:secret:")
    error_message = "Supply a Secrets Manager ARN in this deployment account and region, or leave empty for SES."
  }
}
variable "idle_seconds" { default = 1200 }
variable "warning_seconds" {
  default = 900
  validation {
    condition     = var.warning_seconds >= 0 && var.warning_seconds < var.idle_seconds
    error_message = "Warning must precede idle termination."
  }
}
variable "lifetime_seconds" {
  default = 14400
  validation {
    condition     = var.lifetime_seconds >= 300 && var.lifetime_seconds <= 14400
    error_message = "Alpha lifetime must be between five minutes and four hours."
  }
}
variable "grace_seconds" { default = 300 }
variable "budget_email" { default = "thomasdxdenton@hotmail.com" }
data "aws_caller_identity" "current" {}
data "aws_availability_zones" "available" { state = "available" }
locals {
  prefix       = "mea-${var.environment}"
  origin       = "https://${var.domain}"
  account      = data.aws_caller_identity.current.account_id
  log_groups   = toset(["api", "control", "migrate", "worker", "guacamole", "guacd", "workflow", "http"])
  lambda_trust = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }] })
}
