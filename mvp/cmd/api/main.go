package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/api"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/aws/aws-lambda-go/events"
	lambdaruntime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
)

func env(k, v string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return v
}
func number(k string, v int) int {
	n, e := strconv.Atoi(os.Getenv(k))
	if e != nil {
		return v
	}
	return n
}
func main() {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	key := os.Getenv("GUAC_JSON_KEY")
	var appPassword, guacPassword string
	if arn := os.Getenv("DB_SECRET_ARN"); arn != "" {
		cfg, e := config.LoadDefaultConfig(ctx)
		if e != nil {
			log.Fatal(e)
		}
		out, e := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(arn)})
		if e != nil {
			log.Fatal(e)
		}
		var secret struct{ URL, GuacKey, AppPassword, GuacPassword string }
		if e = json.Unmarshal([]byte(aws.ToString(out.SecretString)), &secret); e != nil {
			log.Fatal(e)
		}
		dsn, key = secret.URL, secret.GuacKey
		appPassword, guacPassword = secret.AppPassword, secret.GuacPassword
	}
	st, e := store.Open(ctx, dsn)
	if e != nil {
		log.Fatal(e)
	}
	mode := env("MODE", "api")
	if mode == "migrate" {
		run := func(ctx context.Context, _ json.RawMessage) (map[string]bool, error) {
			b, e := os.ReadFile(env("GUAC_SCHEMA", "/schema/guacamole.sql"))
			if e != nil {
				return nil, e
			}
			if e = st.Migrate(ctx, string(b)); e != nil {
				return nil, e
			}
			if e = st.BootstrapRoles(ctx, appPassword, guacPassword); e != nil {
				return nil, e
			}
			return map[string]bool{"ok": true}, nil
		}
		if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
			lambdaruntime.Start(run)
			return
		}
		if _, e = run(ctx, nil); e != nil {
			log.Fatal(e)
		}
		return
	}
	c := api.Config{Origin: env("APP_ORIGIN", "http://localhost:5173"), GuacKey: key, Secure: env("SECURE_COOKIES", "true") == "true", RequireVerification: env("REQUIRE_VERIFICATION", "true") == "true", Local: env("LOCAL_MODE", "false") == "true", Cap: number("DESKTOP_CAP", 2), IdleSeconds: number("IDLE_SECONDS", 1200), GraceSeconds: number("GRACE_SECONDS", 300), LifetimeSeconds: number("LIFETIME_SECONDS", 14400)}
	if mode == "control" {
		svc := &control.Service{Store: st, IdleSeconds: c.IdleSeconds, GraceSeconds: c.GraceSeconds, Local: c.Local}
		lambdaruntime.Start(svc.Handle)
		return
	}
	app := api.New(st, c).Handler()
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		adapter := httpadapter.NewV2(app)
		lambdaruntime.Start(func(ctx context.Context, r events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) { // Trust API Gateway's source address, never client-supplied forwarding headers.
			r.Headers["x-mea-source-ip"] = r.RequestContext.HTTP.SourceIP
			return adapter.ProxyWithContext(ctx, r)
		})
		return
	}
	if strings.HasPrefix(dsn, "postgres") {
		fmt.Println("MEA API listening on :8081")
	}
	log.Fatal(http.ListenAndServe(":8081", app))
}
