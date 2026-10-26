package store

import (
	"context"
	"embed"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ DB *pgxpool.Pool }

func (s *Store) BootstrapRoles(ctx context.Context, appPassword, guacPassword string) error {
	if appPassword == "" || guacPassword == "" {
		return nil
	}
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	for name, pw := range map[string]string{"mea_app": appPassword, "mea_guac": guacPassword} {
		var exists bool
		if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", name).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			var command string
			if e = tx.QueryRow(ctx, "SELECT format('CREATE ROLE %I LOGIN PASSWORD %L',$1::text,$2::text)", name, pw).Scan(&command); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, command); e != nil {
				return e
			}
		}
	}
	for _, q := range []string{
		"GRANT USAGE ON SCHEMA public,mea TO mea_app,mea_guac",
		"GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO mea_app,mea_guac",
		"GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO mea_app,mea_guac",
		"GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA mea TO mea_app",
		"GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA mea TO mea_app",
		"GRANT SELECT ON mea.users,mea.sessions,mea.desktops,mea.desktop_sessions TO mea_guac",
		"GRANT UPDATE(tunnel_id,connected_at,closed_at) ON mea.desktop_sessions TO mea_guac",
	} {
		if _, e = tx.Exec(ctx, q); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}

func Open(ctx context.Context, url string) (*Store, error) {
	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		return nil, e
	}
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = time.Minute
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return nil, e
	}
	return &Store{db}, nil
}
func (s *Store) Migrate(ctx context.Context, guacSQL string) error {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(739102)"); e != nil {
		return e
	}
	var exists bool
	if e = tx.QueryRow(ctx, "SELECT to_regclass('public.guacamole_user') IS NOT NULL").Scan(&exists); e != nil {
		return e
	}
	if !exists {
		if guacSQL == "" {
			return fmt.Errorf("official Guacamole schema required")
		}
		if _, e = tx.Exec(ctx, guacSQL); e != nil {
			return e
		}
	}
	// Versioned migrations run once each, in file-name order, inside the same
	// locked transaction. 001 predates tracking and is idempotent.
	if _, e = tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS mea; CREATE TABLE IF NOT EXISTS mea.schema_migrations(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); e != nil {
		return e
	}
	entries, e := migrations.ReadDir("migrations")
	if e != nil {
		return e
	}
	for _, entry := range entries {
		var done bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM mea.schema_migrations WHERE version=$1)`, entry.Name()).Scan(&done); e != nil {
			return e
		}
		if done {
			continue
		}
		b, e := migrations.ReadFile("migrations/" + entry.Name())
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, string(b)); e != nil {
			return fmt.Errorf("migration %s: %w", entry.Name(), e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO mea.schema_migrations(version) VALUES($1)`, entry.Name()); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) Rate(ctx context.Context, key string, max int, window time.Duration) bool {
	var hits int
	e := s.DB.QueryRow(ctx, `INSERT INTO mea.rate_limits(key,hits,reset_at) VALUES($1,1,now()+$2::interval)
 ON CONFLICT(key) DO UPDATE SET hits=CASE WHEN mea.rate_limits.reset_at<now() THEN 1 ELSE mea.rate_limits.hits+1 END,
 reset_at=CASE WHEN mea.rate_limits.reset_at<now() THEN EXCLUDED.reset_at ELSE mea.rate_limits.reset_at END RETURNING hits`, key, fmt.Sprintf("%f seconds", window.Seconds())).Scan(&hits)
	return e == nil && hits <= max
}

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	Name         string `json:"name"`
	Verified     bool   `json:"verified"`
	Subscription string `json:"subscription"`
	EntityID     int    `json:"-"`
}
type Desktop struct {
	ID           string     `json:"id"`
	UserID       string     `json:"-"`
	State        string     `json:"state"`
	InstanceID   *string    `json:"-"`
	PrivateIP    *string    `json:"-"`
	ConnectionID *int       `json:"-"`
	OperationID  string     `json:"operationId"`
	LastActivity time.Time  `json:"lastActivity"`
	ExpiresAt    time.Time  `json:"expiresAt"`
	Error        *string    `json:"error,omitempty"`
	Tier         string     `json:"tier"`
	KeepAlive    *time.Time `json:"keepAliveUntil,omitempty"`
	Kind         string     `json:"kind"`
}

// Ubuntu is the kind of the disposable per-user EC2 desktop. Any other kind is
// a lease on a row of mea.shared_desktops.
const Ubuntu = "ubuntu"

const DesktopColumns = `id::text,user_id::text,state,instance_id,private_ip,connection_id,operation_id::text,last_activity,expires_at,error,tier,keep_alive_until,kind`

func ScanDesktop(row pgx.Row) (Desktop, error) {
	var d Desktop
	e := row.Scan(&d.ID, &d.UserID, &d.State, &d.InstanceID, &d.PrivateIP, &d.ConnectionID, &d.OperationID, &d.LastActivity, &d.ExpiresAt, &d.Error, &d.Tier, &d.KeepAlive, &d.Kind)
	return d, e
}
func (s *Store) Desktop(ctx context.Context, user, id string) (Desktop, error) {
	return ScanDesktop(s.DB.QueryRow(ctx, "SELECT "+DesktopColumns+" FROM mea.desktops WHERE user_id=$1 AND id=$2", user, id))
}

type Shared struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Available   bool   `json:"available"`
}

// SharedDesktops lists enabled shared desktops and whether their seat is free.
func (s *Store) SharedDesktops(ctx context.Context) ([]Shared, error) {
	rows, e := s.DB.Query(ctx, `SELECT s.kind,s.name,s.description,NOT EXISTS(SELECT 1 FROM mea.desktops d WHERE d.kind=s.kind AND d.state IN ('waiting','queued','provisioning','ready','terminating'))
 FROM mea.shared_desktops s WHERE s.enabled ORDER BY s.name`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []Shared{}
	for rows.Next() {
		var d Shared
		if e = rows.Scan(&d.Kind, &d.Name, &d.Description, &d.Available); e != nil {
			return nil, e
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
