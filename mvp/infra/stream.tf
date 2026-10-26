# MEA Stream (Gaming Mode). Off by default: with enable_stream = false this
# file creates nothing. Enabling it adds no compute; it opens the media ports
# on the existing GPU host's security group, adds stream.<domain> in Route53
# (DNS account), and one Secrets Manager secret for the token signing key.
# The GPU host itself is managed by Type3 / Milestone 2.
variable "enable_stream" { default = false }
variable "stream_host_id" {
  default     = ""
  description = "Type3 host id of the GPU host; must equal MEA_HOST_ID on its gateway."
}
variable "stream_public_ip" { default = "" }
variable "stream_host_security_group_id" { default = "" }
variable "stream_media_port" { default = 47998 }
variable "stream_turn" {
  default     = false
  description = "Open coturn ports on the GPU host for networks that block UDP."
}
variable "stream_stun_urls" { default = [] }

locals {
  stream     = var.enable_stream
  stream_dns = "stream.${var.domain}"
  stream_ports = local.stream && var.stream_host_security_group_id != "" ? merge(
    {
      signal_tcp = { proto = "tcp", from = 443, to = 443 }
      media_udp  = { proto = "udp", from = var.stream_media_port, to = var.stream_media_port }
      media_tcp  = { proto = "tcp", from = var.stream_media_port, to = var.stream_media_port }
    },
    var.stream_turn ? {
      turn_udp   = { proto = "udp", from = 3478, to = 3478 }
      turn_tcp   = { proto = "tcp", from = 3478, to = 3478 }
      turns_tcp  = { proto = "tcp", from = 5349, to = 5349 }
      turn_relay = { proto = "udp", from = 49160, to = 49200 }
    } : {}
  ) : {}
  stream_env = local.stream ? merge(
    {
      MEA_STREAM_SECRET_ARN  = aws_secretsmanager_secret.stream[0].arn
      MEA_STREAM_HOST_ID     = var.stream_host_id
      MEA_STREAM_GATEWAY_URL = "https://${local.stream_dns}"
    },
    length(var.stream_stun_urls) > 0 ? { MEA_STUN_URLS = join(",", var.stream_stun_urls) } : {},
    var.stream_turn ? { MEA_TURN_URLS = "turn:${local.stream_dns}:3478?transport=udp,turn:${local.stream_dns}:3478?transport=tcp,turns:${local.stream_dns}:5349?transport=tcp" } : {}
  ) : {}
}

check "stream_inputs" {
  assert {
    condition     = !var.enable_stream || (var.stream_host_id != "" && can(regex("^\\d+\\.\\d+\\.\\d+\\.\\d+$", var.stream_public_ip)))
    error_message = "enable_stream needs stream_host_id and stream_public_ip."
  }
}

# The value is written out of band so the private key never enters state:
#   aws secretsmanager put-secret-value --profile stratus2-tantalus \
#     --secret-id <arn> --secret-string '{"PrivateKey":"...","TURNSecret":"..."}'
resource "aws_secretsmanager_secret" "stream" {
  count       = local.stream ? 1 : 0
  name        = "${local.prefix}-stream"
  description = "MEA Stream token signing key (Ed25519 seed) and TURN shared secret"
}
resource "aws_iam_role_policy" "stream_secret" {
  count  = local.stream ? 1 : 0
  role   = aws_iam_role.lambda["api"].name
  policy = jsonencode({ Version = "2012-10-17", Statement = [{ Effect = "Allow", Action = ["secretsmanager:GetSecretValue"], Resource = [aws_secretsmanager_secret.stream[0].arn] }] })
}
resource "aws_vpc_security_group_ingress_rule" "stream" {
  for_each          = local.stream_ports
  security_group_id = var.stream_host_security_group_id
  description       = "MEA Stream ${each.key}"
  ip_protocol       = each.value.proto
  from_port         = each.value.from
  to_port           = each.value.to
  cidr_ipv4         = "0.0.0.0/0"
}
resource "aws_route53_record" "stream" {
  provider = aws.dns
  count    = local.stream && var.enable_dns ? 1 : 0
  zone_id  = var.zone_id
  name     = local.stream_dns
  type     = "A"
  ttl      = 60
  records  = [var.stream_public_ip]
}
