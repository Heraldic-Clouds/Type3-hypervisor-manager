# Mise En Abyme Newbie deployment

Live URL: https://miseenaby.me

Deployment account 681773521032, profile stratus2-tantalus, us-west-2.
DNS account 434925282829, profile default, zone Z2VQV7OZ3AMOWY.
Git identity: TantalusDenton <thomasdxdenton@hotmail.com>.

## Source and toolchain

Keep Type3-hypervisor-manager, mea-web, guacamole-client and guacamole-server
as sibling directories. Backend is Type3-hypervisor-manager/mvp; frontend is
mea-web/newbie. Legacy LXD and Next.js/Stripe prototypes are retained but are
not deployed. Official Guacamole 1.6.0 images run with the supported lifecycle
extension in mvp/guacamole.

Use Docker Desktop, PowerShell 7, AWS CLI v2, Python 3, Go 1.27.3 and Node
24.10.0+. Terraform 1.17.0 and Packer 1.16.2 run in pinned Docker images.

## Local environment

From mvp:

    ./scripts/local.ps1
    ./scripts/seed-local.ps1

Open http://localhost:5173. Ignored mvp/.env contains generated local secrets.
Seed users: alice@example.test and bob@example.test; password: SEED_PASSWORD
in that file. Compose includes PostgreSQL, API, Guacamole, guacd, XFCE/XRDP and
Vite. PostgreSQL is exposed only on 127.0.0.1:55433.

The local desktop is a reusable development fixture. Restart/rebuild its
container to clear its filesystem. AWS desktops are fresh disposable instances.

    docker compose down
    docker compose down -v

The second command deletes the local database. Never seed test users in AWS.

## Deployment

The ignored infra/staging.auto.tfvars contains current deployment values.
Recreate it from infra/staging.tfvars.example if needed.

    ./scripts/deploy.ps1 -PlanOnly
    ./scripts/deploy.ps1

The script verifies the account, checks connected sessions, builds artifacts,
creates a Terraform plan, applies it, runs migrations and uploads the frontend.
Use a new Guacamole image tag for extension changes; update guac_image_tag
and use the matching deployment -ImageTag argument. Current tag: newbie-v3.

Do not change Lambda ZIPs while their Terraform plan is applying.
scripts/package.py sets executable permissions on the Linux bootstrap binary.
No generated secrets, Terraform state, build artifacts or test browser traces
are committed.

Terraform state contains generated credentials. Keep infra/terraform.tfstate
and backups private and backed up. Move it to an encrypted, versioned S3 backend
with state locking before team-operated deployment.

## Golden AMI

Current image: ami-0964ca3ff232953a2
Name: mea-ubuntu-24.04-xfce-20261025125357
Snapshot: snap-0d89a95a19d76b166

    docker build -f packer/tool.Dockerfile -t mea-packer:1.16.2 .

Run Packer init and build inside mea-packer:1.16.2, mounting the absolute packer
directory at /work, a plugin cache at /plugins, and the AWS credentials directory
read-only at /root/.aws. Working directory: /work.

    packer init desktop.pkr.hcl
    packer build -var subnet_id=SUBNET -var security_group_id=SG -var instance_profile=PROFILE desktop.pkr.hcl

Use Terraform outputs builder_subnet, builder_security_group and builder_profile.
Packer's builder uses SSM forwarding without an inbound SSH rule. It terminates
the builder and removes its temporary key after the image snapshot completes.
Save manifest.json, update desktop_ami, then redeploy the worker.

The Ubuntu 24.04 image includes XFCE, XRDP/xorgxrdp, Firefox, AWS CLI, SSM and
a root-owned idle monitor. Credentials are unique per launch, delivered through
an instance-bound Secrets Manager secret and deleted after JDBC registration.

## Authentication and email

MEA stores Argon2id hashes and HTTP-only sessions. JDBC supplies Guacamole
identities and connection permissions. React uses the official common-js client;
tokens stay in memory. See docs/SSO.md for the pinned protocol and revocation
contract.

