# Troubleshooting Gaming Mode

Start with the gateway: `journalctl -u mea-stream-gateway -f` and
`curl -s -H "Authorization: Bearer $(cat /etc/mea-stream/admin.token)" http://127.0.0.1:47990/v1/metrics | jq`.
In the browser, the stats button (pulse icon) shows codec, fps, bitrate, RTT,
loss, decode time and whether the route is direct or TURN.

| Symptom | Likely cause | Check / fix |
|---|---|---|
| Dashboard has no "Connect to Gaming Desktop" | plan is not Eleet/admin, or desktop not ready | `subscription` in `/api/me` |
| "Gaming Mode is not available" (404) | API has no stream config | `MEA_STREAM_SECRET_ARN` on the API Lambda; Lambda log says why it disabled Gaming Mode |
| "This desktop is open in another window" | a Guacamole or Gaming view is open elsewhere | the page offers "Switch to Gaming Mode here" (takeover) |
| 4003 / 403 from the gateway | token for another host | `MEA_STREAM_HOST_ID` (API) must equal `MEA_HOST_ID` (gateway) |
| 4001 / 401 | expired, replayed or wrongly signed token | clocks (chrony) on host; API and gateway use the same key pair |
| 4100 "not responding yet" | agent not registered or not listening | `cat /var/lib/type3/stream-agents.json`; `lxc config get <ct> user.mea.desktop_id`; `lxc exec <ct> -- systemctl status mea-stream-agent` |
| 4029 host busy | capacity limit | `/v1/metrics` → `admitReason`; raise `maxSessions` only after benchmarking |
| Stuck on "Starting Gaming Mode…" | UDP 47998 blocked | SG rules; try another network; enable TURN (`stream_turn = true`) |
| Black video, input works | X not on the GPU or capture failed | `lxc exec <ct> -- journalctl -u mea-stream-agent`; `mea-capture --probe` prints `{"codecs":[...]}` when NvFBC+NVENC work |
| `mea-capture --probe` → `{"codecs":[]}` | NvFBC unavailable | NvFBC needs an NVIDIA X screen (`mea-xorg`), the `display` driver capability in the LXD profile and a datacenter/Quadro GPU (L4 is supported); agent falls back to ffmpeg nvenc with `MEA_ENCODER=auto` |
| Xorg fails on the GPU | BusID or driver mismatch | `nvidia-xconfig --query-gpu-info` for BusID; the container's X driver must be the host driver version exactly. On some headless setups `ConnectedMonitor` must be replaced by `Option "UseDisplayDevice" "None"` (to validate on the L4) |
| Several desktops, only one X works | multiple X servers sharing one GPU | each container runs its own Xorg on `:0`; this needs validation on the L4 in Phase B. Fallback: per-desktop Xorg with `-sharevts` on distinct VTs |
| Choppy at 60 fps | encoder or network | stats overlay: `encode` > 8 ms → lower profile; loss > 2 % → the ABR already lowers bitrate; try Economy |
| Mouse drifts in games | pointer not locked | click the mouse icon (pointer lock, raw movement) |
| Keys stuck after alt-tab | should not happen | client releases on blur/hide; agent releases on link loss (3.5 s heartbeat). Report with agent log |
| No gamepad | Gamepad API needs a button press first | press a button; host needs `/dev/uinput` passed by the `eleet-gpu` profile |
| No sound | browser autoplay | unmute with the speaker button; agent needs PulseAudio in the session |
