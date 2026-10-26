# MEA Stream

Browser-native cloud gaming for Mise En Abyme Eleet desktops: WebRTC from the
GPU host to the browser, NVENC encoding, keyboard/mouse/gamepad back. No
native app, no always-on media server. Guacamole stays the standard desktop
and the fallback.

```
browser (mea-web /desktop/gaming)
   │ 1. POST /api/desktops/{id}/stream   → MEA API (Lambda) checks Eleet + ownership,
   │                                       mints a 60 s single-use Ed25519 token
   │ 2. POST https://stream.<domain>/v1/sessions {token, SDP offer}
   ▼
mea-stream-gateway  (GPU host, one process, UDP/TCP 47998 for all sessions)
   │ verifies token (host-bound), checks capacity, looks up the desktop's agent
   │ TCP link (private LXD bridge), framed: video AUs, Opus, input, control
   ▼
mea-stream-agent    (inside each Eleet LXD container, as the desktop user)
   mea-capture: NvFBC → CUDA → NVENC (zero-copy), or ffmpeg x11grab + nvenc
   Opus from PulseAudio · XTest keyboard/mouse · uinput virtual pads
```

| Path | What |
|---|---|
| `docs/PROTOCOL.md` | MEA Stream Protocol v1 (normative) |
| `token/` | stream tokens (the MEA API mirror is `mvp/internal/streamtoken`, pinned by a golden vector) |
| `protocol/` | control messages, binary input, agent link framing |
| `agent/` | agent: media backends, input injection |
| `gateway/` | WebRTC gateway: sessions, ABR, capacity, metrics, admin API |
| `capture/` | `mea-capture`, the NvFBC + NVENC helper (C, dlopen()s the driver) |
| `cmd/` | `mea-stream-gateway`, `mea-stream-agent`, `mea-stream-registry`, `mea-stream-token`, `mea-stream-bench` |
| `deploy/` | LXD profile, container and host systemd units, env examples |
| `e2e/` | browser end-to-end test (Chromium, real gateway and agent on Xvfb) |
| `docs/DEPLOY.md`, `docs/TROUBLESHOOTING.md`, `docs/BENCHMARK.md`, `docs/COST.md` | operations |

## Develop and test

```sh
go test -race ./...                       # unit + integration (Pion client E2E, ffmpeg, Xvfb XTest)
go build -o bin/ ./cmd/... ./e2e/xprobe

# browser end-to-end: the real Gaming Mode page in Chromium
(cd ../../mea-web/newbie && npm ci && npm run build)
BIN=$PWD/bin MEA_WEB_DIST=../../mea-web/newbie/dist node e2e/gaming.e2e.mjs
```

The e2e checks video and audio playback, mouse and keyboard reaching X,
frame rate under motion, stats, live profile changes, token replay and
unknown-desktop rejection, and that held keys are released on disconnect.
It uses software x264; it says nothing about NVENC or L4 performance.

`mea-capture` builds only where the NVIDIA Capture SDK is installed (its
header is not redistributed here): `make -C capture CAPTURE_SDK=/opt/nvidia/Capture_Linux_v9.0.0`.

## Security model

- Only the MEA API holds the token signing key (Secrets Manager). Gateways
  and agents have the public key, so a compromised GPU host cannot mint
  access to another desktop.
- Tokens: 60 s, single use (jti), bound to user, desktop, stream session and
  host; agents additionally check the desktop. Only `eleet` and `admin`
  subscriptions can be issued one; Power desktops never get the `eleet-gpu`
  LXD profile, so there is no GPU to reach even with a forged path.
- The gateway only dials registry addresses on private or loopback ranges,
  never link-local (IMDS).
- CORS allows only the MEA origin; the admin API listens on localhost with a
  bearer token.
