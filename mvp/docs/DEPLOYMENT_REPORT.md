# Deployment verification — 25 October 2026

Live site: https://miseenaby.me
CloudFront: d3no1q2t1zez5w.cloudfront.net (E2TESIQ92T6R7X)
API Gateway: https://07no9ratk9.execute-api.us-west-2.amazonaws.com
Guacamole origin: desktop.miseenaby.me (only tokens/tunnels/library are routed)

AWS account 681773521032, Oregon, profile stratus2-tantalus.
Domain/DNS account 434925282829, profile default.
Golden image: ami-0964ca3ff232953a2.

## Performed against real AWS

The final automated browser run passed at 2026-10-25 14:21:22 UTC.
Complete timestamped evidence: verification/live-aws.json.

- Registered two separate accounts through the deployed React website.
- Verified transactional MEA profiles and corresponding Guacamole JDBC user IDs.
- Exercised verification links using SES simulator test identities and an IAM-only
  deployment helper. Arbitrary email delivery was not verified.
- Confirmed concurrent create requests returned one desktop UUID.
- Confirmed User B received 404 for User A's connect, activity, disconnect and
  destroy endpoints.
- Provisioned EC2 i-0af9107bbaa82482c from the golden AMI; one JDBC READ permission.
- Displayed a real Ubuntu XFCE desktop through Guacamole WebSocket inside MEA.
- Sent mouse and keyboard events; terminal input created
  /home/mea/mea-browser-proof.txt. SSM read it back as MEA_BROWSER_INPUT_OK.
- Resized the browser desktop and captured the result.
- Disconnected, advanced the test desktop's server idle timestamp, and allowed
  the normal watchdog to reconcile it.
- Verified EC2 termination and deletion of EBS vol-02a8177e43f7de0d1.
- Provisioned a fresh desktop c09468bc-31d7-419b-a2a8-887e4459e154 on a different
  instance i-00f73c2a52ed9fb33, rendered it, and connected successfully.
- Deactivated test accounts and verified no running/shutting-down desktops,
  no remaining desktop EBS volumes and no desktop credential secrets.
- Deleted the temporary verification-helper Lambda.

The first XFCE session can take 30–60 seconds after the RDP transport connects.
MEA now shows a startup indicator until desktop pixels arrive. The final fresh
desktop test waited for actual canvas image content rather than transport alone.

Screenshots: verification/ubuntu-desktop.png and verification/fresh-desktop.png.

## Local verification

- Go unit tests passed for Argon2id, Guacamole encrypted wire format, failed
  launches, revoked leases, credential bootstrap, idempotent ready retries and
  cleanup waiting for actual instance removal.
- Real PostgreSQL integration tests passed for identity mapping, concurrent
  provisioning, two-user authorization, CSRF, global quota, idle expiration,
  fresh leases, expired sessions, logout revocation, password reset, reset-token
  replay rejection and fail-closed behavior when the database pool is unavailable.
- Docker Compose's real XFCE/XRDP browser workflow passed.
- Desktop and mobile landing-page browser checks passed.
- React TypeScript/Vite build passed; npm audit reported zero vulnerabilities.
- Terraform and Packer configurations validated.
- PowerShell deployment/teardown scripts parsed successfully.
- A separate local run with real 45-second warning and 60-second idle values
  verified that API policy, browser warning/expiry and Guacamole's interval query
  follow configuration. See verification/timeout-policy.json. Docker frontend
  file polling is enabled for reliable updates on Windows bind mounts.
- SMTP protocol tests passed with the race detector: STARTTLS and implicit TLS,
  untrusted certificates, missing STARTTLS, rejected recipients, header injection,
  cancellation and outbox acknowledgement only after server acceptance.

See verification/go-tests.txt for the saved Go results. Both GitHub Actions
workflows also passed: backend run 40199358243 and frontend run 40199643505.
The branch's Actions pages show subsequent commits' current CI status.

## Cost and remaining limits

Estimated idle baseline $75–86/month, approximately $140–151/month with both
capped desktops occupied continuously, before substantial traffic and tax.
Two-desktop limit and $150 account budget are configured. Budget alerts are not
hard spending limits. See COST.md.

The live worker now uses blackmail.heraldic.cloud:587 with STARTTLS, sending from
Mise En Abyme <mea@blackmail.heraldic.cloud>. The password was imported from the
owner's private env file into Secrets Manager without displaying or committing
it. Only the worker can read the exact SMTP secret. Mail credentials are read
per attempt so rotation does not need redeployment. Terraform applied one
worker update and one scoped IAM policy; no other service was replaced.

The real email acceptance run began at 2026-10-25 17:35:29 UTC. It registered
a unique plus alias in the owner's mailbox, confirmed an unverified account
could not launch a desktop, received the verification email through SMTP/IMAP,
used its link and rejected replay. It then received the password-reset email,
revoked the old session, rejected reset-token replay and the old password, and
logged in with the replacement password. Both messages contained DKIM signatures.
The test account was deactivated; no helper or database token read was used.
Evidence: verification/smtp-live.json. See SMTP.md for the repeatable test.

AWS denied SES production sending (case 179293322500539). SMTP is selected, so
that restriction no longer blocks signup or reset. Verification remains required.
Delivery was verified to the owner's mail server; Gmail/Outlook inbox placement
was not tested. No public test credentials were left behind.

Single-AZ database, one Guacamole task, no persistent desktop storage or audio.
Scaling out and concurrent container rollout require shared replay protection
and stronger draining coordination. Explicit destroy/provision actions may wait
up to one one-minute watchdog interval. Teardown is provided as a reviewed plan
and script; the live service was intentionally not destroyed.
