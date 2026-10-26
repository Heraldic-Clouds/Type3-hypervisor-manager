package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/billing"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/paid"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/jackc/pgx/v5"
	"net"
	"regexp"
	"strconv"
	"strings"
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
	Port       int    `json:"port,omitempty"`
	// Observation is the worker's report of the GPU host instance.
	Observation *paid.Observation `json:"observation,omitempty"`
	// Billing carries a database operation for the billing function, which
	// has internet access for Stripe but no route to the private database.
	Billing *billing.Call `json:"billing,omitempty"`
	// Shared desktop registration ("shared-desktop"), invoked only by operators through IAM.
	Kind        string            `json:"kind,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Protocol    string            `json:"protocol,omitempty"`
	Parameters  map[string]string `json:"parameters,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
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
	Jobs         []Job          `json:"jobs,omitempty"`
	Mails        []Mail         `json:"mails,omitempty"`
	Ready        bool           `json:"ready"`
	Skip         bool           `json:"skip"`
	Count        int            `json:"count"`
	Billing      *billing.Reply `json:"billing,omitempty"`
	Plan         *paid.Plan     `json:"plan,omitempty"`
	Summary      map[string]any `json:"summary,omitempty"`
	ConnectionID int            `json:"connectionId,omitempty"`
}
type Service struct {
	Store                     *store.Store
	IdleSeconds, GraceSeconds int
	Local                     bool
	// Paid configures Power/Eleet; Type3 connects to the GPU host's Type3.
	Paid  paid.Config
	Type3 func(ctx context.Context, h paid.Host) (paid.Type3, error)
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
	case "gpu-plan":
		plan, e := paid.HostPlan(ctx, s.Store, s.Paid)
		result.Plan = &plan
		return result, e
	case "gpu-lease":
		ok, e := paid.Lease(ctx, s.Store, r.ID, 5*time.Minute)
		result.Ready = ok
		return result, e
	case "gpu-observed":
		if r.Observation == nil {
			return result, errors.New("observation required")
		}
		return result, paid.Observe(ctx, s.Store, *r.Observation)
	case "paid-reconcile":
		if s.Type3 == nil {
			result.Skip = true
			return result, nil
		}
		en := &paid.Engine{Store: s.Store, Config: s.Paid, Type3: s.Type3,
			Ready: func(ctx context.Context, desktop, ip string, port int, password string) (bool, error) {
				res, err := s.Handle(ctx, Request{Action: "ready", ID: desktop, IP: ip, Port: port, Password: password})
				return res.Ready, err
			},
			Terminated: func(ctx context.Context, desktop string) error {
				_, err := s.Handle(ctx, Request{Action: "terminated", ID: desktop})
				return err
			}}
		summary, e := en.Reconcile(ctx)
		result.Summary = summary
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
		// Idle and disconnect limits apply to every tier; a paid desktop's
		// keep-alive suspends them, and paid provisioning has its own timeout.
		_, e = tx.Exec(ctx, `UPDATE mea.desktops SET state='terminating' WHERE state IN ('waiting','queued','provisioning','ready') AND (
    expires_at<now() OR (tier='newbie' AND state IN ('queued','provisioning') AND created_at<now()-interval '15 minutes')
    OR (state='ready' AND last_activity<now()-$1::interval AND NOT COALESCE(keep_alive_until>now(),false))
    OR (state='ready' AND disconnect_at<now()-$2::interval AND NOT COALESCE(keep_alive_until>now(),false)))`, fmt.Sprintf("%d seconds", s.IdleSeconds), fmt.Sprintf("%d seconds", s.GraceSeconds))
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
		// A shared lease owns no machine: with its permission revoked and sessions
		// closed it is finished, and its connection is released for the next lease.
		_, e = tx.Exec(ctx, `UPDATE mea.desktops SET state='terminated',connection_id=NULL,terminated_at=now() WHERE state='terminating' AND kind<>'ubuntu'`)
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
		// EC2 desktop jobs are Newbie only; paid desktops run in LXD containers.
		rows, e := db.Query(ctx, `SELECT id::text,state,instance_id,operation_id::text,created_at FROM mea.desktops WHERE tier='newbie' AND state IN ('queued','provisioning','terminating') ORDER BY created_at LIMIT 10`)
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
		e = db.QueryRow(ctx, `SELECT count(*) FROM mea.desktops WHERE tier='newbie' AND kind='ubuntu' AND state IN ('queued','provisioning','ready','terminating')`).Scan(&result.Count)
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
		port := "3389"
		if r.Port != 0 {
			port = strconv.Itoa(r.Port)
		}
		conn, e := net.DialTimeout("tcp", net.JoinHostPort(r.IP, port), 2*time.Second)
		if e != nil {
			return result, nil
		}
		conn.Close()
		// Verify an RDP negotiation response, not only an accepting TCP port.
		conn, e = net.DialTimeout("tcp", net.JoinHostPort(r.IP, port), 2*time.Second)
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
		for k, v := range map[string]string{"hostname": r.IP, "port": port, "username": "mea", "password": r.Password, "security": "tls", "ignore-cert": "true", "resize-method": "display-update", "enable-wallpaper": "false", "enable-font-smoothing": "false", "disable-audio": "true"} {
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
		_, e = tx.Exec(ctx, `DELETE FROM guacamole_connection WHERE connection_id=(SELECT connection_id FROM mea.desktops WHERE id=$1 AND kind='ubuntu')`, r.ID)
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
	case "shared-desktop":
		return s.sharedDesktop(ctx, r)
	case "orphans":
		// Only MEA-owned connections can be removed; upstream/admin connections are preserved.
		_, e := db.Exec(ctx, `DELETE FROM guacamole_connection c WHERE connection_name LIKE 'MEA %' AND NOT EXISTS(SELECT 1 FROM mea.desktops d WHERE d.connection_id=c.connection_id AND d.state IN ('provisioning','ready','terminating'))`)
		return result, e
	default:
		return result, fmt.Errorf("unknown control action %q", r.Action)
	}
}
func ConnectionID(i int) string { return strconv.Itoa(i) }

