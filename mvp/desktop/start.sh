#!/bin/bash
set -euo pipefail
printf 'mea:%s\n' "$LOCAL_DESKTOP_PASSWORD" | chpasswd
mkdir -p /run/dbus /var/run/xrdp
dbus-daemon --system --fork
/usr/sbin/xrdp-sesman
exec /usr/sbin/xrdp --nodaemon

