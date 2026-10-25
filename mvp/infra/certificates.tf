resource "aws_acm_certificate" "edge" {
  provider                  = aws.edge
  domain_name               = var.domain
  subject_alternative_names = ["www.${var.domain}"]
  validation_method         = "DNS"
  lifecycle { create_before_destroy = true }
}
resource "aws_acm_certificate" "desktop" {
  domain_name       = "desktop.${var.domain}"
  validation_method = "DNS"
  lifecycle { create_before_destroy = true }
}
resource "aws_route53_record" "edge_validation" {
  provider        = aws.dns
  for_each        = { for dvo in aws_acm_certificate.edge.domain_validation_options : dvo.domain_name => { name = dvo.resource_record_name, record = dvo.resource_record_value, type = dvo.resource_record_type } }
  zone_id         = var.zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}
resource "aws_route53_record" "desktop_validation" {
  provider        = aws.dns
  for_each        = { for dvo in aws_acm_certificate.desktop.domain_validation_options : dvo.domain_name => { name = dvo.resource_record_name, record = dvo.resource_record_value, type = dvo.resource_record_type } }
  zone_id         = var.zone_id
  name            = each.value.name
  type            = each.value.type
  records         = [each.value.record]
  ttl             = 60
  allow_overwrite = true
}
resource "aws_acm_certificate_validation" "edge" {
  provider                = aws.edge
  certificate_arn         = aws_acm_certificate.edge.arn
  validation_record_fqdns = [for r in aws_route53_record.edge_validation : r.fqdn]
}
resource "aws_acm_certificate_validation" "desktop" {
  certificate_arn         = aws_acm_certificate.desktop.arn
  validation_record_fqdns = [for r in aws_route53_record.desktop_validation : r.fqdn]
}
resource "aws_sesv2_email_identity" "domain" { email_identity = var.domain }
resource "aws_route53_record" "ses_dkim" {
  provider = aws.dns
  count    = 3
  zone_id  = var.zone_id
  name     = "${aws_sesv2_email_identity.domain.dkim_signing_attributes[0].tokens[count.index]}._domainkey.${var.domain}"
  type     = "CNAME"
  ttl      = 300
  records  = ["${aws_sesv2_email_identity.domain.dkim_signing_attributes[0].tokens[count.index]}.dkim.amazonses.com"]
}
