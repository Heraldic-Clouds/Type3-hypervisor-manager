#!/usr/bin/env bash
# Exercise a running Type3 server end to end with curl.
#
#   TYPE3_API_KEY=t3_...  ./scripts/api-smoke.sh            # read-only checks
#   TYPE3_API_KEY=t3_... SMOKE_LXD_ENDPOINT=https://10.0.0.5:8443 \
#     SMOKE_LXD_SERVER_CERT=/path/server.crt ./scripts/api-smoke.sh   # full lifecycle
#
# Needs curl and jq. The key must be an admin key for the lifecycle part.
set -euo pipefail

BASE=${TYPE3_URL:-http://127.0.0.1:8080}/api/v1
KEY=${TYPE3_API_KEY:?set TYPE3_API_KEY}
HOST=${SMOKE_HOST_NAME:-smoke-host}
NAME=${SMOKE_CONTAINER:-smoke-$(date +%H%M%S)}
TIER=${SMOKE_TIER:-power}

command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }

call() { # method path [json]
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -X "$method" -H "Authorization: Bearer $KEY" -w '\n%{http_code}')
  [[ -n $body ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  local out code
  out=$(curl "${args[@]}" "$BASE$path")
  code=${out##*$'\n'}
  RESP=${out%$'\n'*}
  echo "$method $path -> $code" >&2
  [[ $code =~ ^2 ]] || { echo "$RESP" | jq . >&2; return 1; }
}

wait_op() { # operation id
  local id=$1 status
  for _ in $(seq 1 900); do
    call GET "/operations/$id" 2>/dev/null
    status=$(jq -r .status <<<"$RESP")
    case $status in
      succeeded) echo "  op $id succeeded" >&2; return 0 ;;
      failed|cancelled) jq . <<<"$RESP" >&2; return 1 ;;
    esac
    printf '  %s: %s %s\r' "$(jq -r .stage <<<"$RESP")" "$(jq -r .progress <<<"$RESP")" "$(jq -r '.progress_detail // ""' <<<"$RESP")" >&2
    sleep 2
  done
  echo "operation $id did not finish" >&2; return 1
}

curl -fsS "$BASE/health" | jq .
call GET /profiles && jq -r '.profiles[] | "\(.name): \(.defaults.cpu) vCPU, \(.defaults.memory_mib) MiB, gpu=\(.gpu), root=\(.root_access)"' <<<"$RESP"
call GET /hosts && jq -r '.hosts[] | "\(.id) \(.status) \(.endpoint)"' <<<"$RESP"

if [[ -z ${SMOKE_LXD_ENDPOINT:-} ]]; then
  echo "SMOKE_LXD_ENDPOINT not set: read-only checks done." >&2
  exit 0
fi

if ! call GET "/hosts/$HOST" 2>/dev/null; then
  cert=""
  [[ -n ${SMOKE_LXD_SERVER_CERT:-} ]] && cert=$(jq -Rs . <"$SMOKE_LXD_SERVER_CERT")
  call POST /hosts "{\"name\":\"$HOST\",\"endpoint\":\"$SMOKE_LXD_ENDPOINT\"${cert:+,\"server_cert\":$cert}}"
fi
call GET "/hosts/$HOST/resources" && jq '{cpu: .cpu.threads, memory_mib: (.memory.total_bytes/1048576|floor), gpu: .gpu, allocated: .allocated}' <<<"$RESP"
call GET "/hosts/$HOST/health" && jq '{status, latency_ms, checks}' <<<"$RESP"

call POST /containers "{\"name\":\"$NAME\",\"owner_id\":\"smoke\",\"tier\":\"$TIER\",\"host_id\":\"$HOST\"}"
wait_op "$(jq -r .operation.id <<<"$RESP")"
call GET "/containers/$NAME" && jq '{id, status, ipv4, resources}' <<<"$RESP"

call POST "/containers/$NAME/stop" && wait_op "$(jq -r .operation.id <<<"$RESP")"
call POST "/containers/$NAME/start" && wait_op "$(jq -r .operation.id <<<"$RESP")"
call POST "/containers/$NAME/restart" && wait_op "$(jq -r .operation.id <<<"$RESP")"
call PATCH "/containers/$NAME/resources" '{"memory_mib":4096}' && wait_op "$(jq -r .operation.id <<<"$RESP")"
call DELETE "/containers/$NAME" '{"force":true,"reason":"smoke test cleanup"}' && wait_op "$(jq -r .operation.id <<<"$RESP")"
echo "smoke test passed" >&2
