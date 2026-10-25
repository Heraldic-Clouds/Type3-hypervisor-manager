# Legacy prototype

`prototype_main.go.txt` is the original Lambda prototype, kept for reference
(renamed so it is not compiled). It logged AWS secrets and used hardcoded
hosts; do not revive it. Its behaviour now lives in:

* `internal/lxdclient` — mTLS connection to LXD
* `internal/secrets` — certificates loaded from `s3://` references (AWS SDK v2, in memory)
* `internal/containers` — create from image and start, via the modern instance API

`aws-codestar/` is the original CodeStar scaffold (Lambda `go1.x` runtime,
which AWS has retired). AWS deployment is Milestone 2 and will replace it.
