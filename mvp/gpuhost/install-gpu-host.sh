#!/bin/bash
# Packer provisioner: bakes LXD, the NVIDIA driver and container runtime, the
# CloudWatch agent, Type3 and the MEA boot units into the GPU host image.
# Runtime configuration (EFS, LXD pool on the data volume, Type3 keys) happens
# on every boot in mea-host-boot, so the image holds no secrets.
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait || true
apt-get update
apt-get -y upgrade
# Canonical's signed, prebuilt NVIDIA modules for the AWS kernel plus the
# headless userspace (no X server on the host itself).
apt-get install -y --no-install-recommends "linux-modules-nvidia-${NVIDIA_BRANCH}-server-open-aws" \
  "nvidia-headless-no-dkms-${NVIDIA_BRANCH}-server-open" "nvidia-utils-${NVIDIA_BRANCH}-server" \
  nfs-common jq curl gnupg ca-certificates zfsutils-linux nftables unzip

# NVIDIA container library: LXD's nvidia.runtime uses nvidia-container-cli.
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
  | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#' \
  > /etc/apt/sources.list.d/nvidia-container-toolkit.list
apt-get update
apt-get install -y --no-install-recommends libnvidia-container1 libnvidia-container-tools

snap install lxd --channel="${LXD_CHANNEL}"
snap set lxd daemon.debug=false
# AWS CLI for Secrets Manager and SSM parameters at boot.
curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
unzip -q /tmp/awscliv2.zip -d /tmp && /tmp/aws/install && rm -rf /tmp/aws /tmp/awscliv2.zip
# CloudWatch agent with its native nvidia_gpu collector.
curl -fsSL https://amazoncloudwatch-agent.s3.amazonaws.com/ubuntu/amd64/latest/amazon-cloudwatch-agent.deb -o /tmp/cwagent.deb
dpkg -i /tmp/cwagent.deb && rm -f /tmp/cwagent.deb

install -d -m 755 /etc/type3 /var/lib/type3 /mnt/mea-efs /etc/mea
install -m 755 /tmp/mea-files/type3 /usr/local/bin/type3
install -m 644 /tmp/mea-files/profiles.json /etc/type3/profiles.json
install -m 644 /tmp/mea-files/desktop.cloud-init.yaml /etc/mea/desktop.cloud-init.yaml
install -m 755 /tmp/mea-files/mea-host-boot /usr/local/sbin/mea-host-boot
install -m 644 /tmp/mea-files/mea-host-boot.service /etc/systemd/system/mea-host-boot.service
install -m 644 /tmp/mea-files/type3.service /etc/systemd/system/type3.service
install -m 644 /tmp/mea-files/cloudwatch.json /opt/aws/amazon-cloudwatch-agent/etc/amazon-cloudwatch-agent.json
systemctl enable mea-host-boot.service
systemctl disable type3.service
systemctl disable nftables || true
# No SSH on the running host (Packer itself still needs it until the build
# ends): administration goes through SSM Session Manager.
systemctl disable ssh.socket ssh.service || true

apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/mea-files
cloud-init clean --logs
truncate -s 0 /etc/machine-id
sync
