// Package model holds the records Type3 persists and returns over the API.
package model

import (
	"time"
)

// HostStatus is Type3's view of an LXD host's reachability.
type HostStatus string

const (
	HostUnknown  HostStatus = "unknown"
	HostOnline   HostStatus = "online"
	HostDegraded HostStatus = "degraded"
	HostOffline  HostStatus = "offline"
)

// Host is a registered LXD server (a standalone daemon or one endpoint of an
// LXD cluster). Provider and ProviderRef let later milestones attach EC2 or
// dedicated-server metadata without changing this shape.
type Host struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Project  string `json:"project"`

	// ServerCertPEM is the LXD server's public certificate, pinned for TLS.
	ServerCertPEM         string `json:"server_cert,omitempty"`
	ServerCertFingerprint string `json:"server_cert_fingerprint,omitempty"`
	// ClientCertRef/ClientKeyRef optionally override the global LXD client
	// identity for this host. They are references (paths/URIs), never contents.
	ClientCertRef string `json:"client_cert_ref,omitempty"`
	ClientKeyRef  string `json:"client_key_ref,omitempty"`

	Provider    string            `json:"provider"`
	ProviderRef string            `json:"provider_ref,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`

	DefaultNetwork     string `json:"default_network"`
	DefaultStoragePool string `json:"default_storage_pool"`
	// Schedulable=false excludes the host from automatic placement.
	Schedulable bool `json:"schedulable"`

	Status              HostStatus `json:"status"`
	StatusReason        string     `json:"status_reason,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	LastSeenAt          *time.Time `json:"last_seen_at,omitempty"`
	LastCheckAt         *time.Time `json:"last_check_at,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LatencyMillis       int64      `json:"latency_ms"`

	ServerName     string   `json:"server_name,omitempty"`
	ServerVersion  string   `json:"server_version,omitempty"`
	Clustered      bool     `json:"clustered"`
	ClusterMembers []string `json:"cluster_members,omitempty"`

	Capacity HostCapacity `json:"capacity"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HostCapacity caches the last discovered totals for placement decisions.
type HostCapacity struct {
	CPUThreads   uint64    `json:"cpu_threads"`
	MemoryBytes  uint64    `json:"memory_bytes"`
	GPUs         int       `json:"gpus"`
	NvidiaGPUs   int       `json:"nvidia_gpus"`
	DiscoveredAt time.Time `json:"discovered_at,omitempty"`
}

// GPUCapable reports whether the host exposes at least one NVIDIA GPU.
func (h *Host) GPUCapable() bool { return h.Capacity.NvidiaGPUs > 0 }

// ContainerStatus is the lifecycle state Type3 reports for a container.
type ContainerStatus string

const (
	ContainerPending    ContainerStatus = "pending"
	ContainerCreating   ContainerStatus = "creating"
	ContainerStopped    ContainerStatus = "stopped"
	ContainerStarting   ContainerStatus = "starting"
	ContainerRunning    ContainerStatus = "running"
	ContainerStopping   ContainerStatus = "stopping"
	ContainerRestarting ContainerStatus = "restarting"
	ContainerFrozen     ContainerStatus = "frozen"
	ContainerDeleting   ContainerStatus = "deleting"
	ContainerError      ContainerStatus = "error"
	// ContainerMissing means Type3 has a record but LXD has no instance.
	ContainerMissing ContainerStatus = "missing"
	ContainerUnknown ContainerStatus = "unknown"
)

// Resources are the CPU, memory and disk allocation of a container.
type Resources struct {
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memory_mib"`
	DiskGiB   int `json:"disk_gib"`
}

// ImageRef identifies the image a container was created from.
type ImageRef struct {
	// Server is a configured image server name ("ubuntu") or empty for an
	// image already present on the host.
	Server      string `json:"server,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Container is a managed customer system container.
type Container struct {
	// ID equals the LXD instance name and is unique across all hosts.
	ID      string `json:"id"`
	HostID  string `json:"host_id"`
	OwnerID string `json:"owner_id"`
	Tier    string `json:"tier"`

	Image       ImageRef  `json:"image"`
	Resources   Resources `json:"resources"`
	GPU         bool      `json:"gpu"`
	RootAccess  bool      `json:"root_access"`
	Ephemeral   bool      `json:"ephemeral"`
	Network     string    `json:"network"`
	StoragePool string    `json:"storage_pool"`
	Profiles    []string  `json:"profiles"`

	Status    ContainerStatus `json:"status"`
	LXDStatus string          `json:"lxd_status,omitempty"`
	IPv4      []string        `json:"ipv4,omitempty"`
	IPv6      []string        `json:"ipv6,omitempty"`
	Location  string          `json:"location,omitempty"`
	Error     string          `json:"error,omitempty"`
	// Orphaned is set when reconciliation adopted an instance Type3 had no
	// record for.
	Orphaned bool `json:"orphaned,omitempty"`

	SpecHash        string     `json:"spec_hash"`
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	LastOperationID string     `json:"last_operation_id,omitempty"`
	ObservedAt      *time.Time `json:"observed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// OperationStatus is the state of an asynchronous Type3 operation.
type OperationStatus string

const (
	OpPending   OperationStatus = "pending"
	OpRunning   OperationStatus = "running"
	OpSucceeded OperationStatus = "succeeded"
	OpFailed    OperationStatus = "failed"
	OpCancelled OperationStatus = "cancelled"
)

// Final reports whether the status is terminal.
func (s OperationStatus) Final() bool {
	return s == OpSucceeded || s == OpFailed || s == OpCancelled
}

// Operation tracks a long-running request so the API never blocks on LXD.
type Operation struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       OperationStatus `json:"status"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	HostID       string          `json:"host_id,omitempty"`
	Actor        string          `json:"actor,omitempty"`
	Stage        string          `json:"stage,omitempty"`
	// Progress is 0-100 when known, -1 when LXD reports none.
	Progress       int            `json:"progress"`
	ProgressDetail string         `json:"progress_detail,omitempty"`
	LXDOperation   string         `json:"lxd_operation,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty"`
	ErrorMessage   string         `json:"error_message,omitempty"`
	Result         map[string]any `json:"result,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty"`
}
