-- Milestone 2: Power/Eleet private beta, administrators, Stripe billing,
-- persistent LXD desktops and the single GPU host.

CREATE TABLE IF NOT EXISTS mea.user_roles (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id),
 role text NOT NULL CHECK (role IN ('admin')),
 reason text NOT NULL, granted_by uuid REFERENCES mea.users(id), granted_at timestamptz NOT NULL DEFAULT now(),
 revoked_by uuid REFERENCES mea.users(id), revoked_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_role ON mea.user_roles(user_id, role) WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS mea.stripe_customers (
 user_id uuid PRIMARY KEY REFERENCES mea.users(id), customer_id text NOT NULL UNIQUE,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mea.subscriptions (
 id text PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id), customer_id text NOT NULL,
 tier text NOT NULL CHECK (tier IN ('power','eleet')), price_id text NOT NULL, status text NOT NULL,
 current_period_end timestamptz, cancel_at_period_end boolean NOT NULL DEFAULT false,
 latest_invoice_status text, stripe_updated bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS subscriptions_user ON mea.subscriptions(user_id);
CREATE TABLE IF NOT EXISTS mea.stripe_events (
 id text PRIMARY KEY, type text NOT NULL, created bigint NOT NULL,
 received_at timestamptz NOT NULL DEFAULT now(), processed_at timestamptz, error text
);

-- entitlement_source distinguishes paid access from free administrator access.
-- Billing synchronization only ever changes rows whose source is 'stripe'.
CREATE TABLE IF NOT EXISTS mea.entitlements (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id),
 tier text NOT NULL CHECK (tier IN ('power','eleet')),
 source text NOT NULL CHECK (source IN ('stripe','admin_grant')),
 subscription_id text REFERENCES mea.subscriptions(id),
 status text NOT NULL CHECK (status IN ('active','suspended','revoked')),
 reason text NOT NULL DEFAULT '', granted_by uuid REFERENCES mea.users(id),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((source='stripe') = (subscription_id IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS one_live_entitlement ON mea.entitlements(user_id, source, tier) WHERE status<>'revoked';

CREATE TABLE IF NOT EXISTS mea.invitations (
 id uuid PRIMARY KEY, code_hash text NOT NULL UNIQUE, hint text NOT NULL,
 tiers text[] NOT NULL CHECK (cardinality(tiers) > 0 AND tiers <@ ARRAY['power','eleet']::text[]),
 max_redemptions integer NOT NULL CHECK (max_redemptions BETWEEN 1 AND 10000),
 redemptions integer NOT NULL DEFAULT 0 CHECK (redemptions >= 0 AND redemptions <= max_redemptions),
 email text, note text NOT NULL DEFAULT '',
 expires_at timestamptz NOT NULL, created_by uuid NOT NULL REFERENCES mea.users(id), created_at timestamptz NOT NULL DEFAULT now(),
 revoked_by uuid REFERENCES mea.users(id), revoked_at timestamptz
);
CREATE TABLE IF NOT EXISTS mea.invitation_reservations (
 id uuid PRIMARY KEY, invitation_id uuid NOT NULL REFERENCES mea.invitations(id), user_id uuid NOT NULL REFERENCES mea.users(id),
 tier text NOT NULL CHECK (tier IN ('power','eleet')),
 state text NOT NULL CHECK (state IN ('reserved','redeemed','released')),
 checkout_session_id text UNIQUE, expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), redeemed_at timestamptz, released_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS one_reservation_per_user ON mea.invitation_reservations(user_id) WHERE state='reserved';
CREATE INDEX IF NOT EXISTS reservations_invitation ON mea.invitation_reservations(invitation_id, state);

-- The staging slot check is the database half of the one-GPU-host limit.
CREATE TABLE IF NOT EXISTS mea.gpu_hosts (
 slot integer PRIMARY KEY CHECK (slot = 1),
 instance_id text UNIQUE, state text NOT NULL DEFAULT 'absent'
  CHECK (state IN ('absent','launching','pending','running','stopping','stopped','failed')),
 desired text NOT NULL DEFAULT 'stopped' CHECK (desired IN ('running','stopped')),
 private_ip text, ready boolean NOT NULL DEFAULT false,
 lease_owner text, lease_until timestamptz,
 started_at timestamptz, last_busy_at timestamptz, last_transition timestamptz NOT NULL DEFAULT now(),
 running_seconds bigint NOT NULL DEFAULT 0, metrics jsonb NOT NULL DEFAULT '{}'::jsonb, error text,
 fingerprint text, hold_until timestamptz, launches integer NOT NULL DEFAULT 0,
 metrics_at timestamptz
);
INSERT INTO mea.gpu_hosts(slot) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS mea.containers (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id),
 tier text NOT NULL CHECK (tier IN ('power','eleet')), name text NOT NULL UNIQUE,
 state text NOT NULL CHECK (state IN ('requested','creating','stopped','starting','running','stopping','failed','deleting','deleted')),
 desired text NOT NULL DEFAULT 'running' CHECK (desired IN ('running','stopped','deleted')),
 type3_id text UNIQUE, operation_id text, host_slot integer REFERENCES mea.gpu_hosts(slot),
 private_ip text, rdp_port integer UNIQUE, password_secret text,
 cpu integer NOT NULL, memory_mib integer NOT NULL, disk_gib integer NOT NULL, gpu boolean NOT NULL,
 storage_bytes bigint, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now(), error text,
 CHECK (gpu = (tier='eleet'))
);
CREATE UNIQUE INDEX IF NOT EXISTS one_container_per_tier ON mea.containers(user_id, tier) WHERE state<>'deleted';

ALTER TABLE mea.desktops ADD COLUMN IF NOT EXISTS tier text NOT NULL DEFAULT 'newbie';
ALTER TABLE mea.desktops ADD COLUMN IF NOT EXISTS container_id uuid REFERENCES mea.containers(id);
ALTER TABLE mea.desktops ADD COLUMN IF NOT EXISTS keep_alive_until timestamptz;
DO $$ BEGIN
 ALTER TABLE mea.desktops ADD CONSTRAINT desktops_tier CHECK (tier IN ('newbie','power','eleet'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;
ALTER TABLE mea.desktops DROP CONSTRAINT IF EXISTS desktops_state_check;
ALTER TABLE mea.desktops ADD CONSTRAINT desktops_state_check CHECK (state IN ('waiting','queued','provisioning','ready','terminating','terminated','failed'));
DROP INDEX IF EXISTS mea.one_active_desktop;
CREATE UNIQUE INDEX IF NOT EXISTS one_active_desktop ON mea.desktops(user_id) WHERE state IN ('waiting','queued','provisioning','ready','terminating');

-- Step-up: destructive administrator actions need a password check within 10 minutes.
ALTER TABLE mea.sessions ADD COLUMN IF NOT EXISTS reauth_at timestamptz;

CREATE TABLE IF NOT EXISTS mea.admin_audit (
 id bigserial PRIMARY KEY, actor_id uuid, action text NOT NULL, target text NOT NULL DEFAULT '',
 details jsonb NOT NULL DEFAULT '{}'::jsonb, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_audit_time ON mea.admin_audit(created_at DESC);
