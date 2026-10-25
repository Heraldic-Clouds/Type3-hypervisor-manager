resource "random_password" "database" {
  length  = 40
  special = false
}
resource "random_password" "app" {
  length  = 40
  special = false
}
resource "random_password" "guac" {
  length  = 40
  special = false
}
resource "random_id" "bridge" { byte_length = 16 }
resource "aws_db_subnet_group" "database" {
  name       = local.prefix
  subnet_ids = aws_subnet.private[*].id
}
resource "aws_db_instance" "database" {
  identifier                      = local.prefix
  engine                          = "postgres"
  engine_version                  = "17.6"
  instance_class                  = "db.t4g.micro"
  allocated_storage               = 20
  max_allocated_storage           = 20
  storage_type                    = "gp3"
  storage_encrypted               = true
  db_name                         = "mea"
  username                        = "mea_owner"
  password                        = random_password.database.result
  multi_az                        = false
  publicly_accessible             = false
  db_subnet_group_name            = aws_db_subnet_group.database.name
  vpc_security_group_ids          = [aws_security_group.database.id]
  backup_retention_period         = 1
  skip_final_snapshot             = true
  deletion_protection             = false
  auto_minor_version_upgrade      = true
  apply_immediately               = true
  enabled_cloudwatch_logs_exports = ["postgresql"]
}
resource "aws_secretsmanager_secret" "database" {
  for_each                = toset(["owner", "app", "guac"])
  name                    = "${local.prefix}/database/${each.key}"
  recovery_window_in_days = 0
}
resource "aws_secretsmanager_secret_version" "owner" {
  secret_id = aws_secretsmanager_secret.database["owner"].id
  secret_string = jsonencode({
    URL          = "postgres://mea_owner:${random_password.database.result}@${aws_db_instance.database.endpoint}/mea?sslmode=verify-full&sslrootcert=/var/task/schema/rds-ca.pem"
    GuacKey      = random_id.bridge.hex
    AppPassword  = random_password.app.result
    GuacPassword = random_password.guac.result
  })
}
resource "aws_secretsmanager_secret_version" "app" {
  secret_id = aws_secretsmanager_secret.database["app"].id
  secret_string = jsonencode({
    URL     = "postgres://mea_app:${random_password.app.result}@${aws_db_instance.database.endpoint}/mea?sslmode=verify-full&sslrootcert=/var/task/schema/rds-ca.pem"
    GuacKey = random_id.bridge.hex
  })
}
resource "aws_secretsmanager_secret_version" "guac" {
  secret_id     = aws_secretsmanager_secret.database["guac"].id
  secret_string = jsonencode({ password = random_password.guac.result, jsonKey = random_id.bridge.hex })
}
resource "aws_cloudwatch_log_group" "main" {
  for_each          = local.log_groups
  name              = "/mea/${var.environment}/${each.key}"
  retention_in_days = 7
}