var sharedKind = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Connection parameters an operator may set on a shared desktop. Anything else,
// such as SFTP, drive redirection or recording, is refused.
var sharedParameters = map[string]bool{"hostname": true, "port": true, "username": true, "password": true,
	"color-depth": true, "cursor": true, "encodings": true, "read-only": true, "swap-red-blue": true,
	"disable-copy": true, "disable-paste": true, "clipboard-encoding": true, "autoretry": true,
	"security": true, "ignore-cert": true, "resize-method": true, "disable-audio": true}

// sharedDesktop creates or updates a shared desktop and its Guacamole connection.
// Disabling one ends any current lease at the next reconciliation.
func (s *Service) sharedDesktop(ctx context.Context, r Request) (Result, error) {
	result := Result{}
	if !sharedKind.MatchString(r.Kind) || r.Kind == store.Ubuntu {
		return result, fmt.Errorf("invalid shared desktop kind")
	}
	name := strings.TrimSpace(r.Name)
	if name == "" || len(name) > 64 || strings.HasPrefix(name, "MEA ") {
		return result, fmt.Errorf("invalid shared desktop name")
	}
	if r.Protocol != "vnc" && r.Protocol != "rdp" {
		return result, fmt.Errorf("unsupported shared desktop protocol")
	}
	for k := range r.Parameters {
		if !sharedParameters[k] {
			return result, fmt.Errorf("unsupported connection parameter %q", k)
		}
	}
	if r.Parameters["hostname"] == "" || r.Parameters["port"] == "" {
		return result, fmt.Errorf("hostname and port are required")
	}
	enabled := r.Enabled == nil || *r.Enabled
	tx, e := s.Store.DB.Begin(ctx)
	if e != nil {
		return result, e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(739103)"); e != nil {
		return result, e
	}
	var connection int
	e = tx.QueryRow(ctx, `SELECT connection_id FROM mea.shared_desktops WHERE kind=$1 FOR UPDATE`, r.Kind).Scan(&connection)
	switch {
	case errors.Is(e, pgx.ErrNoRows):
		if e = tx.QueryRow(ctx, `INSERT INTO guacamole_connection(connection_name,protocol,max_connections,max_connections_per_user) VALUES($1,$2,1,1) RETURNING connection_id`, name, r.Protocol).Scan(&connection); e != nil {
			return result, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO mea.shared_desktops(kind,name,description,connection_id,enabled) VALUES($1,$2,$3,$4,$5)`, r.Kind, name, r.Description, connection, enabled); e != nil {
			return result, e
		}
	case e != nil:
		return result, e
	default:
		if _, e = tx.Exec(ctx, `UPDATE guacamole_connection SET connection_name=$2,protocol=$3,max_connections=1,max_connections_per_user=1 WHERE connection_id=$1`, connection, name, r.Protocol); e != nil {
			return result, e
		}
		if _, e = tx.Exec(ctx, `DELETE FROM guacamole_connection_parameter WHERE connection_id=$1`, connection); e != nil {
			return result, e
		}
		if _, e = tx.Exec(ctx, `UPDATE mea.shared_desktops SET name=$2,description=$3,enabled=$4,updated_at=now() WHERE kind=$1`, r.Kind, name, r.Description, enabled); e != nil {
			return result, e
		}
	}
	for k, v := range r.Parameters {
		if _, e = tx.Exec(ctx, `INSERT INTO guacamole_connection_parameter(connection_id,parameter_name,parameter_value) VALUES($1,$2,$3)`, connection, k, v); e != nil {
			return result, e
		}
	}
	if !enabled {
		if _, e = tx.Exec(ctx, `UPDATE mea.desktops SET state='terminating' WHERE kind=$1 AND state IN ('queued','provisioning','ready')`, r.Kind); e != nil {
			return result, e
		}
	}
	if _, e = tx.Exec(ctx, `INSERT INTO mea.audit(action) VALUES($1)`, "shared.configure:"+r.Kind); e != nil {
		return result, e
	}
	if e = tx.Commit(ctx); e != nil {
		return result, e
	}
	result.Ready, result.ConnectionID = enabled, connection
	return result, nil
}
