CREATE SCHEMA IF NOT EXISTS mea;
CREATE TABLE IF NOT EXISTS mea.users (
 id uuid PRIMARY KEY, guac_user_id integer NOT NULL UNIQUE REFERENCES guacamole_user(user_id),
 entity_id integer NOT NULL UNIQUE REFERENCES guacamole_entity(entity_id),
 username text NOT NULL UNIQUE, email text NOT NULL UNIQUE, name text NOT NULL,
 password_hash text NOT NULL, subscription text NOT NULL DEFAULT 'Newbie',
 verified boolean NOT NULL DEFAULT false, disabled boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mea.sessions (
 hash text PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id), csrf text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 revoked_at timestamptz, user_agent text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_user ON mea.sessions(user_id);
CREATE TABLE IF NOT EXISTS mea.tokens (
 hash text PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id), kind text NOT NULL,
 expires_at timestamptz NOT NULL, used_at timestamptz
);
CREATE TABLE IF NOT EXISTS mea.rate_limits (
 key text PRIMARY KEY, hits integer NOT NULL, reset_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS mea.desktops (
 id uuid PRIMARY KEY, user_id uuid NOT NULL REFERENCES mea.users(id),
 state text NOT NULL CHECK (state IN ('queued','provisioning','ready','terminating','terminated','failed')),
 instance_id text UNIQUE, private_ip text, connection_id integer UNIQUE REFERENCES guacamole_connection(connection_id) ON DELETE SET NULL,
 operation_id uuid NOT NULL UNIQUE, execution_arn text,
 last_activity timestamptz NOT NULL DEFAULT now(), last_heartbeat timestamptz,
 disconnect_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), ready_at timestamptz,
 expires_at timestamptz NOT NULL, terminated_at timestamptz, error text
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_desktop ON mea.desktops(user_id) WHERE state IN ('queued','provisioning','ready','terminating');
CREATE TABLE IF NOT EXISTS mea.desktop_sessions (
 id uuid PRIMARY KEY, desktop_id uuid NOT NULL REFERENCES mea.desktops(id),
 web_session_hash text NOT NULL REFERENCES mea.sessions(hash),
 tunnel_id text UNIQUE, connected_at timestamptz, closed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mea.outbox (
 id uuid PRIMARY KEY, kind text NOT NULL, payload jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), sent_at timestamptz, attempts integer NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS mea.audit (
 id bigserial PRIMARY KEY, user_id uuid, desktop_id uuid, action text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now()
);
