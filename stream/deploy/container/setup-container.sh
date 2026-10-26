#!/bin/sh
# Prepares an Eleet desktop image for Gaming Mode (run once per image build).
# Needs the eleet-gpu LXD profile so the host's NVIDIA userspace is mounted.
#   NVIDIA_VERSION=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader)
#   lxc file push mea-stream-agent mea-capture setup-container.sh ... <ct>/root/
#   lxc exec <ct> -- env NVIDIA_VERSION=$NVIDIA_VERSION sh /root/setup-container.sh
set -eu
: "${NVIDIA_VERSION:?set NVIDIA_VERSION to the host driver version}"
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends xserver-xorg-core xfce4 xfce4-terminal dbus-x11 \
  x11-xserver-utils x11vnc pulseaudio ffmpeg libffmpeg-nvenc-dev mesa-utils vulkan-tools
# The X driver module must match the host kernel module exactly. LXD mounts
# the libraries; the Xorg module comes from the same .run, userspace only.
run=NVIDIA-Linux-x86_64-$NVIDIA_VERSION.run
[ -f /tmp/$run ] || curl -fsSLo /tmp/$run "https://us.download.nvidia.com/tesla/$NVIDIA_VERSION/$run"
sh /tmp/$run --silent --no-kernel-module --no-kernel-module-source --no-nvidia-modprobe \
  --no-systemd --no-check-for-alternate-installs --install-libglvnd
id mea >/dev/null 2>&1 || useradd -m -u 1000 -G input,audio,video -s /bin/bash mea
install -d -m 0755 /etc/mea-stream
install -m 0755 /root/mea-stream-agent /root/mea-capture /usr/local/bin/
install -m 0644 /root/xorg-nvidia.conf /etc/X11/xorg.conf
for u in mea-xorg mea-desktop mea-x11vnc mea-stream-agent; do
  install -m 0644 /root/$u.service /etc/systemd/system/
done
systemctl daemon-reload
systemctl enable mea-xorg mea-desktop mea-x11vnc mea-stream-agent
echo "ready: write /etc/mea-stream/agent.env, vnc.env and vnc.pass, then start the units"
