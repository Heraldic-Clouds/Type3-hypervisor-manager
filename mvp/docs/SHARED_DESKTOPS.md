# Shared desktops

A shared desktop is a long-lived machine that any verified Newbie user can
borrow, one user at a time. The first one is **XP25-AI**: a Windows XP 25th
Anniversary Edition VM with ClippyAI and RoverAI, running on a home KVM host
and reached through a private relay. It appears on the dashboard under
"Shared machines".

## How a lease works

- `POST /api/desktops {"kind": "xp25-ai"}` creates an ordinary `mea.desktops`
  row with `kind='xp25-ai'`, `tier='newbie'` and state `ready`. There is nothing
  to provision; the lease grants Guacamole `READ` on the shared connection.
- The unchanged Guacamole lifecycle listener applies the usual checks to the
  tunnel: MEA web session, desktop session, heartbeat, idle limit and lifetime.
  Connect, activity and disconnect use the existing endpoints.
- One seat: the partial unique index `one_shared_seat` allows one active lease
  per kind. A second user gets 409 "XP25-AI is in use by another Newbie".
- The one-desktop-per-user rule still holds: a user with an Ubuntu or paid
  desktop must end it before borrowing, and the reverse.
- Shared leases do not count toward the EC2 desktop cap.
- The lease ends on "End session", idle expiry, the disconnect grace period,
  the lifetime limit, account deletion, or when the shared desktop is disabled.
  Reconciliation revokes the permission, closes sessions and finishes the lease
  in the same transaction (`state='terminated'`, `connection_id=NULL`); the EC2
  worker never sees it. The machine and its Guacamole connection stay in place.
  The `terminated` action never deletes a shared connection, and orphan cleanup
  only touches connections named `MEA ...`.
- The machine is shared: whatever one user leaves on screen, the next one sees.
  The dashboard and the remote view say so.

## Registering a shared desktop

Operators use the control function directly (IAM only; there is no public
route). The call creates or updates the Guacamole connection and the
`mea.shared_desktops` row:

```json
{"action": "shared-desktop", "kind": "xp25-ai", "name": "XP25-AI",
 "description": "Windows XP 25th Anniversary Edition with ClippyAI and RoverAI. The machine is offline: it has no network connection.",
 "protocol": "vnc",
 "parameters": {"hostname": "RELAY_PRIVATE_IP", "port": "5900", "password": "VNC_PASSWORD", "color-depth": "24"}}
```

```sh
aws lambda invoke --function-name mea-staging-control --payload fileb://register.json result.json
```

Write the payload with mode 0600 and delete it afterwards: it holds the VNC
password. Validation refuses the `ubuntu` kind, names starting with `MEA `,
protocols other than `vnc` and `rdp`, and connection parameters outside a
short allowlist (no SFTP, drive redirection or recording). Send the same call
with `"enabled": false` to hide the desktop and end the current lease at the
next reconciliation.

## XP25-AI path and its AWS resources

```text
browser -> CloudFront/ALB (existing routes) -> Guacamole/guacd (Fargate)
  -> relay EC2 private address :5900
  -> SSH reverse tunnel, carried over SSM Session Manager
  -> home KVM host, QEMU VNC on loopback -> XP25-AI (no network adapter)
```

The relay resources are outside this Terraform configuration and are managed
from the winxp_2026 repository's `remote-access/` directory:

| Resource | Name | Notes |
| --- | --- | --- |
| EC2 instance | `mea-staging-xp25-relay` | t4g.nano Amazon Linux 2023 in the desktop subnet; IMDSv2 only, hop limit 1; encrypted root; termination protection. Not tagged `MEAEnvironment`, so the worker's instance sweep ignores it. |
| Security group | `mea-staging-xp25-relay` | Inbound TCP 5900 from the Guacamole task's group only; outbound HTTPS only (SSM and security updates). |
| Role and instance profile | `mea-staging-xp25-relay` | `AmazonSSMManagedInstanceCore` only. |
| IAM user | `mea-staging-xp25-tunnel` | Used by the home host. `ssm:StartSession` with `AWS-StartSSHSession` on that one instance, and ending its own sessions. |

On the relay, sshd listens on loopback, and the only account may open exactly
one remote forward on the relay's private address and nothing else (no shell,
TTY or local forwarding). No public SSH, VNC, RDP or guacd port exists, and
no existing security-group rule was changed.

## Deployment record (2026-10-26)

- Built on `feature/m2-power-eleet-beta` (`f1cebd1` backend, `93f8cfa`
  frontend). Before deploying, rebuilds of those commits matched the live
  `api` bootstrap and frontend bundle byte for byte, and no desktop session
  was connected.
- Backend `9b72a9f`: `aws lambda update-function-code` of `api.zip` for
  `migrate` (invoked: `{"ok": true}`, applying `003_shared_desktops.sql`),
  `control` and `api`. The worker and the Guacamole image were not changed.
- Frontend `073b777`: `s3 sync` of `dist`, then `index.html`, then a
  CloudFront invalidation, as in `deploy.ps1`.
- XP25-AI registered as Guacamole connection 11.
- `shared-e2e.mjs` (mea-web) passed against the live site; its first run failed
  on two test mistakes and is kept with its evidence.
- Ubuntu regression: the unmodified `live-e2e.mjs` failed after this deploy, and
  equally with page loads served the previous frontend (`index-3yfRWN7m.js`).
  In both runs:
  - the EC2 desktop was provisioned with its permission, guacd's RDP session
    connected and the XFCE session started;
  - the browser stayed on "Starting desktop".

  A diagnostic connection showed why. The XFCE desktop renders fully (panel,
  icons, dock; 238 sampled colours after 45 s), but with wallpaper disabled over
  RDP its background is never drawn. The frontend's 35-point paint check ignores
  transparent pixels, never sees more than two colours, and leaves the "Starting
  your Ubuntu session" overlay over a working desktop.

  This is independent of the shared desktop change: the overlay logic is
  unchanged, and the backend is not in the RDP path. The test desktops were
  terminated by the watchdog and the test accounts deleted. A fix would treat
  any painted frame after the tunnel connects as connected, or count
  transparent pixels as a colour.

**Terraform state.** The Lambda packages were updated with the AWS CLI, so the
Terraform state on the release machine still records the previous package
hash. Merge this branch before the next `deploy.ps1`. Deploying a branch
without it removes the feature. Worse, older code's `terminated` action would
delete the shared connection when an active XP25-AI lease ends; recreate it
with the registration call above.

## Tests

`internal/api/shared_test.go` (real PostgreSQL) covers:
- registration validation and idempotency;
- the single seat and independence from the EC2 cap;
- cross-user isolation;
- release on end, idle and disable;
- preservation of the shared connection against late `terminated` and `orphans` calls;
- the lifecycle listener's two SQL statements, run verbatim against a shared lease.

The migration was also applied on top of a database migrated by the
previous release (`001`, `002` applied, `003` added once).
