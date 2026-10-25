# Guacamole SSO compatibility contract

The official Guacamole 1.6.0 encrypted JSON authentication extension authenticates
the existing username. Its payload contains an empty connection set; PostgreSQL
JDBC supplies the actual connections and READ permissions. A customer never has
a Guacamole password. MEA stores Argon2id hashes and manages web sessions.

The backend checks desktop ownership and the current lease, then creates a
30-second, single-use JSON payload. The React adapter posts that payload to the
documented /guacamole/api/tokens endpoint. guacamole-common-js from the same
pinned official image renders the desktop over WebSocketTunnel.

Guacamole tokens stay in a JavaScript closure, never localStorage. The signing
key and desktop password never reach browser JavaScript. The API returns only
the current user's authorized connection identifier. Arbitrary IDs are still
checked by JDBC permissions and the MEA lifecycle extension.

The WebSocket application's request parameters are an integration contract
specific to 1.6.0, not a claim of a permanent REST API. Test tokens, wrong-owner
connection IDs, replay rejection, WebSocket handshake, rendering and revocation
before upgrading either Guacamole image. Both images are version/digest pinned.

The narrowly scoped lifecycle listener uses the supported Java extension event
API to bind tunnels to web sessions and close them after revocation, idle expiry,
desktop teardown or loss of database authorization. It does not replace JDBC
authentication, permissions, table definitions or password hashing.

Only token exchange, tunnels and guacamole-common-js are routed publicly through
the ALB; the upstream login and administration UI are not published.

For alpha, Guacamole is restricted to one task. Its single-use JSON replay cache
is process-local. Scaling out requires shared replay enforcement; routine ECS
deployment can interrupt active connections. Drain during a maintenance window.
