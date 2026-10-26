FROM golang:1.27.3 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /api ./cmd/api && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /billing ./cmd/billing
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=build /api /api
COPY --from=build /billing /billing
COPY schema /schema
USER 65532:65532
ENTRYPOINT ["/api"]
