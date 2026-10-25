packer {
 required_plugins {
 amazon = { version = "= 1.7.0", source = "github.com/hashicorp/amazon" }
 }
}
variable "region" { default="us-west-2" }
variable "profile" { default="stratus2-tantalus" }
variable "subnet_id" { type=string }
variable "security_group_id" { type=string }
variable "instance_profile" { type=string }
source "amazon-ebs" "desktop" {
 region=var.region
 profile=var.profile
 ami_name="mea-ubuntu-24.04-xfce-${formatdate("YYYYMMDDhhmmss",timestamp())}"
 instance_type="t3a.medium"
 subnet_id=var.subnet_id
 security_group_id=var.security_group_id
 associate_public_ip_address=true
 iam_instance_profile=var.instance_profile
 communicator="ssh"
 ssh_username="ubuntu"
 ssh_interface="session_manager"
 ssh_timeout="20m"
 source_ami_filter {
 filters={name="ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*",virtualization-type="hvm",root-device-type="ebs"}
 owners=["099720109477"]
 most_recent=true
 }
 launch_block_device_mappings {
 device_name="/dev/sda1"
 volume_size=12
 volume_type="gp3"
 encrypted=true
 delete_on_termination=true
 }
 tags={Project="MiseEnAbyme",MEAImageVersion="newbie-v1",ManagedBy="Packer"}
 run_tags={Project="MiseEnAbyme",Name="MEA temporary golden-image builder"}
}
build {
 sources=["source.amazon-ebs.desktop"]
 provisioner "shell" {
 script="install-desktop.sh"
 execute_command="chmod +x {{ .Path }}; sudo -E {{ .Vars }} {{ .Path }}"
 }
 post-processor "manifest" {
 output="manifest.json"
 strip_path=true
 }
}

