# Milestone 2 incremental AWS estimate, 26 October 2026

Target: account 681773521032, Oregon (us-west-2), profile stratus2-tantalus.
Approximate USD on-demand before tax and credits. Adds to the Newbie baseline
in COST.md ($75–86/month idle).

g6.4xlarge Linux on-demand rate confirmed through the AWS Pricing API:
$1.3232/hour (16 vCPU, 64 GiB, one NVIDIA L4).

## Fixed, whether the GPU host runs or not

| Component | Monthly |
|---|---:|
| GPU host EBS gp3: 60 GiB root + 250 GiB LXD pool (persists while stopped) | $24.80 |
| EBS snapshots of the LXD pool (incremental backups) | $3–5 |
| GPU host AMI snapshot (~30 GiB) | $1.50 |
| EFS Standard, ~20 GiB subscriber data, elastic throughput | $6–9 |
| CloudWatch custom GPU/host metrics, alarms, logs | $5–7 |
| Secrets Manager: Stripe key, webhook secret, Type3/LXD client identity | $1.20 |
| Lambda, Step Functions, Scheduler | <$1 |
| **Fixed subtotal** | **~$42–49** |

EFS mount targets have no hourly charge. No NAT gateway, interface endpoint,
load balancer, RDS or Fargate task is added.

## Per running hour of the GPU host

$1.3232 instance + $0.005 public IPv4 = ~$1.33/hour. Guacamole display traffic
leaves through the existing ALB at roughly $0.09/GB (about 0.5–1 GB per
session-hour at desktop quality).

| Host runtime | Compute | Total incremental (with fixed) |
|---|---:|---:|
| 2 h/day (61 h) | $81 | ~$125–135 |
| 4 h/day (122 h) | $162 | ~$205–215 |
| 8 h/day (243 h) | $323 | ~$365–375 |
| 24/7 (730 h) | $970 | ~$1,015–1,025 |

Paid tiers have no hour limit and an active Power or Eleet session keeps the
shared host running, so worst-case spend is the 24/7 row. Idle detection and
StopInstances keep it near the real usage rows.

One-off: a Packer build of the GPU AMI runs a g6.4xlarge for about one hour
(~$1.50).

## Safeguards

- Service quota "Running On-Demand G and VT instances" is currently 0 vCPU.
  Raising it to exactly 16 allows one g6.4xlarge and makes a second
  physically impossible at the AWS layer.
- IAM allows RunInstances only for g6.4xlarge with the MEA GPU tag; Start/Stop
  only on the tagged host; no TerminateInstances on the GPU host.
- Database lease plus reconciliation enforces a single host in the backend.
- Budget: account-wide; current $150 limit will be exceeded by normal GPU use.
  Proposed: raise to $400 with alerts at 50% and 80% actual, 100% forecast.
- Budget alerts are not hard spending limits.
