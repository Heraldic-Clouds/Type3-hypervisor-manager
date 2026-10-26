# Milestone 1 implementation plan

## What existed

The repository held a single-file prototype (`main.go`) written for AWS
Lambda/CodeStar:

* It downloaded the LXD client certificate, key and server certificate from a
  hardcoded S3 bucket into `/tmp`, using AWS SDK v1 with static credentials.
* It **logged the AWS access key, secret key and session token** in plain text.
* It connected to a hardcoded private IP (`https://172.30.2.171:8443`) with
  `github.com/lxc/lxd/client`, created a container named
  `madewithapiLambda-1` from the `image4go` alias via the deprecated
  `CreateContainer`, and started it with `UpdateContainerState`.
* There was no `go.mod`, the test was a commented-out stub, and four compiled
  binaries (~60 MB) were committed.
* `template.yml`/`buildspec.yml`/`public/` were the unchanged AWS CodeStar
  "hello world" scaffold (Lambda `go1.x` runtime, now retired by AWS).

## What is kept, and where it goes

| Prototype behaviour | New home |
| --- | --- |
| mTLS connection to a remote LXD (client cert + key + pinned server cert) | `internal/lxdclient` (`Connector`), using `github.com/canonical/lxd/client` |
| Certificates fetched from S3 | `internal/secrets` resolves `s3://bucket/key` references with AWS SDK v2, in memory only (never written to `/tmp`, never logged) |
| Create container from image alias, then start | `internal/containers` (`CreateInstance` + `UpdateInstanceState`, modern instance API) |
| CodeStar/Lambda scaffold | moved to `legacy/aws-codestar/` untouched, for reference until Milestone 2 |

## Layout

```
cmd/type3            server binary and helper subcommands (serve, gen-key, hash-key, version)
internal/config      env/.env configuration, validation
internal/logging     slog JSON/text logger with secret redaction
internal/secrets     file:// / s3:// / env:// secret references
internal/errs        typed errors -> consistent JSON error responses
internal/model       Host, Container, Operation, Tier records
internal/store       Store interface + atomic JSON-file implementation (PostgreSQL in M3)
internal/lxdclient   narrow LXD interface, real adapter, TLS/mTLS connector
internal/hosts       HostRegistry, health monitor, resource discovery, HostProvider, placement
internal/gpu         GPU inventory and tier eligibility
internal/profiles    Newbie / Power / Eleet catalog, validation, LXD instance spec builder
internal/operations  async OperationManager (progress, timeouts, cancellation)
internal/containers  ContainerManager: lifecycle, resources, snapshots, reconciliation
internal/images      ImageManager: local/remote listing, import with fingerprint check, delete
internal/auth        API keys (SHA-256 hashed), roles admin/operator/viewer
internal/audit       audit log for destructive and privileged operations
internal/metrics     Prometheus text exposition
internal/httpapi     /api/v1 REST API on net/http ServeMux
```

## Order of work

1. Module, config, logging, legacy cleanup.
2. Store, models, errors.
3. LXD client wrapper + connector; host registry, health, resources, GPU.
4. Profiles, operations, container manager, images.
5. Auth, audit, metrics, HTTP API, server main with graceful shutdown.
6. Unit tests against an in-memory fake LXD; integration test gated on
   `TYPE3_IT_LXD_ENDPOINT`.
7. OpenAPI, Makefile, `.env.example`, scripts, docs.
