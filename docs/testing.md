# Testing

## Unit tests (no LXD needed)

```sh
make test         # go test ./...
make test-race    # with the race detector
make check        # gofmt check + go vet + race tests
make cover        # coverage summary
```

Unit tests run against `internal/lxdclient/fake`, an in-memory LXD that
models instance states, snapshots, images, NVIDIA GPU inventory, operation
delays/hangs and host outages. They cover:

| Area | Tests |
| --- | --- |
| LXD connectivity | endpoint parsing, mandatory client cert and pinned cert/CA, transport error detection, operation polling and timeouts (`internal/lxdclient`) |
| Host registration and discovery | validation, public-endpoint rejection, duplicates, unreachable hosts, `allow_offline` (`internal/hosts`) |
| Host health and disconnection | degraded → offline thresholds, reconnect with a fresh connection and callback, stale container reads, ops failing with `host_unavailable` (`internal/hosts`, `internal/containers`) |
| Resources and GPU | CPU/RAM/storage/GPU inventory, allocation, nouveau vs nvidia, eligibility (`internal/hosts`, `internal/gpu`) |
| Container lifecycle | create/start/stop/restart/delete, forced stop safeguards, ephemeral Newbie, resource changes, snapshots (`internal/containers`) |
| Profiles and security | tier defaults/bounds, denylisted LXD keys, unprivileged/no-nesting/no-host-mount specs, sudo only on root tiers (`internal/profiles`) |
| Networking | port-isolated NIC, IPv4/IPv6 discovery (`internal/containers`) |
| Idempotency and duplicates | same key, same spec, concurrent identical creates, reused keys, one operation per container, adoption of partial creates (`internal/containers`, `internal/operations`) |
| Timeout and recovery | operation timeouts, cancellation, panics, restart recovery, shutdown draining (`internal/operations`) |
| Reconciliation | missing, drifted and orphaned instances (`internal/containers`) |
| Images | import with fingerprint verification and rollback, in-use protection (`internal/httpapi`) |
| API | unauthorized and forbidden access, error shape, strict JSON, full lifecycle over HTTP, audit records, metrics (`internal/httpapi`) |
| Secrets and logging | file/env/s3 references, log redaction (`internal/secrets`, `internal/logging`) |

## Integration tests (real LXD)

`test/integration` runs the same managers against a real LXD daemon. They are
built only with `-tags integration` and skip themselves unless
`TYPE3_IT_LXD_ENDPOINT` is set. They create and delete real containers
(names start with `type3-it-`) and download `ubuntu:24.04` on first run.

Local LXD on the machine running the tests:

```sh
sudo snap install lxd && sudo lxd init --auto
sudo usermod -aG lxd $USER && newgrp lxd
TYPE3_IT_LXD_ENDPOINT=unix:// make test-integration
```

Remote LXD over mTLS:

```sh
make client-cert                                   # on the Type3 machine
lxc config trust add config/type3-client.crt --name type3-it   # on the LXD host
scp lxd-host:/var/snap/lxd/common/lxd/server.crt ./lxd-server.crt

TYPE3_IT_LXD_ENDPOINT=https://10.0.0.5:8443 \
TYPE3_IT_SERVER_CERT=./lxd-server.crt \
TYPE3_IT_CLIENT_CERT=./config/type3-client.crt \
TYPE3_IT_CLIENT_KEY=./config/type3-client.key \
make test-integration
```

Optional variables: `TYPE3_IT_NETWORK` (default `lxdbr0`), `TYPE3_IT_POOL`
(default `default`), `TYPE3_IT_IMAGE` (default `24.04`),
`TYPE3_IT_IDMAP_ISOLATED=0` if the host's subuid range is too small for
isolated idmaps, and `TYPE3_IT_GPU=1` on a host with an NVIDIA GPU, driver and
`nvidia-container-toolkit` to also create an Eleet container.

## End-to-end smoke test

With a running server (`make run`):

```sh
TYPE3_API_KEY=t3_... ./scripts/api-smoke.sh                 # read-only
TYPE3_API_KEY=t3_... SMOKE_LXD_ENDPOINT=https://10.0.0.5:8443 \
  SMOKE_LXD_SERVER_CERT=./lxd-server.crt ./scripts/api-smoke.sh   # full lifecycle
```

## Verification status for Milestone 1

* Unit tests: run and passing (`go test -race ./...`).
* Server binary: built and started locally; health, authentication, role
  checks, public-endpoint rejection, offline host registration, placement
  refusal, metrics, audit log and graceful shutdown were exercised over HTTP.
* Real LXD integration tests: **not run**. The development container has no
  LXD daemon (no snap/systemd). Run `make test-integration` on an Ubuntu host
  with LXD before relying on the lifecycle against real hosts.
* GPU: **not verified on hardware**. GPU inventory, eligibility and the LXD
  `gpu` device definition are unit tested only. Shared NVIDIA access,
  `nvidia.runtime`, NVENC availability inside containers and the per-GPU
  sharing limit need a real NVIDIA host (Milestone 6 benchmark).
