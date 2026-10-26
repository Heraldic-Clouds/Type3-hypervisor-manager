// mea-stream-token is an operator tool: it generates the Ed25519 key pair the
// MEA API and gateways share, and mints test tokens for local development.
//
//	mea-stream-token keygen
//	MEA_STREAM_PRIVATE_KEY=... mea-stream-token mint -user u -desktop d -host h
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/internal/env"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mea-stream-token keygen | mint [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			panic(err)
		}
		fmt.Printf("MEA_STREAM_PUBLIC_KEY=%s\n", base64.StdEncoding.EncodeToString(pub))
		fmt.Printf("MEA_STREAM_PRIVATE_KEY=%s\n", base64.StdEncoding.EncodeToString(priv.Seed()))
	case "mint":
		fs := flag.NewFlagSet("mint", flag.ExitOnError)
		user := fs.String("user", "dev-user", "MEA user id")
		desk := fs.String("desktop", "dev-desktop", "desktop id")
		host := fs.String("host", "dev-host", "Type3 host id")
		ent := fs.String("ent", "eleet", "entitlement")
		prof := fs.String("profile", "balanced", "max profile")
		ttl := fs.Duration("ttl", 60*time.Second, "lifetime (max 120s)")
		fs.Parse(os.Args[2:])
		priv, err := token.ParsePrivateKey(env.Secret("MEA_STREAM_PRIVATE_KEY"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		now := time.Now()
		tok, _, err := token.Mint(priv, token.Claims{
			User: *user, Desktop: *desk, Host: *host, Session: token.NewID(),
			Entitlement: *ent, Profile: *prof, IssuedAt: now.Unix(), ExpiresAt: now.Add(*ttl).Unix(),
		}, now)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(tok)
	default:
		fmt.Fprintln(os.Stderr, "unknown command", os.Args[1])
		os.Exit(2)
	}
}
