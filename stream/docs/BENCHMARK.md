# Benchmark: concurrent streams on one NVIDIA L4 (Phase H)

**Status: not run. No L4 measurements exist yet.** The Milestone 2 GPU host
(g6.4xlarge) is not provisioned. Until this file has measured numbers,
`MEA_MAX_SESSIONS` stays at 1. Nothing below is a result.

## Method

1. One g6.4xlarge (1× L4 24 GB, 16 vCPU, 64 GiB), the production driver,
   four Eleet containers with the `eleet-gpu` profile, each running the same
   moving workload on its NVIDIA X screen (pick one and keep it: e.g. a
   looping game benchmark, or `vkcube` + a 1080p60 video for a floor).
2. Gateway with `MEA_MAX_SESSIONS=4` for the run only.
3. From a client machine near us-west-2:

   ```sh
   MEA_STREAM_PRIVATE_KEY=... MEA_ADMIN_TOKEN=... \
   mea-stream-bench -gateway https://stream.miseenaby.me -admin http://127.0.0.1:47990 \
     -host <host id> -desktops d1,d2,d3,d4 -steps 1,2,3,4 \
     -profile balanced -codec h264 -duration 120s -out l4-h264-balanced.json
   ```
   (`-admin` needs an SSH tunnel to the host's localhost admin port.)
   Repeat for `-codec h265`, `-codec av1`, and `-profile high`/`economy`.
4. Glass-to-glass latency (not measurable by the tool): film the client
   screen and a local keypress at 240 fps, count frames, 20 samples per step.

## Pass criteria per concurrency level

| Metric | Playable | Source |
|---|---|---|
| Received fps, every stream | ≥ 57 at 60 fps target (≥ 95 %) | bench `fps` |
| Encode time p95 | ≤ 8 ms | bench `encode p95` (agent, native helper) |
| Capture time p95 | ≤ 4 ms | bench `capture p95` |
| NVENC utilisation average | ≤ 85 % (admission limit) | bench `NVENC avg` |
| GPU memory | ≤ 22 GB of 24 GB | bench `mem` |
| Glass-to-glass | ≤ 60 ms median at < 20 ms RTT | camera |

`maxSessions` = the highest level where every row passes with the chosen
workload, recorded with the driver version and workload name.

## Tool check (not an L4 result)

The tool itself was exercised in a CPU-only container with two agents on
Xvfb and software x264 at the Economy profile (1280×720@30): 2 streams at
28.8–29.4 fps, 0.4 Mbps each, RTT ≤ 2 ms on loopback; GPU fields empty
because there is no GPU. This only shows the measurement path works.

## Results

_Pending L4 hardware._
