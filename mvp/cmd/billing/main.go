// billing serves /api/billing/* and the Stripe webhook. In AWS it runs outside
// the VPC so it can reach Stripe, and reaches the database only through the
// control function. Stripe keys live in Secrets Manager; live keys are refused.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/beta"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/billing"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/store"
	"github.com/aws/aws-lambda-go/events"
	lambdaruntime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
)

type keys struct {
	SecretKey     string `json:"secretKey"`
	WebhookSecret string `json:"webhookSecret"`
}

// keyCache rereads the secret every five minutes so rotation needs no deploy.
type keyCache struct {
	mu     sync.Mutex
	at     time.Time
	value  keys
	client *secretsmanager.Client
	arn    string
}

func (k *keyCache) get(ctx context.Context) keys {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.client == nil || time.Since(k.at) < 5*time.Minute {
		return k.value
	}
	out, e := k.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(k.arn)})
	if e != nil {
		slog.Error("stripe secret unavailable")
		return k.value
	}
	var v keys
	if json.Unmarshal([]byte(aws.ToString(out.SecretString)), &v) == nil {
		k.value, k.at = v, time.Now()
	}
	return k.value
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx := context.Background()
	prices := billing.Prices{"power": os.Getenv("STRIPE_PRICE_POWER"), "eleet": os.Getenv("STRIPE_PRICE_ELEET")}
	cache := &keyCache{value: keys{SecretKey: os.Getenv("STRIPE_SECRET_KEY"), WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET")}}
	client := &billing.Client{Prices: prices}
	srv := &billing.Server{Stripe: lazyKey{client, cache}, Prices: prices, Origin: os.Getenv("APP_ORIGIN"), WebhookSecret: func() string { return cache.get(context.Background()).WebhookSecret }}
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") == "" {
		// Local development: talk to PostgreSQL directly.
		st, e := store.Open(ctx, os.Getenv("DATABASE_URL"))
		if e != nil {
			log.Fatal(e)
		}
		srv.Backend = billing.DB{Store: st}
		fmt.Println("MEA billing listening on :8082")
		log.Fatal(http.ListenAndServe(":8082", srv))
	}
	cfg, e := config.LoadDefaultConfig(ctx)
	if e != nil {
		log.Fatal(e)
	}
	cache.client, cache.arn = secretsmanager.NewFromConfig(cfg), os.Getenv("STRIPE_SECRET_ARN")
	cache.at = time.Time{}
	invoker := lambda.NewFromConfig(cfg)
	function := os.Getenv("CONTROL_FUNCTION")
	srv.Backend = billing.Remote{Invoke: func(ctx context.Context, c billing.Call) (billing.Reply, error) {
		b, _ := json.Marshal(control.Request{Action: "billing", Billing: &c})
		out, e := invoker.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(function), Payload: b})
		if e != nil {
			return billing.Reply{}, e
		}
		if out.FunctionError != nil {
			return billing.Reply{}, fmt.Errorf("control billing %s failed", c.Op)
		}
		var r control.Result
		if e = json.Unmarshal(out.Payload, &r); e != nil || r.Billing == nil {
			return billing.Reply{}, fmt.Errorf("control billing %s: empty reply", c.Op)
		}
		return *r.Billing, nil
	}}
	adapter := httpadapter.NewV2(srv)
	lambdaruntime.Start(func(ctx context.Context, r events.APIGatewayV2HTTPRequest) (events.APIGatewayV2HTTPResponse, error) {
		// Trust API Gateway's source address, never client-supplied forwarding headers.
		r.Headers["x-mea-source-ip"] = r.RequestContext.HTTP.SourceIP
		return adapter.ProxyWithContext(ctx, r)
	})
}

// lazyKey resolves the current Stripe key for each call.
type lazyKey struct {
	c     *billing.Client
	cache *keyCache
}

func (l lazyKey) client(ctx context.Context) *billing.Client {
	c := *l.c
	c.Key = l.cache.get(ctx).SecretKey
	return &c
}
func (l lazyKey) CreateCustomer(ctx context.Context, user, email, name string) (string, error) {
	return l.client(ctx).CreateCustomer(ctx, user, email, name)
}
func (l lazyKey) CreateCheckout(ctx context.Context, p billing.CheckoutParams) (string, string, error) {
	return l.client(ctx).CreateCheckout(ctx, p)
}
func (l lazyKey) Portal(ctx context.Context, customer, returnURL string) (string, error) {
	return l.client(ctx).Portal(ctx, customer, returnURL)
}
func (l lazyKey) Subscription(ctx context.Context, id string) (beta.Subscription, error) {
	return l.client(ctx).Subscription(ctx, id)
}
func (l lazyKey) ChangePrice(ctx context.Context, sub, price, key string) error {
	return l.client(ctx).ChangePrice(ctx, sub, price, key)
}
