package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/billing"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/jackc/pgx/v5"
	"net"
	"strconv"
	"time"
)

type Request struct {
	Action     string `json:"action"`
	ID         string `json:"id,omitempty"`
	InstanceID string `json:"instanceId,omitempty"`
	IP         string `json:"ip,omitempty"`
	Password   string `json:"password,omitempty"`
	ARN        string `json:"arn,omitempty"`
	Error      string `json:"error,omitempty"`
	OutboxID   string `json:"outboxId,omitempty"`
	// Billing carries a database operation for the billing function, which
	// has internet access for Stripe but no route to the private database.
	Billing *billing.Call `json:"billing,omitempty"`
}
type Job struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	InstanceID  *string   `json:"instanceId"`
	OperationID string    `json:"operationId"`
	CreatedAt   time.Time `json:"createdAt"`
}
type Mail struct {
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload"`
}
type Result struct {
	Jobs  []Job  `json:"jobs,omitempty"`
	Mails []Mail `json:"mails,omitempty"`
	Ready bool   `json:"ready"`
	Skip  bool   `json:"skip"`
	Count int    `json:"count"`
	Billing *billing.Reply `json:"billing,omitempty"`
}
type Service struct {
	Store                     *store.Store
	IdleSeconds, GraceSeconds int
	Local                     bool
}

