# MEA Stream Protocol v1

Status: draft-1 (implemented by `stream/protocol`, `stream/gateway`, `stream/agent`
and `mea-web/newbie/src/gaming`). Version string: `mea-stream/1`.

MEA Stream is a small, versioned application protocol carried over WebRTC.
WebRTC (ICE, DTLS-SRTP, SCTP data channels, RTCP, transport-wide congestion
control) is the v1 transport. Nothing in the MEA layer depends on WebRTC
internals, so a future transport only has to provide: one encrypted media path
for video/audio, one reliable ordered message channel and one unreliable
unordered message channel.

## 1. Roles

| Role | Runs where | Responsibilities |
|---|---|---|
| **MEA API** (existing `mvp` Lambda) | AWS | Authenticates the MEA user, checks entitlement and desktop ownership, asks Type3 for placement, mints a stream token. Never carries media. |
| **Gateway** (`mea-stream-gateway`) | GPU host, one process | Verifies tokens, enforces capacity, terminates WebRTC for every session on the host on one UDP port, routes each session to exactly one agent, exports metrics to Type3. |
| **Agent** (`mea-stream-agent`) | Inside each Eleet LXD container | Captures that container's display and audio, drives NVENC, injects input into that container's session only. Never talks to browsers directly. |
| **Client** (React Gaming Mode) | Browser | Renders media, captures keyboard/mouse/gamepad, reports decode stats. |

Media path: `agent → gateway (LXD bridge, TCP) → browser (SRTP over UDP)`.
Signalling path: `browser → MEA API (token) → gateway HTTPS (offer/answer)`.
Video never passes Lambda, API Gateway, RDS, Fargate or a NAT gateway.

## 2. Session establishment

```
Browser            MEA API              Type3            Gateway            Agent
  | POST /api/desktops/{id}/stream ---->|                   |                 |
  |                  |-- reserve(host) ->|                   |                 |
  |<-- {token, gatewayUrl, iceServers} --|                   |                 |
  | POST {gatewayUrl}/v1/sessions  {token, offer, hello} ---->|                 |
  |                  |                   |   verify token, capacity          |
  |                  |                   |                   |-- link HELLO -->|
  |                  |                   |                   |<- link READY ---|
  |<---------------------------- {answer, welcome} ----------|                 |
  |=============== ICE / DTLS / SRTP / SCTP (UDP) ===========|                 |
  |<===================== video / audio ======================|<== frames =====|
  |== mea-rt / mea-key ======================================>|== input ======>|
```

The offer/answer exchange is a single HTTP round trip (WHIP style). The gateway
gathers its candidates before answering; it has one public host candidate on a
single muxed UDP port, plus TURN relay candidates when TURN is configured.

### 2.1 Stream token

`token = base64url(payload) "." base64url(ed25519_signature(payload))`

Payload is JSON:

| Field | Meaning |
|---|---|
| `v` | Token format version, `1`. |
| `jti` | Random 128-bit id. Single use: the gateway and the agent each reject a second use. |
| `sub` | MEA user id. |
| `dsk` | Desktop (LXD instance) id the user owns. |
| `sid` | Stream session id chosen by the MEA API. |
| `host` | Type3 host id the session was placed on. Gateways reject tokens for other hosts. |
| `ent` | Entitlement: `eleet` or `admin`. Anything else is rejected. |
| `iat`, `exp` | Unix seconds. Lifetime ≤ 120 s; 5 s clock skew allowed. |
| `prof` | Highest profile allowed (`economy`, `balanced`, `high`). |

Only the MEA API holds the Ed25519 private key (Secrets Manager). Gateways and
agents hold the public key, so a compromised GPU host cannot mint tokens.
Reconnection always uses a **new** token; reusing the `sid` within 60 s lets the
gateway resume the same agent session (same encoder, no desktop restart).

## 3. Data channels

