// Ubuntu 24.04 LXD + NVIDIA host image for the Power/Eleet GPU host.
// Built on a non-GPU instance: the NVIDIA kernel modules are Canonical's
// prebuilt, signed packages for the AWS kernel, so no GPU is needed to build.
packer {
  required_plugins {
    amazon = { version = "= 1.7.0", source = "github.com/hashicorp/amazon" }
  }
}
variable "region" { default = "us-west-2" }
variable "profile" { default = "stratus2-tantalus" }
variable "subnet_id" { type = string }
variable "security_group_id" { type = string }
variable "instance_profile" { type = string }
variable "nvidia_branch" { default = "570" }
variable "lxd_channel" { default = "5.21/stable" }
source "amazon-ebs" "gpuhost" {
  region                      = var.region
  profile                     = var.profile
  ami_name                    = "mea-gpu-host-ubuntu-24.04-${formatdate("YYYYMMDDhhmmss", timestamp())}"
  instance_type               = "t3a.large"
  subnet_id                   = var.subnet_id
  security_group_id           = var.security_group_id
  associate_public_ip_address = true
  iam_instance_profile        = var.instance_profile
  communicator                = "ssh"
  ssh_username                = "ubuntu"
  ssh_interface               = "session_manager"
  ssh_timeout                 = "20m"
  ssh_clear_authorized_keys   = true
  source_ami_filter {
    filters     = { name = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*", virtualization-type = "hvm", root-device-type = "ebs" }
    owners      = ["099720109477"]
    most_recent = true
  }
  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = 40
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = true
  }
  tags     = { Project = "MiseEnAbyme", MEAImage = "gpu-host", ManagedBy = "Packer" }
  run_tags = { Project = "MiseEnAbyme", Name = "MEA temporary GPU host image builder" }
}
build {
  sources = ["source.amazon-ebs.gpuhost"]
  # A trailing-slash directory upload needs its destination to exist.
  provisioner "shell" {
    inline = ["mkdir -p /tmp/mea-files"]
  }
  provisioner "file" {
    source      = "files/"
    destination = "/tmp/mea-files"
  }
  provisioner "shell" {
    script           = "install-gpu-host.sh"
    environment_vars = ["NVIDIA_BRANCH=${var.nvidia_branch}", "LXD_CHANNEL=${var.lxd_channel}"]
    execute_command  = "chmod +x {{ .Path }}; sudo -E {{ .Vars }} {{ .Path }}"
  }
  post-processor "manifest" {
    output     = "manifest.json"
    strip_path = true
  }
}
