# Architecture and security model

## Components

```
            HTTP (internal network only, bearer API keys)
                         │
                 internal/httpapi  ── auth (roles) ── audit ── metrics
                         │
   ┌─────────────────────┼──────────────────────────┐
   │                     │                          │
containers.Manager   images.Manager        operations.Manager
   │   (lifecycle,       │ (list/import/          (async work, progress,
   │   snapshots,        │  delete)               timeouts, idempotency)
   │   reconcile)        │
   └──────────┬──────────┘
        hosts.Registry ── HostProvider (static; EC2 in M2)
              │
        lxdclient.Server  (narrow, context-aware interface)
              │
   canonical/lxd client ── mTLS ──▶ LXD hosts (standalone or cluster endpoint)

        store.Store (JSON file now, PostgreSQL in M3)
```

Interfaces named in the milestone brief map to code as follows:

| Interface | Location |
| --- | --- |
| `ContainerManager` | `internal/containers` |
| `HostRegistry` | `internal/hosts/registry.go` |
| `HostProvider`, `HostProvisioner` | `internal/hosts/provider.go` |
| `ImageManager` | `internal/images` |
| `StorageManager` | `internal/hosts/registry.go` (pools, networks) |
| `ResourceMonitor` | `internal/hosts/registry.go`; GPU `UtilizationMonitor` in `internal/gpu` |
| `OperationManager` | `internal/operations` |
| `Store` | `internal/store` |

## Hosts

* A host is registered with an endpoint (`https://private-ip:8443` or
  `unix://`), the LXD server certificate (pinned) and optionally per-host
  client identity references. Type3 probes it before saving it.
* Endpoints that resolve to public addresses are refused unless
  `TYPE3_ALLOW_PUBLIC_LXD_ENDPOINTS=true`. Plain `http://`, credentials in
  URLs and paths are refused.
* The health loop (`TYPE3_HEALTH_INTERVAL`) checks the API, the default
  network and the default storage pool. One failure marks a host
  `degraded`; `TYPE3_HOST_FAILURE_THRESHOLD` consecutive failures mark it
  `offline` and drop its connection. When it answers again it becomes
  `online`, capacity is rediscovered and its containers are reconciled.
* A transport failure during any container operation is reported to the
  registry, so a dead connection is never reused.
* LXD clusters: registering one member's endpoint works (LXD places
  instances; `location` is recorded) and cluster members are listed. Per-member
  targeting is left for the scheduler milestone.
* `Provider`/`ProviderRef`/`Labels` are where Milestone 2 attaches EC2 instance
  IDs and lifecycle; non-AWS dedicated servers use the `static` provider.

## Placement

Explicit `host_id` is always allowed if the host is not offline, has the
capacity and (for GPU tiers) a usable NVIDIA GPU. Without it, Type3 picks an
online, schedulable host that fits after overcommit
(`TYPE3_CPU_OVERCOMMIT`, `TYPE3_MEMORY_OVERCOMMIT`), preferring CPU-only hosts
for non-GPU tiers and then the most free memory. GPU sharing is capped at
`TYPE3_MAX_CONTAINERS_PER_GPU` (default 4) containers per NVIDIA GPU; this
limit has not been validated on GPU hardware. Anything smarter is Milestone 6.

## Containers

* Every instance is created with `type: container` (never VM),
  `security.privileged=false`, `security.nesting=false`,
  `security.idmap.isolated=true` (configurable), `limits.cpu`,
  `limits.memory` + `limits.memory.enforce=hard`, `limits.processes`, a sized
  root disk, and an `eth0` NIC with `security.port_isolation` and
  `security.mac_filtering`, so customers on one bridge cannot reach each
  other.
* LXD applies its generated AppArmor profile and default seccomp policy to
  unprivileged containers. Type3 never sets `raw.*`, `security.*` overrides,
  host disk mounts or other devices; profile files are checked against a
  denylist and the API never accepts raw LXD config.
* Root access (Power, Eleet) is passwordless sudo for the `mea` user
  inside the unprivileged container, set through cloud-init. Container root
  maps to an unprivileged host UID range. Newbie gets no sudo. Root's
  password is locked and SSH password login is off in every tier.
* Eleet adds an LXD `gpu` device (`gputype=physical`, `vendorid=10de`) and
  `nvidia.runtime=true`. Newbie and Power requests that ask for a GPU are
  refused with `403 gpu_not_allowed`.
* Managed instances carry `user.mea.managed`, `user.mea.owner`,
  `user.mea.tier` and `user.mea.spec-hash`. Type3 only acts on instances it
  has records for; unmanaged instances on a host are listed but never touched.

### Idempotency and partial failures

* Container names are unique across hosts. A create with the same name and the
  same spec (or the same `Idempotency-Key`) returns the existing container and
  operation (`200`, `existing: true`). Concurrent identical requests are
  serialized per name, so exactly one LXD instance is created.
* Only one operation runs per container. Repeating the same action while it
  runs returns the running operation; a different action gets `409
  operation_in_progress`.
* If LXD accepted a create but Type3 failed afterwards, the retry adopts the
  instance when its `user.mea.spec-hash` matches, and refuses when an
  instance with that name is not Type3's.
* Starting a running container or stopping a stopped one succeeds.
* Operations still running when Type3 stops are marked `failed`
  (`interrupted`) on the next start, and reconciliation repairs the records.

### Reconciliation

Runs at startup, every `TYPE3_RECONCILE_INTERVAL`, after a host reconnects, and
on `POST /api/v1/hosts/{id}/reconcile`. It refreshes status and IPs, marks
records whose instance vanished as `missing` (deleting such a record only
removes it), drops records of vanished ephemeral containers, and adopts
`user.mea.managed` instances without a record as `orphaned`.

## Operations

Every LXD change runs as an operation with a stage, a progress percentage (or
-1), LXD's progress detail (for example image download progress), a timeout
(`TYPE3_OPERATION_TIMEOUT`) and cancellation (`DELETE /operations/{id}`). LXD
operations are polled rather than waited on through the events websocket.
Finished operations are kept for `TYPE3_OPERATION_RETENTION`.

## Security controls

| Control | Implementation |
| --- | --- |
| Authenticated admin API | Bearer API keys, only SHA-256 hashes configured, constant-time comparison, Type3 refuses to start without keys |
| Role boundaries | viewer (read), operator (container lifecycle), admin (hosts, images, reconcile, forced stop/delete, image override) |
| mTLS to LXD | client cert + pinned server cert or CA; verification is never disabled |
| No public LXD API | private-address check at registration; LXD preseed binds to a private IP |
| Secrets | referenced by path/URI, resolved in memory, never logged, never written to disk; a logging hook redacts secret-looking keys and PEM keys |
| Input validation | strict JSON (unknown fields rejected, 1 MiB body limit), validated names, owner IDs, SSH keys, fingerprints, aliases, ranges |
| Audit log | JSON lines for every privileged request (accepted/rejected/denied, actor, reason) and every finished operation |
| Network exposure | Type3 binds to 127.0.0.1 by default; warns when serving plain HTTP elsewhere; optional TLS |
| State file | written atomically with mode 0600 |

Not in Milestone 1: per-customer API scoping (customers never call Type3
directly; the platform backend does), rate limiting, and an external secret
manager.