func (s *Service) Handle(ctx context.Context, r Request) (Result, error) {
	result := Result{}
	db := s.Store.DB
	switch r.Action {
	case "billing":
		if r.Billing == nil {
			return result, errors.New("billing call required")
		}
		reply, e := billing.Dispatch(ctx, billing.DB{Store: s.Store}, *r.Billing)
		result.Billing = &reply
		return result, e
	case "active-sessions":
		e := db.QueryRow(ctx, `SELECT count(*) FROM mea.desktop_sessions WHERE closed_at IS NULL AND connected_at IS NOT NULL`).Scan(&result.Count)
		return result, e
	case "reconcile":
		tx, e := db.Begin(ctx)
		if e != nil {
			return result, e
		}
		defer tx.Rollback(ctx)
		// Revoke web-session-bound tunnels, stale browser connections, and expired leases.
		_, e = tx.Exec(ctx, `UPDATE mea.desktop_sessions ds SET closed_at=now() WHERE closed_at IS NULL AND (
    NOT EXISTS(SELECT 1 FROM mea.sessions s WHERE s.hash=ds.web_session_hash AND s.revoked_at IS NULL AND s.expires_at>now())
    OR EXISTS(SELECT 1 FROM mea.desktops d WHERE d.id=ds.desktop_id AND d.last_heartbeat<now()-interval '90 seconds')
    OR (connected_at IS NULL AND created_at<now()-interval '40 seconds'))`)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `UPDATE mea.desktops d SET disconnect_at=COALESCE(disconnect_at,now()) WHERE state='ready' AND NOT EXISTS(SELECT 1 FROM mea.desktop_sessions ds WHERE ds.desktop_id=d.id AND ds.closed_at IS NULL)`)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `UPDATE mea.desktops SET state='terminating' WHERE state IN ('queued','provisioning','ready') AND (
    expires_at<now() OR (state IN ('queued','provisioning') AND created_at<now()-interval '15 minutes')
    OR (state='ready' AND last_activity<now()-$1::interval)
    OR (state='ready' AND disconnect_at<now()-$2::interval))`, fmt.Sprintf("%d seconds", s.IdleSeconds), fmt.Sprintf("%d seconds", s.GraceSeconds))
		if e != nil {
			return result, e
		}
		// Removing permissions makes new JDBC connection attempts fail immediately.
		_, e = tx.Exec(ctx, `DELETE FROM guacamole_connection_permission WHERE connection_id IN (SELECT connection_id FROM mea.desktops WHERE state='terminating')`)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `UPDATE mea.desktop_sessions SET closed_at=COALESCE(closed_at,now()) WHERE desktop_id IN (SELECT id FROM mea.desktops WHERE state='terminating')`)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `DELETE FROM mea.rate_limits WHERE reset_at<now()-interval '1 day'; DELETE FROM mea.tokens WHERE expires_at<now()-interval '1 day'`)
		if e != nil {
			return result, e
		}
		// Abandoned checkouts return their invitation slot.
		if e = beta.ReleaseExpired(ctx, tx); e != nil {
			return result, e
		}
		if e = tx.Commit(ctx); e != nil {
			return result, e
		}
		rows, e := db.Query(ctx, `SELECT id::text,state,instance_id,operation_id::text,created_at FROM mea.desktops WHERE state IN ('queued','provisioning','terminating') ORDER BY created_at LIMIT 10`)
		if e != nil {
			return result, e
		}
		for rows.Next() {
			var j Job
			if e = rows.Scan(&j.ID, &j.State, &j.InstanceID, &j.OperationID, &j.CreatedAt); e != nil {
				rows.Close()
				return result, e
			}
			result.Jobs = append(result.Jobs, j)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return result, e
		}
		rows, e = db.Query(ctx, `SELECT id::text,payload FROM mea.outbox WHERE sent_at IS NULL AND attempts<5 ORDER BY created_at LIMIT 10`)
		if e != nil {
			return result, e
		}
		defer rows.Close()
		for rows.Next() {
			var m Mail
			if e = rows.Scan(&m.ID, &m.Payload); e != nil {
				return result, e
			}
			result.Mails = append(result.Mails, m)
		}
		if e = rows.Err(); e != nil {
			return result, e
		}
		e = db.QueryRow(ctx, `SELECT count(*) FROM mea.desktops WHERE state IN ('queued','provisioning','ready','terminating')`).Scan(&result.Count)
		return result, e
	case "claim":
		tag, e := db.Exec(ctx, `UPDATE mea.desktops SET state='provisioning',execution_arn=$2 WHERE id=$1 AND state='queued'`, r.ID, r.ARN)
		result.Skip = tag.RowsAffected() == 0
		return result, e
	case "lookup":
		var state string
		e := db.QueryRow(ctx, `SELECT state FROM mea.desktops WHERE id=$1`, r.ID).Scan(&state)
		if errors.Is(e, pgx.ErrNoRows) {
			result.Skip = true
			return result, nil
		}
		result.Skip = state != "provisioning" && state != "ready"
		result.Ready = state == "ready"
		return result, e
	case "assign":
		tag, e := db.Exec(ctx, `UPDATE mea.desktops SET instance_id=$2,private_ip=$3 WHERE id=$1 AND state='provisioning'`, r.ID, r.InstanceID, r.IP)
		result.Skip = tag.RowsAffected() != 1
		return result, e
	case "ready":
		d, e := store.ScanDesktop(db.QueryRow(ctx, "SELECT "+store.DesktopColumns+" FROM mea.desktops WHERE id=$1", r.ID))
		if e != nil {
			return result, e
		}
		if d.State != "provisioning" {
			result.Skip = true
			return result, nil
		}
		conn, e := net.DialTimeout("tcp", net.JoinHostPort(r.IP, "3389"), 2*time.Second)
		if e != nil {
			return result, nil
		}
		conn.Close()
		// Verify an RDP negotiation response, not only an accepting TCP port.
		conn, e = net.DialTimeout("tcp", net.JoinHostPort(r.IP, "3389"), 2*time.Second)
		if e != nil {
			return result, nil
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, e = conn.Write([]byte{3, 0, 0, 19, 14, 224, 0, 0, 0, 0, 0, 1, 0, 8, 0, 3, 0, 0, 0})
		if e != nil {
			return result, nil
		}
		buf := make([]byte, 256)
		n, e := conn.Read(buf)
		if e != nil || n < 11 || buf[0] != 3 || buf[5] != 0xd0 {
			return result, nil
		}
		tx, e := db.Begin(ctx)
		if e != nil {
			return result, e
		}
		defer tx.Rollback(ctx)
		var state string
		if e = tx.QueryRow(ctx, `SELECT state FROM mea.desktops WHERE id=$1 FOR UPDATE`, r.ID).Scan(&state); e != nil {
			return result, e
		}
		if state != "provisioning" {
			result.Skip = true
			return result, nil
		}
		var connection int
		e = tx.QueryRow(ctx, `INSERT INTO guacamole_connection(connection_name,protocol,max_connections,max_connections_per_user) VALUES($1,'rdp',1,1) RETURNING connection_id`, "MEA "+r.ID).Scan(&connection)
		if e != nil {
			return result, e
		}
		for k, v := range map[string]string{"hostname": r.IP, "port": "3389", "username": "mea", "password": r.Password, "security": "tls", "ignore-cert": "true", "resize-method": "display-update", "enable-wallpaper": "false", "enable-font-smoothing": "false", "disable-audio": "true"} {
			if _, e = tx.Exec(ctx, `INSERT INTO guacamole_connection_parameter(connection_id,parameter_name,parameter_value) VALUES($1,$2,$3)`, connection, k, v); e != nil {
				return result, e
			}
		}
		_, e = tx.Exec(ctx, `INSERT INTO guacamole_connection_permission(entity_id,connection_id,permission) SELECT entity_id,$2,'READ' FROM mea.users WHERE id=$1 AND NOT disabled`, d.UserID, connection)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `UPDATE mea.desktops SET state='ready',connection_id=$2,ready_at=now(),last_activity=now(),disconnect_at=now() WHERE id=$1`, r.ID, connection)
		if e != nil {
			return result, e
		}
		if e = tx.Commit(ctx); e != nil {
			return result, e
		}
		result.Ready = true
		return result, nil
	case "terminating":
		_, e := db.Exec(ctx, `UPDATE mea.desktops SET state='terminating',error=NULLIF($2,'') WHERE id=$1 AND state NOT IN ('terminated','failed')`, r.ID, r.Error)
		return result, e
	case "terminated":
		tx, e := db.Begin(ctx)
		if e != nil {
			return result, e
		}
		defer tx.Rollback(ctx)
		_, e = tx.Exec(ctx, `DELETE FROM guacamole_connection WHERE connection_id=(SELECT connection_id FROM mea.desktops WHERE id=$1)`, r.ID)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `UPDATE mea.desktop_sessions SET closed_at=COALESCE(closed_at,now()) WHERE desktop_id=$1`, r.ID)
		if e != nil {
			return result, e
		}
		_, e = tx.Exec(ctx, `UPDATE mea.desktops SET state=CASE WHEN error IS NULL THEN 'terminated' ELSE 'failed' END,terminated_at=now() WHERE id=$1`, r.ID)
		if e != nil {
			return result, e
		}
		return result, tx.Commit(ctx)
	case "mail-sent":
		_, e := db.Exec(ctx, `UPDATE mea.outbox SET sent_at=now(),payload='{}'::jsonb WHERE id=$1`, r.OutboxID)
		return result, e
	case "mail-failed":
		_, e := db.Exec(ctx, `UPDATE mea.outbox SET attempts=attempts+1 WHERE id=$1`, r.OutboxID)
		return result, e
	case "orphans":
		// Only MEA-owned connections can be removed; upstream/admin connections are preserved.
		_, e := db.Exec(ctx, `DELETE FROM guacamole_connection c WHERE connection_name LIKE 'MEA %' AND NOT EXISTS(SELECT 1 FROM mea.desktops d WHERE d.connection_id=c.connection_id AND d.state IN ('provisioning','ready','terminating'))`)
		return result, e
	default:
		return result, fmt.Errorf("unknown control action %q", r.Action)
	}
}
func ConnectionID(i int) string { return strconv.Itoa(i) }
