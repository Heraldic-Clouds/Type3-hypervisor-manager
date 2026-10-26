# Deploying MEA Stream to staging

Prerequisites: the Milestone 2 GPU host (g6.4xlarge, one NVIDIA L4) running
Type3 and LXD, with Eleet desktops as LXD containers. One GPU host at most.

All AWS changes use `--profile stratus2-tantalus`; Route53 uses
`--profile default` only. Nothing here starts an instance.

## 1. Keys

```sh
go run ./cmd/mea-stream-token keygen
# MEA_STREAM_PUBLIC_KEY=...   → gateway and agents
# MEA_STREAM_PRIVATE_KEY=...  → Secrets Manager only
TURN_SECRET=$(head -c 32 /dev/urandom | base64)
```

## 2. Terraform (mvp/infra)

```hcl
enable_stream                 = true
stream_host_id                = "<Type3 host id>"
stream_public_ip              = "<GPU host Elastic IP>"
stream_host_security_group_id = "<GPU host SG>"
stream_turn                   = true   # optional
stream_stun_urls              = ["stun:stream.miseenaby.me:3478"]
```

`terraform apply` opens 443/tcp and 47998/udp+tcp (and coturn ports if
enabled) on the existing host SG, creates `stream.miseenaby.me` (when
`enable_dns`), one Secrets Manager secret, and passes `MEA_STREAM_*` to the
API Lambda. Then write the secret value (it never enters Terraform state):

```sh
aws secretsmanager put-secret-value --profile stratus2-tantalus --region us-west-2 \
  --secret-id <stream secret arn> \
  --secret-string "{\"PrivateKey\":\"$MEA_STREAM_PRIVATE_KEY\",\"TURNSecret\":\"$TURN_SECRET\"}"
```

Redeploy the API Lambda (it reads the secret at cold start) and the mea-web
build that contains `/desktop/gaming`.

## 3. GPU host

```sh
go build -o bin/ ./cmd/...           # CGO_ENABLED=0, linux/amd64
make -C capture CAPTURE_SDK=...      # on a machine with the Capture SDK
scp bin/mea-stream-* deploy/host/* gpu-host:/tmp/mea/
ssh gpu-host 'cd /tmp/mea && sudo sh install-host.sh'
sudoedit /etc/mea-stream/gateway.env  # from gateway.env.example
sudo systemctl restart mea-stream-gateway
curl -s https://stream.miseenaby.me/healthz
```

Optional TURN: `apt install coturn`, `deploy/host/turnserver.conf.example`
as `/etc/turnserver.conf`.

## 4. Eleet desktop image

```sh
lxc profile create eleet-gpu && lxc profile edit eleet-gpu < deploy/lxd/eleet-gpu.yaml
NV=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader)
lxc file push bin/mea-stream-agent capture/mea-capture deploy/container/* <ct>/root/
lxc exec <ct> -- env NVIDIA_VERSION=$NV sh /root/setup-container.sh
```

Type3, when it creates an Eleet desktop container, must:

1. attach the `eleet-gpu` profile (Eleet only, never Power);
2. `lxc config set <ct> user.mea.desktop_id <MEA desktop id>` — the registry
   picks it up within 5 s;
3. write `/etc/mea-stream/agent.env` (desktop id + public key) and the VNC
   password for the Guacamole fallback, then start the units.

## 5. Host shutdown guard

Before Type3 or the M2 idle policy stops the GPU host, it must check:

```sh
curl -s -H "Authorization: Bearer $(cat /etc/mea-stream/admin.token)" \
  http://127.0.0.1:47990/v1/metrics | jq .activeSessions   # 0 = safe
# drain first: PUT /v1/limits {"draining": true} refuses new sessions
```

## 6. Capacity

`MEA_MAX_SESSIONS=1` until `docs/BENCHMARK.md` has real L4 numbers. Change
live without a restart:

```sh
# fields given are merged into the current limits
curl -X PUT -H "Authorization: Bearer $TOKEN" -d '{"maxSessions":2}' http://127.0.0.1:47990/v1/limits
```

## Rollback

Set `enable_stream = false` (removes the ports, DNS and secret, and Gaming
Mode disappears from the API), or just stop `mea-stream-gateway`: the
dashboard's standard desktop is unaffected.
