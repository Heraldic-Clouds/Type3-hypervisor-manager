# API examples

All requests use an API key from `type3 gen-key`. Write requests return
`202 Accepted` with an `operation`; poll it until `status` is `succeeded`,
`failed` or `cancelled`. The full contract is in [`api/openapi.yaml`](../api/openapi.yaml).

```sh
export T3=http://127.0.0.1:8080/api/v1
export KEY=t3_...                       # admin key for these examples
H=(-H "Authorization: Bearer $KEY" -H 'Content-Type: application/json')
```

## Monitoring

```sh
curl -s $T3/health                                  # no key needed
curl -s "${H[@]}" $T3/metrics
curl -s "${H[@]}" $T3/profiles | jq '.profiles[] | {name, defaults, max, gpu, root_access}'
curl -s "${H[@]}" "$T3/operations?resource_type=container&status=running"
curl -s "${H[@]}" $T3/operations/op-0123456789abcdef01234567
curl -s "${H[@]}" -X DELETE $T3/operations/op-0123456789abcdef01234567   # cancel
```

Operation while running:

```json
{"id":"op-5f...","type":"container.create","status":"running","resource_type":"container",
 "resource_id":"cust42-game1","host_id":"node1","actor":"admin","stage":"creating instance",
 "progress":10,"progress_detail":"rootfs: 42% (18.20MB/s)","lxd_operation":"b3c1...", "...": "..."}
```

## Hosts

```sh
# Remote LXD (mTLS). server_cert is the host's /var/snap/lxd/common/lxd/server.crt
curl -s "${H[@]}" -d "$(jq -n --rawfile c lxd-server.crt \
  '{name:"node1", endpoint:"https://10.0.0.5:8443", server_cert:$c, labels:{zone:"eu-1"}}')" $T3/hosts

# Local LXD over the unix socket
curl -s "${H[@]}" -d '{"name":"local","endpoint":"unix://"}' $T3/hosts

# A machine that is still booting (registered offline, picked up by the health loop)
curl -s "${H[@]}" -d "$(jq -n --rawfile c lxd-server.crt \
  '{name:"node2", endpoint:"https://10.0.0.6:8443", server_cert:$c, allow_offline:true}')" $T3/hosts

curl -s "${H[@]}" $T3/hosts
curl -s "${H[@]}" $T3/hosts/node1
curl -s "${H[@]}" $T3/hosts/node1/resources | jq '{cpu, memory, gpu_capable, gpu: .gpu.cards, allocated, allocatable}'
curl -s "${H[@]}" $T3/hosts/node1/health
curl -s "${H[@]}" $T3/hosts/node1/containers
curl -s "${H[@]}" -X POST $T3/hosts/node1/reconcile
curl -s "${H[@]}" -X DELETE $T3/hosts/node1            # 409 while it has containers
curl -s "${H[@]}" -X DELETE "$T3/hosts/node1?force=true"  # forget records, keep LXD instances
```

## Containers

```sh
# Create from the tier defaults; host chosen automatically
curl -s "${H[@]}" -H 'Idempotency-Key: order-1001-create' \
  -d '{"name":"cust42-game1","owner_id":"cust-42","tier":"power"}' $T3/containers

# Explicit host, adjusted resources (within tier bounds) and an SSH key
curl -s "${H[@]}" -d '{"name":"cust7-eleet","owner_id":"cust-7","tier":"eleet","host_id":"gpu-node1",
  "resources":{"cpu":6,"memory_mib":16384},
  "ssh_authorized_keys":["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... user@laptop"]}' $T3/containers

# Admin only: a non-default image (alias or fingerprint already on the host, or on an image server)
curl -s "${H[@]}" -d '{"name":"cust9-base","owner_id":"cust-9","tier":"power","image":{"alias":"mea-base-24.04"}}' $T3/containers

curl -s "${H[@]}" "$T3/containers?owner_id=cust-42"
curl -s "${H[@]}" $T3/containers/cust42-game1              # live state + IPs
curl -s "${H[@]}" "$T3/containers/cust42-game1?live=false"  # recorded state only

curl -s "${H[@]}" -X POST $T3/containers/cust42-game1/stop
curl -s "${H[@]}" -X POST $T3/containers/cust42-game1/start
curl -s "${H[@]}" -X POST $T3/containers/cust42-game1/restart -d '{"timeout_seconds":30}'
curl -s "${H[@]}" -X POST $T3/containers/cust42-game1/stop -d '{"force":true,"reason":"hung game process"}'  # admin

curl -s "${H[@]}" -X PATCH $T3/containers/cust42-game1/resources -d '{"cpu":3,"memory_mib":8192,"disk_gib":60}'

curl -s "${H[@]}" -X POST $T3/containers/cust42-game1/snapshots -d '{"name":"before-update"}'
curl -s "${H[@]}" $T3/containers/cust42-game1/snapshots
curl -s "${H[@]}" -X POST $T3/containers/cust42-game1/snapshots/before-update/restore
curl -s "${H[@]}" -X DELETE $T3/containers/cust42-game1/snapshots/before-update

curl -s "${H[@]}" -X DELETE $T3/containers/cust42-game1          # must be stopped
curl -s "${H[@]}" -X DELETE $T3/containers/cust42-game1 -d '{"force":true,"reason":"subscription ended"}'  # admin
```

## Images

```sh
curl -s "${H[@]}" "$T3/images?host_id=node1"
curl -s "${H[@]}" $T3/images                                  # all hosts
curl -s "${H[@]}" "$T3/images/remote?server=ubuntu&q=24.04" | jq '.images[:5]'

# Pull ubuntu 24.04 onto node1 as alias ubuntu-noble; roll back if the fingerprint differs
curl -s "${H[@]}" -d '{"host_id":"node1","server":"ubuntu","alias":"24.04","aliases":["ubuntu-noble"],
  "expected_fingerprint":"<64-hex fingerprint from images/remote>"}' $T3/images/import

curl -s "${H[@]}" -X DELETE "$T3/images/<fingerprint>?host_id=node1"            # 409 if in use
curl -s "${H[@]}" -X DELETE "$T3/images/<fingerprint>?host_id=node1&force=true"
```

## Errors

Every error has the same shape:

```json
{"error":{"code":"gpu_not_allowed","message":"tier power does not include GPU access",
          "details":{"tier":"power"},"request_id":"req-1f2e3d4c5b6a7980"}}
```

| Status | Typical codes |
| --- | --- |
| 400 | `invalid_json`, `invalid_name`, `resource_limit_exceeded`, `reason_required`, `public_endpoint_rejected` |
| 401 | `unauthenticated` |
| 403 | `forbidden`, `gpu_not_allowed` |
| 404 | `container_not_found`, `host_not_found`, `operation_not_found`, `route_not_found` |
| 409 | `container_exists`, `container_running`, `operation_in_progress`, `idempotency_key_reused`, `no_host_available`, `host_has_containers`, `image_in_use` |
| 422 | `gpu_unavailable`, `fingerprint_mismatch` |
| 503 | `host_unavailable`, `host_unreachable` |