Domain DKIM verification is complete. AWS denied SES production sending
(case 179293322500539). The account remains sandboxed. Verification and reset
flows are implemented, but arbitrary inbox delivery requires AWS approval.
The default deployment requires verification; a restricted alpha can use
individually verified SES recipient identities.

Live tests use SES simulator identities and a temporary IAM-only test helper
to obtain their test verification links. This does not verify delivery to
arbitrary inboxes. The helper, cmd/dbops, has no public route and is deleted
after validation.

## Lifecycle, networking and cost

One active desktop per Newbie user, global cap two, configurable approved types
t3a.medium / t3a.small. The one-minute watchdog reconciles provisioning jobs,
expired sessions, abandoned instances and orphaned connections. Starting and
explicitly destroying a desktop may wait up to one scheduler interval.

Defaults: warning at 15 minutes without input, termination at 20, five-minute
disconnect grace, four-hour absolute machine life. Terraform warning_seconds,
idle_seconds, grace_seconds and lifetime_seconds configure the API, browser,
Guacamole listener and launch-time instance backstop together. Local equivalents
are LOCAL_WARNING_SECONDS and LOCAL_IDLE_SECONDS in the Compose environment.

No public SSH or RDP. Only Guacamole and readiness-probe Lambdas can reach RDP.
The private Single-AZ RDS and database Lambdas have no internet route; Fargate
and desktops use public IPv4 for outbound access. There is no NAT gateway.
EBS root volumes are encrypted and deleted on termination.

Cost estimate in docs/COST.md: $75–86/month idle; $140–151/month with both
desktops occupied continuously, before substantial transfer and tax.
The $150 budget is account-wide because this linked account cannot activate
cost-allocation tags. Budget alerts are not hard spending limits.

## Tests

    go test ./...
    go vet ./...

Set TEST_DATABASE_URL to disposable local PostgreSQL for integration tests.
Fixtures truncate their local database. The tests reject non-local database
hosts. Run in golang:1.27.3 if Windows application control blocks generated
test executables.

From mea-web/newbie:

    npm ci
    npm run build
    npm audit --audit-level=high
    $env:MEA_LOCAL_E2E='1'
    npx playwright test --workers=1

To test shortened real-time expiration, set LOCAL_IDLE_SECONDS=60 and
LOCAL_WARNING_SECONDS=45, recreate the Compose api/guacamole services, and run
node scripts/timeouts-e2e.mjs from mea-web/newbie. Remove those environment
overrides and recreate the services to restore the 15/20-minute defaults.

Full AWS test: run ./scripts/test-live.ps1 from mvp. It builds, creates the
temporary helper, runs the browser test, and deletes the helper in a finally block.
To run the browser portion directly, set MEA_LIVE_CONFIRM=YES and run scripts/live-e2e.mjs.
It requires the temporary dbops helper configured with the migration Lambda's
role, VPC and secret reference. It checks two-user isolation, provisioning,
real browser input, idle expiration, EC2 termination, EBS deletion and a fresh
desktop. Test artifacts and credentials remain in ignored result directories.

## Teardown

    ./scripts/teardown.ps1

This creates a destroy plan without applying it. To remove the deployment,
database, customer profiles and optional golden image:

    ./scripts/teardown.ps1 -Execute -RemoveImage

The script terminates MEA-tagged desktops, empties only the MEA staging web
bucket, applies the destroy plan and restores the original apex/www A records
to 54.69.123.200. The hosted zone and unrelated AWS resources remain.

## Alpha limits

Single-AZ RDS and one Guacamole task limit availability. Audio and persistent
storage are disabled. ECS task capacity is locked to one: scaling out requires
a shared JSON replay cache and tested session-aware draining. Deploy the
container/backend during a maintenance window after users disconnect; stop new
connections operationally during that window. Frontend updates do not replace
a Guacamole task.

No billing, paid tiers, external identity provider, LXD or GPU streaming is
included. SES production approval remains external to the implementation.
