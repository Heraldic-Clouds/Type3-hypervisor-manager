# Staging AWS estimate, 25 October 2026

Target: account 681773521032, Oregon (us-west-2), 730 hours/month.
Approximate USD pay-as-you-go before tax and credits.

| Component | Idle monthly estimate |
|---|---:|
| Single-AZ RDS PostgreSQL db.t4g.micro | $11.68 |
| 20 GiB RDS gp3 storage | $2.30 |
| Fargate 0.5 vCPU / 1 GiB task (Guacamole and guacd) | $18.02 |
| Application Load Balancer, low usage | $17–23 |
| Public IPv4: two ALB addresses and one Fargate task | $10.95 |
| One Secrets Manager interface endpoint, one AZ | $7.30 |
| Secrets, S3, ECR, logs, Lambda, API and workflow allowance | $5–10 |
| Golden AMI snapshot and DNS allowance | $2 |
| **Estimated idle baseline** | **$75–86/month** |

RDS instance rate was confirmed through AWS Pricing API: $0.016/hour.
Other estimates use published AWS on-demand pricing; actual usage and regional
price changes affect the bill. No NAT gateway, GPU, Multi-AZ RDS or paid tiers.
The SMTP secret fits the secrets allowance. Hosting costs for the owner's
existing mail server in the separate account are outside this MEA estimate.

Two t3a.medium desktops running continuously add roughly $55/month in compute,
$7.30 in public IPv4 and $2 in EBS, so a continuously occupied two-desktop alpha
is approximately $140–151/month before significant data transfer.
Intermittent desktops cost less. A Packer build is a temporary medium instance.

Safeguards: two-desktop global cap under a database advisory lock, instance-type
allowlist, encrypted disposable 12 GiB volumes, 20-minute inactivity limit,
5-minute disconnect grace, four-hour absolute lifespan and an independent
one-minute watchdog. EC2 CPU credits use standard mode to avoid surplus charges.

Budget: $150/month, account-wide, alerts at 50% actual and 100% forecast. **Budget alerts are
not hard spending limits.** Network traffic, attacks, log volume, build retries,
and keeping the deployment running can incur additional charges. API throttling
and short log retention reduce exposure. The linked deployment account cannot
activate cost-allocation tags, so the budget includes all spending in this account
instead of relying on an inactive Project tag filter. Resources still carry cost tags.

Sources: https://aws.amazon.com/fargate/pricing/
https://aws.amazon.com/rds/postgresql/pricing/
https://aws.amazon.com/elasticloadbalancing/pricing/
https://aws.amazon.com/vpc/pricing/
https://aws.amazon.com/ec2/pricing/on-demand/
