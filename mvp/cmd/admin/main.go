// admin is an IAM-only operator command for administrator identity changes.
// It is never attached to API Gateway. Run it with aws lambda invoke and delete
// it afterwards, like the dbops verification helper.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	runtime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type request struct {
	Action   string `json:"action"`
	Username string `json:"username"`
	Email    string `json:"email"`
	UserID   string `json:"userId"`
	Reason   string `json:"reason"`
}

type candidate struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	Verified      bool      `json:"verified"`
	Disabled      bool      `json:"disabled"`
	CreatedAt     time.Time `json:"createdAt"`
	GuacUserID    int       `json:"guacamoleUserId"`
	EntityID      int       `json:"guacamoleEntityId"`
	GuacEntity    string    `json:"guacamoleEntityName"`
	GuacDisabled  bool      `json:"guacamoleDisabled"`
	SystemPerms   []string  `json:"guacamoleSystemPermissions"`
	MatchUsername bool      `json:"matchesUsername"`
	MatchEmail    bool      `json:"matchesEmail"`
}

func main() {
	ctx := context.Background()
	cfg, e := config.LoadDefaultConfig(ctx)
	if e != nil {
		panic(e)
	}
	secret, e := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(os.Getenv("DB_SECRET_ARN"))})
	if e != nil {
		panic(e)
	}
	var credentials struct{ URL string }
	if e = json.Unmarshal([]byte(aws.ToString(secret.SecretString)), &credentials); e != nil {
		panic(e)
	}
	st, e := store.Open(ctx, credentials.URL)
	if e != nil {
		panic(e)
	}
	runtime.Start(func(ctx context.Context, r request) (map[string]any, error) {
		switch r.Action {
		case "lookup":
			return lookup(ctx, st, r)
		case "grant":
			return grant(ctx, st, r)
		default:
			return nil, errors.New("unsupported admin operation")
		}
	})
}

// lookup is read-only. It reports every account matching the username or
// email so an operator can confirm one immutable identity before granting.
func lookup(ctx context.Context, st *store.Store, r request) (map[string]any, error) {
	username, email := strings.ToLower(strings.TrimSpace(r.Username)), strings.ToLower(strings.TrimSpace(r.Email))
	if username == "" && email == "" {
		return nil, errors.New("username or email required")
	}
	rows, e := st.DB.Query(ctx, `SELECT u.id::text,u.username,u.email,u.name,u.verified,u.disabled,u.created_at,u.guac_user_id,u.entity_id,
 ge.name,gu.disabled,COALESCE((SELECT array_agg(p.permission::text ORDER BY p.permission) FROM guacamole_system_permission p WHERE p.entity_id=u.entity_id),'{}'),
 lower(u.username)=$1 OR lower(u.name)=$1, lower(u.email)=$2
 FROM mea.users u JOIN guacamole_entity ge ON ge.entity_id=u.entity_id JOIN guacamole_user gu ON gu.user_id=u.guac_user_id
 WHERE lower(u.username)=$1 OR lower(u.name)=$1 OR lower(u.email)=$2 ORDER BY u.created_at`, username, email)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	found := []candidate{}
	for rows.Next() {
		var c candidate
		if e = rows.Scan(&c.ID, &c.Username, &c.Email, &c.Name, &c.Verified, &c.Disabled, &c.CreatedAt, &c.GuacUserID, &c.EntityID, &c.GuacEntity, &c.GuacDisabled, &c.SystemPerms, &c.MatchUsername, &c.MatchEmail); e != nil {
			return nil, e
		}
		found = append(found, c)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	var admins []string
	arows, e := st.DB.Query(ctx, `SELECT ge.name FROM guacamole_system_permission p JOIN guacamole_entity ge ON ge.entity_id=p.entity_id WHERE p.permission='ADMINISTER' ORDER BY ge.name`)
	if e != nil {
		return nil, e
	}
	defer arows.Close()
	for arows.Next() {
		var n string
		if e = arows.Scan(&n); e != nil {
			return nil, e
		}
		admins = append(admins, n)
	}
	if e = arows.Err(); e != nil {
		return nil, e
	}
	// Connections MEA did not create (for example an XP25 administrative
	// connection) must be preserved, so report them by name and protocol only.
	var preserved []string
	crows, e := st.DB.Query(ctx, `SELECT connection_name||' ('||protocol||')' FROM guacamole_connection WHERE connection_name NOT LIKE 'MEA %' ORDER BY connection_name`)
	if e != nil {
		return nil, e
	}
	defer crows.Close()
	for crows.Next() {
		var n string
		if e = crows.Scan(&n); e != nil {
			return nil, e
		}
		preserved = append(preserved, n)
	}
	var users int
	if e = st.DB.QueryRow(ctx, `SELECT count(*) FROM mea.users`).Scan(&users); e != nil {
		return nil, e
	}
	return map[string]any{"candidates": found, "guacamoleAdministrators": admins, "otherConnections": preserved, "totalUsers": users}, crows.Err()
}

// grant promotes one account, named by its immutable ID and checked against
// the expected verified email. It is idempotent and audited.
func grant(ctx context.Context, st *store.Store, r request) (map[string]any, error) {
	if r.UserID == "" || r.Email == "" {
		return nil, errors.New("userId and email are both required")
	}
	changed, e := beta.GrantAdmin(ctx, st.DB, "", r.UserID, r.Email, r.Reason)
	if e != nil {
		return nil, e
	}
	result, e := lookup(ctx, st, request{Email: r.Email})
	if result != nil {
		result["changed"] = changed
		result["meaAdmin"], _ = beta.IsAdmin(ctx, st.DB, r.UserID)
		result["access"], _ = beta.AccessFor(ctx, st.DB, r.UserID)
	}
	return result, e
}