| Label | Reliability | Payload | Use |
|---|---|---|---|
| `mea-control` | reliable, ordered | JSON (UTF-8) | Negotiation, configuration, stats, heartbeats, errors. |
| `mea-key` | reliable, ordered | binary input events | Keys, mouse buttons, gamepad connect, text. Never lost. |
| `mea-rt` | unordered, `maxRetransmits: 0` | binary input events | Pointer motion, wheel, gamepad snapshots. Newest wins. |

Control messages are JSON objects `{"t": "<type>", ...}`.

### 3.1 Control messages

| `t` | Direction | Fields |
|---|---|---|
| `hello` | C→S | `v` (supported versions), `codecs` (decodable, preference order), `screen` `{w,h,dpr,hz}`, `profile`, `gamepad` |
| `welcome` | S→C | `v` (chosen), `sid`, `codec`, `w`, `h`, `fps`, `maxKbps`, `audio`, `caps` |
| `configure` | C→S | any of `profile`, `w`, `h`, `fps`, `maxKbps`; answered with `configured` |
| `configured` | S→C | effective `w`, `h`, `fps`, `kbps`, `codec` |
| `keyframe` | C→S | request an IDR |
| `ping` / `pong` | both | `n` (sequence), `ts` (sender ms); every 1 s |
| `stats` | C→S | `fps`, `decodeMs`, `dropped`, `jitterMs`, `lossPct`, `rttMs`, `kbps` (1 s window) |
| `sstats` | S→C | `encMs`, `capMs`, `fps`, `kbps`, `targetKbps`, `idle`, `gpu`, `nvenc` |
| `release` | C→S | release all held keys/buttons |
| `suspend` / `resume` | C→S | tab hidden/visible; encoding stops while suspended |
| `error` | S→C | `code`, `msg`, `fatal` |
| `bye` | both | `reason` |

### 3.2 Binary input events

Little-endian. Every event starts with `type:u8`.

| type | Name | Body | Channel |
|---|---|---|---|
| 0x01 | key | `down:u8, keysym:u32` (X11 keysym) | key |
| 0x02 | button | `down:u8, button:u8` (1 left, 2 middle, 3 right, 8 back, 9 forward) | key |
| 0x03 | motion-rel | `dx:i16, dy:i16` | rt |
| 0x04 | motion-abs | `x:u16, y:u16` (0..65535 scaled to the remote surface) | rt |
| 0x05 | wheel | `dx:i16, dy:i16` (120 = one notch) | rt |
| 0x06 | gamepad | `pad:u8, seq:u16, buttons:u32, axes:4×i16, triggers:2×u8` | rt |
| 0x07 | gamepad-conn | `pad:u8, connected:u8` | key |
| 0x08 | text | `len:u16, utf8` (≤ 8 KiB) | key |

Gamepad events are full state snapshots (standard mapping: buttons 0–16 as
bits, axes LX LY RX RY, triggers LT RT). A receiver drops a snapshot whose `seq`
is older than the last applied (16-bit wrap-around comparison), so loss and
reordering never leave a button stuck.

### 3.3 Stuck-input rules

1. The agent tracks every key and button it pressed on behalf of the client.
2. On `release`, link loss, `mea-key` close, ICE disconnected > 2 s, or 3 missed
   heartbeats, the agent releases everything it holds and centres every pad.
3. The client sends `release` on `blur`, `visibilitychange`, pointer-lock exit
   and full-screen exit.

## 4. Codec negotiation

1. The client lists decodable codecs (`RTCRtpReceiver.getCapabilities`) in
   `hello.codecs` and in its SDP offer.
2. The agent reports encoder capabilities in link `READY` (NVENC on L4:
   `av1`,`h265`,`h264`; software: `h264`).
3. The gateway picks the first codec of the server preference (`av1`, `h265`,
   `h264`) present in the client list, the SDP offer and the agent list, and
   answers SDP with only that codec. H.264 (packetization-mode=1, constrained
   baseline) is mandatory for every client and agent.

