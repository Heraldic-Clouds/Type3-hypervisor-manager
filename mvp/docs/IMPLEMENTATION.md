# Mise En Abyme Newbie MVP implementation

Requested architecture: React/Vite, Go Lambda, PostgreSQL with official Guacamole
tables in public and MEA tables in mea, ECS Fargate Guacamole/guacd, Step Functions,
EC2 Ubuntu XFCE golden AMI, Terraform and Packer.

Legacy Type3 and mea-web source remain available in Git and in their original
locations. New control plane is an isolated Go module in mvp; Vite app lives in
mea-web/newbie. No LXD, GPU, paid tiers, external identity provider or Stripe.

Deployment account: 681773521032, profile stratus2-tantalus, us-west-2.
DNS account: 434925282829, default profile, zone Z2VQV7OZ3AMOWY.
Original apex and www A records: 54.69.123.200, TTL 300.

Network design avoids NAT gateways: private database and database Lambdas;
non-VPC worker invokes database Lambda, starts Step Functions, manages EC2/SES;
database outbox decouples API transactions from AWS control-plane requests.
Fargate and desktops have internet egress via public IPv4 with security groups
restricting inbound traffic. Only Fargate can reach desktop RDP. No SSH ingress.

SES sandbox is enabled with no verified identities at initial inspection.
Public self-service email delivery needs SES production access. Verification and
reset requests are queued transactionally and fail closed if email delivery fails.
