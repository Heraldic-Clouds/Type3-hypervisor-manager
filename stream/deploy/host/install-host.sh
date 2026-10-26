#!/bin/sh
# Installs the gateway and registry on the GPU host (Type3 host).
#   sudo sh install-host.sh   (from a directory with the built binaries)
set -eu
id mea-stream >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin mea-stream
install -d -m 0750 -o mea-stream /etc/mea-stream
install -d -m 0755 /var/lib/type3
install -m 0755 mea-stream-gateway mea-stream-registry mea-stream-token /usr/local/bin/
install -m 0644 mea-stream-gateway.service mea-stream-registry.service /etc/systemd/system/
[ -f /etc/mea-stream/gateway.env ] || install -m 0600 -o mea-stream gateway.env.example /etc/mea-stream/gateway.env
[ -f /etc/mea-stream/admin.token ] || (umask 077; head -c 32 /dev/urandom | base64 > /etc/mea-stream/admin.token; chown mea-stream /etc/mea-stream/admin.token)
# Agents listen inside containers on the LXD bridge, which is not routed from
# outside the host; the security group opens only 443/tcp and 47998/udp+tcp.
systemctl daemon-reload
systemctl enable --now mea-stream-registry mea-stream-gateway
echo "edit /etc/mea-stream/gateway.env, then: systemctl restart mea-stream-gateway"
