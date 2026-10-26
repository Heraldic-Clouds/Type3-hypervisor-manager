-- Shared desktops are long-lived machines that Newbie users borrow one at a
-- time, such as the home-hosted XP25-AI. A lease is an ordinary mea.desktops
-- row with a non-'ubuntu' kind, so the Guacamole lifecycle listener applies the
-- same session, heartbeat, idle and lifetime checks to it.
ALTER TABLE mea.desktops ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'ubuntu';
CREATE TABLE IF NOT EXISTS mea.shared_desktops (
 kind text PRIMARY KEY CHECK (kind ~ '^[a-z][a-z0-9-]{1,31}$' AND kind <> 'ubuntu'),
 name text NOT NULL,
 description text NOT NULL DEFAULT '',
 connection_id integer NOT NULL UNIQUE REFERENCES guacamole_connection(connection_id) ON DELETE CASCADE,
 enabled boolean NOT NULL DEFAULT true,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS one_shared_seat ON mea.desktops(kind) WHERE kind <> 'ubuntu' AND state IN ('waiting','queued','provisioning','ready','terminating');
