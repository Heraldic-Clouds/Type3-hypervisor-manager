// dbops is an IAM-only, temporary deployment-verification helper.
// It is never attached to API Gateway and must be deleted after the test run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	runtime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"os"
	"strings"
)

type request struct{ Action, Email, ID string }

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
		case "verification":
			if !strings.HasPrefix(r.Email, "meae2e") || !strings.HasSuffix(r.Email, "@simulator.amazonses.com") {
				return nil, errors.New("only deployment test identities are permitted")
			}
			var token, user, username string
			var guac int
			e := st.DB.QueryRow(ctx, "SELECT o.payload->>'token',u.id::text,u.username,u.guac_user_id FROM mea.users u JOIN mea.tokens t ON t.user_id=u.id JOIN mea.outbox o ON o.payload->>'email'=u.email WHERE u.email=$1 AND t.kind='verify' AND t.used_at IS NULL AND o.payload->>'kind'='verify' ORDER BY o.created_at DESC LIMIT 1", r.Email).Scan(&token, &user, &username, &guac)
			return map[string]any{"token": token, "user": user, "username": username, "guacamoleUserId": guac}, e
		case "inspect":
			var id, state string
			var instance *string
			var connection *int
			var count int
			e := st.DB.QueryRow(ctx, "SELECT d.id::text,d.state,d.instance_id,d.connection_id,(SELECT count(*) FROM guacamole_connection_permission p WHERE p.connection_id=d.connection_id) FROM mea.desktops d JOIN mea.users u ON u.id=d.user_id WHERE d.id=$1 AND u.email LIKE 'meae2e%@simulator.amazonses.com'", r.ID).Scan(&id, &state, &instance, &connection, &count)
			return map[string]any{"id": id, "state": state, "instanceId": instance, "connectionId": connection, "permissionCount": count}, e
		case "expire":
			tag, e := st.DB.Exec(ctx, "UPDATE mea.desktops SET last_activity=now()-interval '21 minutes',last_heartbeat=now() WHERE id=$1 AND user_id IN (SELECT id FROM mea.users WHERE email LIKE 'meae2e%@simulator.amazonses.com')", r.ID)
			return map[string]any{"updated": tag.RowsAffected()}, e
		default:
			return nil, errors.New("unsupported verification operation")
		}
	})
}
