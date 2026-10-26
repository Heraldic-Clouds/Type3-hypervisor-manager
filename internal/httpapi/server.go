// Package httpapi serves the versioned Type3 REST API (/api/v1) on the
// standard library router. Every response is JSON; every error has the shape
// {"error": {"code", "message", "details", "request_id"}}.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/agent"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/audit"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/auth"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/containers"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/metrics"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
)

// Deps are the services the API exposes.
type Deps struct {
	Hosts        hosts.HostRegistry
	Containers   containers.ContainerManager
	Images       images.ImageManager
	Ops          operations.OperationManager
	Catalog      *profiles.Catalog
	Auth         *auth.Authenticator
	Audit        *audit.Logger
	Metrics      *metrics.Registry
	Store        store.Store
	Log          *slog.Logger
	MaxBodyBytes int64
	// Agent is set when Type3 runs on the LXD host (telemetry, logs).
	Agent *agent.Agent
}

// API holds the handlers.
type API struct {
	d Deps
}

// New builds the HTTP handler with all routes and middleware.
func New(d Deps) http.Handler {
	if d.MaxBodyBytes <= 0 {
		d.MaxBodyBytes = 1 << 20
	}
	if d.Audit == nil {
		d.Audit = audit.Discard()
	}
	if d.Metrics == nil {
		d.Metrics = metrics.New()
	}
	a := &API{d: d}
	mux := http.NewServeMux()

	viewer, operator, admin := auth.RoleViewer, auth.RoleOperator, auth.RoleAdmin
	route := func(pattern string, role auth.Role, h http.HandlerFunc) {
		mux.Handle(pattern, a.authenticate(role, h))
	}

	// Monitoring
	mux.HandleFunc("GET /api/v1/health", a.health)
	route("GET /api/v1/metrics", viewer, a.metrics)
	route("GET /api/v1/operations", viewer, a.listOperations)
	route("GET /api/v1/operations/{id}", viewer, a.getOperation)
	route("DELETE /api/v1/operations/{id}", operator, a.cancelOperation)
	route("GET /api/v1/profiles", viewer, a.listProfiles)

	// Hosts
	route("POST /api/v1/hosts", admin, a.registerHost)
	route("GET /api/v1/hosts", viewer, a.listHosts)
	route("GET /api/v1/hosts/{id}", viewer, a.getHost)
	route("GET /api/v1/hosts/{id}/resources", viewer, a.hostResources)
	route("GET /api/v1/hosts/{id}/health", viewer, a.hostHealth)
	route("GET /api/v1/hosts/{id}/containers", viewer, a.hostContainers)
	route("GET /api/v1/hosts/{id}/telemetry", viewer, a.hostTelemetry)
	route("POST /api/v1/hosts/{id}/reconcile", admin, a.reconcileHost)
	route("DELETE /api/v1/hosts/{id}", admin, a.unregisterHost)

	// Containers
	route("POST /api/v1/containers", operator, a.createContainer)
	route("GET /api/v1/containers", viewer, a.listContainers)
	route("GET /api/v1/containers/{id}", viewer, a.getContainer)
	route("POST /api/v1/containers/{id}/start", operator, a.startContainer)
	route("POST /api/v1/containers/{id}/stop", operator, a.stopContainer)
	route("POST /api/v1/containers/{id}/restart", operator, a.restartContainer)
	route("DELETE /api/v1/containers/{id}", operator, a.deleteContainer)
	route("PATCH /api/v1/containers/{id}/resources", operator, a.updateResources)
	route("POST /api/v1/containers/{id}/credentials", operator, a.setCredentials)
	route("GET /api/v1/containers/{id}/logs", viewer, a.containerLogs)
	route("GET /api/v1/containers/{id}/snapshots", viewer, a.listSnapshots)
	route("POST /api/v1/containers/{id}/snapshots", operator, a.createSnapshot)
	route("POST /api/v1/containers/{id}/snapshots/{snapshot}/restore", operator, a.restoreSnapshot)
	route("DELETE /api/v1/containers/{id}/snapshots/{snapshot}", operator, a.deleteSnapshot)

	// Images
	route("GET /api/v1/images", viewer, a.listImages)
	route("GET /api/v1/images/remote", viewer, a.listRemoteImages)
	route("POST /api/v1/images/import", admin, a.importImage)
	route("DELETE /api/v1/images/{id}", admin, a.deleteImage)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, notFoundRoute())
	})

	var h http.Handler = mux
	h = a.observe(h)
	h = limitBody(d.MaxBodyBytes, h)
	h = securityHeaders(h)
	h = requestID(h)
	h = a.recoverPanics(h)
	return h
}
