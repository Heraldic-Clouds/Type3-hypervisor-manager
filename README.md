Type3 - Remote LXD server manager
==================================

Type3 is the container orchestration engine of **Mise En Abyme**. It manages
unprivileged LXD system containers across many Ubuntu hosts through an
authenticated, versioned REST API. Container hosts are "a new type of
virtualization" next to Type 1 and Type 2 hypervisors, and Type3 is their
remote administration tool.

Milestone 1 delivers the engine: multi-host registration over mTLS, health
and resource discovery (CPU, RAM, storage, NVIDIA GPUs), the full container
lifecycle, the Newbie/Power/Eleet subscription profiles, images, snapshots,
reconciliation, audit logging and metrics. AWS provisioning (M2), PostgreSQL
(M3), S3 images (M4) and GPU scheduling (M6) build on the interfaces here.

* API reference: [`api/openapi.yaml`](api/openapi.yaml)
* Example requests: [`docs/api-examples.md`](docs/api-examples.md)
* Architecture and security model: [`docs/architecture.md`](docs/architecture.md)
* Tests (unit and real LXD): [`docs/testing.md`](docs/testing.md)
* Milestone 1 plan and what happened to the old prototype: [`docs/milestone-1-plan.md`](docs/milestone-1-plan.md)

Quick start
-----------

Requirements: Go 1.27.3 (pinned in `go.mod`; an older Go downloads it
automatically with `GOTOOLCHAIN=auto`), `make`, `openssl`, and for real use an
Ubuntu host with LXD (`snap install lxd`). `curl` and `jq` for the smoke script.

```sh
git clone https://github.com/Heraldic-Clouds/Type3-hypervisor-manager
cd Type3-hypervisor-manager
cp .env.example .env

# 1. API key: prints the key once (keep it) and a name:role:hash entry
make gen-key NAME=admin ROLE=admin
#    put the printed entry into TYPE3_API_KEYS in .env

# 2. LXD client identity Type3 uses for mTLS
make client-cert
#    on every LXD host:  lxc config trust add config/type3-client.crt --name type3

# 3. Build, test and run
make test
make run          # listens on 127.0.0.1:8080 by default
```

Register a host and create a container (see
[`docs/api-examples.md`](docs/api-examples.md) for every endpoint):

```sh
export T3=http://127.0.0.1:8080/api/v1 KEY=t3_...   # the key from gen-key
CERT=$(jq -Rs . < lxd-server.crt)   # from the LXD host: /var/snap/lxd/common/lxd/server.crt

curl -sS -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d "{\"name\":\"node1\",\"endpoint\":\"https://10.0.0.5:8443\",\"server_cert\":$CERT}" $T3/hosts

curl -sS -H "Authorization: Bearer $KEY" $T3/hosts/node1/resources | jq

curl -sS -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-1001-create' \
  -d '{"name":"cust42-game1","owner_id":"cust-42","tier":"power"}' $T3/containers
# -> 202 with operation.id; poll GET $T3/operations/<id>
```

For a host on the same machine, use `"endpoint": "unix://"` (the LXD
socket; no certificates needed, run Type3 as a user in the `lxd` group).

Or drive the whole lifecycle with the smoke script:

```sh
TYPE3_API_KEY=$KEY SMOKE_LXD_ENDPOINT=https://10.0.0.5:8443 \
  SMOKE_LXD_SERVER_CERT=lxd-server.crt ./scripts/api-smoke.sh
```

Commands
--------

| Command | What it does |
| --- | --- |
| `make build` | builds `bin/type3` |
| `make run` | builds and runs `type3 serve` with `.env` |
| `make test` / `make test-race` | unit tests against an in-memory fake LXD |
| `make test-integration` | real LXD tests (needs `TYPE3_IT_LXD_ENDPOINT`) |
| `make check` | gofmt check, vet, race tests |
| `type3 gen-key NAME ROLE` | new API key + config entry |
| `type3 hash-key` | SHA-256 of a key read from stdin |
| `type3 check-config` | validate `.env`, keys and profiles |

Subscription profiles
---------------------

Defaults below are configurable (`TYPE3_PROFILES_FILE`, see
[`config/profiles.example.json`](config/profiles.example.json)); requests can
change resources only within each tier's min/max.

| Tier | Default | GPU | Customer access | Storage |
| --- | --- | --- | --- | --- |
| newbie | 1 vCPU, 1 GiB, 10 GiB disk | denied | no sudo | ephemeral (destroyed on stop) |
| power | 2 vCPU, 6 GiB, 40 GiB disk | denied | sudo inside the container | local (EFS later) |
| eleet | 4 vCPU, 12 GiB, 80 GiB disk | shared NVIDIA GPU | sudo inside the container | local (EFS later) |

All tiers are unprivileged system containers with nesting off, an isolated
idmap, hard memory limits and a port-isolated NIC.

Project layout
--------------

```
cmd/type3            server binary and helper subcommands
internal/            config, logging, secrets, store, lxdclient, hosts, gpu,
                     profiles, operations, containers, images, auth, audit,
                     metrics, httpapi
api/openapi.yaml     OpenAPI 3.1 specification
deploy/lxd/          LXD host preseed and reproducible Ubuntu 24.04 base image
config/              example API key and profile files (no secrets)
scripts/             api-smoke.sh
test/integration/    real LXD tests (build tag "integration")
legacy/              the original Lambda prototype, for reference
```

Dependencies
------------

* LXD Go client by Canonical (`github.com/canonical/lxd/client`, `shared/api`)
* AWS SDK for Go v2 (only to read certificates from `s3://` references)
* Go standard library for HTTP, logging (`log/slog`) and metrics