## 5. Quality control

| Profile | Size | FPS | Start kbps | Max kbps |
|---|---|---|---|---|
| economy | 1280×720 | 30 | 3 000 | 6 000 |
| balanced | 1920×1080 | 60 | 8 000 | 15 000 |
| high | 1920×1080 | 60 | 12 000 | 25 000 |

Targets, not guarantees. The gateway runs Google congestion control (TWCC) per
peer connection. Every 500 ms the target bitrate becomes
`min(profile.max, client maxKbps, 0.85 × estimate)`, never below 500 kbps.
While the target stays below 40 % of the profile's start bitrate for 5 s the
frame rate is halved (60 → 30); it is restored after 15 s of headroom.
Encoder: no B-frames, no lookahead, infinite GOP with IDR on demand, CBR with
a one-frame VBV, low-latency presets `p1`–`p4`.

### 5.1 Idle behaviour

The capture helper reports whether each frame changed. Unchanged frames are not
encoded; after 2 s without change the stream drops to a 5 fps keep-alive and
returns to full rate on the next change. Input inactivity alone never marks a
session idle. `suspend` stops encoding entirely; the desktop keeps running.

## 6. Agent link (gateway ↔ agent)

TCP on the LXD bridge, container port 47100. Frame: `len:u32 | kind:u8 |
payload` (len counts kind + payload, big-endian, ≤ 4 MiB).

| kind | Name | Payload |
|---|---|---|
| 0x01 | HELLO | JSON `{token, codecs, profile, w, h, fps, kbps}` (gateway → agent) |
| 0x02 | READY | JSON `{codec, codecs, w, h, fps, audio, encoder}` |
| 0x10 | VIDEO | `flags:u8` (bit0 keyframe), `pts_us:u64`, access unit (Annex B or AV1 OBUs) |
| 0x11 | AUDIO | `pts_us:u64`, one Opus packet (48 kHz) |
| 0x20 | INPUT | one §3.2 event |
| 0x21 | CONTROL | JSON: `configure`, `keyframe`, `release`, `suspend`, `resume`, `bitrate{kbps}` |
| 0x30 | STATS | JSON agent/encoder stats |
| 0x7F | BYE | JSON `{code, reason}` |

The agent verifies the token itself and refuses one whose `dsk` is not its own
desktop id, so even a misrouted gateway cannot attach to the wrong desktop.

## 7. Error codes

| Code | Meaning | Client action |
|---|---|---|
| 1000 | normal close | none |
| 4001 | token invalid / expired / replayed | fetch a new token |
| 4003 | not entitled / wrong desktop / wrong host | stop |
| 4009 | taken over by a newer connection of the same user | stop |
| 4029 | host at capacity | retry later or use Guacamole |
| 4100 | agent unreachable | retry with backoff |
| 4101 | capture failed | use Guacamole |
| 4102 | encoder failed or saturated | retry with a lower profile |
| 4200 | unsupported protocol version | stop |
| 4400 | malformed message | stop |

## 8. Compatibility rules

* Ignore unknown JSON fields and unknown `t` values.
* Drop unknown binary input types; never treat them as errors.
* Versions are negotiated with `hello.v`; the server picks the highest common.
* v2 may add data channels but must keep the v1 labels while v1 is supported.

## 9. Session states

```
          token ok           ICE+DTLS up        welcome sent
 [new] ----------> [negotiating] ------> [connected] ------> [streaming]
   |                    |                     |                 |  ^
   | bad token          | timeout 15 s        | ICE lost >2 s   |  | resume
   v                    v                     v                 v  |
 [rejected]          [failed]            [recovering] <---- [suspended]
                                            |  new token, same sid, < 60 s
                                            +-----------> [negotiating]
                                            |  60 s elapsed
                                            v
                                         [closed]  agent releases input,
                                                   stops encoder, Type3
                                                   releases capacity
```
