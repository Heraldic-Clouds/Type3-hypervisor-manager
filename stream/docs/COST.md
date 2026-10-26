# Gaming Mode cost model

Prices are AWS on-demand list prices for us-west-2 as known at the time of
writing; confirm with the Pricing API before relying on them.

| Item | Price | Note |
|---|---|---|
| g6.4xlarge (1× L4) | ≈ $1.32 / hour | already the M2 Eleet host; Gaming Mode adds no instance |
| Internet egress | $0.09 / GB (first 10 TB/month) | dominant per-stream cost |
| Secrets Manager (stream key) | $0.40 / month | only when `enable_stream = true` |
| Route53 record | $0 (existing zone) | |
| coturn on the GPU host | $0 extra | runs only while the host runs; relay egress is billed as above |

Video egress per stream-hour (bitrate × 3600 s):

| Profile | Typical bitrate | GB / hour | Egress $ / hour |
|---|---|---|---|
| Economy 720p30 | 3–6 Mbps | 1.4–2.7 | $0.12–0.24 |
| Balanced 1080p60 | 8–15 Mbps | 3.6–6.8 | $0.32–0.61 |
| High 1080p60 | 12–25 Mbps | 5.4–11.3 | $0.49–1.01 |

The encoder drops unchanged frames (5 fps keep-alive), so menus and idle
desktops cost far less than the table.

Cost per user-hour ≈ host $/h ÷ concurrent streams + egress $/h:

| Concurrent streams | Balanced @ 10 Mbps | Economy @ 4 Mbps |
|---|---|---|
| 1 | $1.32 + $0.41 = **$1.73** | $1.32 + $0.16 = **$1.48** |
| 2 | $0.66 + $0.41 = **$1.07** | $0.82 |
| 4 (if the benchmark allows) | $0.33 + $0.41 = **$0.74** | $0.49 |

At $50/month, an Eleet user breaks even at about 29 hours/month at 1
stream per host, or 68 hours at 4 per host (Balanced). Levers, in order:
host concurrency (benchmark), the default profile (Balanced capped at
10 Mbps is the recommended default), and the existing idle shutdown.
