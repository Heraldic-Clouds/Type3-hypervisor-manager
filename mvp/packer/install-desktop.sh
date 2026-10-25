#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
cloud-init status --wait
apt-get update
apt-get install -y --no-install-recommends xfce4 xfce4-terminal xrdp xorgxrdp dbus-x11 x11-xserver-utils xprintidle thunar mousepad fonts-dejavu-core firefox python3 unzip curl ca-certificates
curl -fsSL https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
unzip -q /tmp/awscliv2.zip -d /tmp
/tmp/aws/install
rm -rf /tmp/aws /tmp/awscliv2.zip
useradd -m -s /bin/bash mea
passwd -l mea
echo 'startxfce4' > /home/mea/.xsession
chown mea:mea /home/mea/.xsession
adduser xrdp ssl-cert
sed -i 's/^max_bpp=.*/max_bpp=24/' /etc/xrdp/xrdp.ini
systemctl enable xrdp
systemctl stop xrdp
cat > /usr/local/sbin/mea-session-monitor <<'MONITOR'
#!/bin/bash
# Root-owned backstop. The control-plane watchdog is authoritative; this
# terminates the disposable machine if the desktop's actual X session is idle.
set -euo pipefail
idle=0
for auth in /home/mea/.Xauthority; do
 for display in $(find /tmp/.X11-unix -maxdepth 1 -name 'X*' -printf '%f\n' 2>/dev/null | sed 's/^X/:/'); do
  value=$(runuser -u mea -- env DISPLAY="$display" XAUTHORITY="$auth" xprintidle 2>/dev/null || true)
  if [[ "$value" =~ ^[0-9]+$ ]] && ((value>=1200000)); then shutdown -h now; fi
 done
done
MONITOR
chmod 700 /usr/local/sbin/mea-session-monitor
cat > /etc/systemd/system/mea-session-monitor.service <<'SERVICE'
[Unit]
Description=Mise En Abyme desktop inactivity backstop
[Service]
Type=oneshot
ExecStart=/usr/local/sbin/mea-session-monitor
SERVICE
cat > /etc/systemd/system/mea-session-monitor.timer <<'TIMER'
[Unit]
Description=Check actual desktop idle time every minute
[Timer]
OnBootSec=10min
OnUnitActiveSec=1min
[Install]
WantedBy=timers.target
TIMER
systemctl enable mea-session-monitor.timer
apt-get clean
rm -rf /var/lib/apt/lists/*
rm -f /home/ubuntu/.ssh/authorized_keys /root/.ssh/authorized_keys
cloud-init clean --logs
truncate -s 0 /etc/machine-id
sync

