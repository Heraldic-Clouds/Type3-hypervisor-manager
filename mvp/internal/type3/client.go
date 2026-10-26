// Package type3 is MEA's client for the Type3 orchestration API on the GPU
// host. Type3 is reachable only on the host's private address from the VPC
// control plane; MEA pins the host's self-signed certificate by SHA-256
// fingerprint and authenticates with an admin API key from Secrets Manager.
package type3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	Base        string // https://10.84.0.x:7443
	Key         string
	Fingerprint string // lowercase hex SHA-256 of the leaf certificate
	HTTP        *http.Client
}

// Pinned returns an HTTP client that accepts only the pinned certificate.
func Pinned(fingerprint string, timeout time.Duration) *http.Client {
	want := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Chain verification is replaced by an exact pin of the host's own
		// certificate, published by the host at boot through SSM.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || want == "" {
				return errors.New("type3 certificate pin missing")
			}
			sum := sha256.Sum256(raw[0])
			if hex.EncodeToString(sum[:]) != want {
				return errors.New("type3 certificate does not match the pinned fingerprint")
			}
			return nil
		},
	}}}
}

// APIError is a Type3 error response.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e APIError) Error() string { return fmt.Sprintf("type3 %d %s: %s", e.Status, e.Code, e.Message) }

func IsNotFound(err error) bool {
	var e APIError
	return errors.As(err, &e) && e.Status == 404
}

func (c *Client) do(ctx context.Context, method, path string, body any, idem string, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	h := c.HTTP
	if h == nil {
		h = Pinned(c.Fingerprint, 15*time.Second)
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string }
		}
		_ = json.Unmarshal(raw, &e)
		return APIError{resp.StatusCode, e.Error.Code, e.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

type Host struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
	Capacity struct {
		CPUThreads  uint64 `json:"cpu_threads"`
		MemoryBytes uint64 `json:"memory_bytes"`
		NvidiaGPUs  int    `json:"nvidia_gpus"`
	} `json:"capacity"`
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, "GET", "/api/v1/health", nil, "", nil)
}
func (c *Client) Hosts(ctx context.Context) ([]Host, error) {
	var out struct{ Hosts []Host }
	err := c.do(ctx, "GET", "/api/v1/hosts", nil, "", &out)
	return out.Hosts, err
}

// EnsureLocalHost registers the co-located LXD daemon over its unix socket.
func (c *Client) EnsureLocalHost(ctx context.Context, name string) (Host, error) {
	hs, err := c.Hosts(ctx)
	if err != nil {
		return Host{}, err
	}
	for _, h := range hs {
		if h.Name == name {
			return h, nil
		}
	}
	var h Host
	err = c.do(ctx, "POST", "/api/v1/hosts", map[string]any{"name": name, "endpoint": "unix:///var/snap/lxd/common/lxd/unix.socket", "provider": "aws-ec2", "schedulable": true}, "", &h)
	return h, err
}

type Resources struct {
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memory_mib"`
	DiskGiB   int `json:"disk_gib"`
}
type Container struct {
	ID        string    `json:"id"`
	HostID    string    `json:"host_id"`
	OwnerID   string    `json:"owner_id"`
	Tier      string    `json:"tier"`
	Status    string    `json:"status"`
	GPU       bool      `json:"gpu"`
	Resources Resources `json:"resources"`
	IPv4      []string  `json:"ipv4"`
	RDPPort   int       `json:"rdp_port"`
	HomeReady bool      `json:"home_ready"`
	Error     string    `json:"error"`
	Stale     bool      `json:"stale"`
	ActiveOp  *struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Status string `json:"status"`
		Stage  string `json:"stage"`
	} `json:"active_operation"`
}
type Operation struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	Stage        string `json:"stage"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}
type CreateRequest struct {
	Name           string    `json:"name"`
	OwnerID        string    `json:"owner_id"`
	Tier           string    `json:"tier"`
	HostID         string    `json:"host_id,omitempty"`
	Resources      Resources `json:"resources"`
	RDPPort        int       `json:"rdp_port"`
	PersistentHome bool      `json:"persistent_home"`
}

func (c *Client) Create(ctx context.Context, r CreateRequest, idem string) (Container, Operation, error) {
	var out struct {
		Container Container
		Operation Operation
	}
	err := c.do(ctx, "POST", "/api/v1/containers", r, idem, &out)
	return out.Container, out.Operation, err
}
func (c *Client) Container(ctx context.Context, id string) (Container, error) {
	var out Container
	err := c.do(ctx, "GET", "/api/v1/containers/"+url.PathEscape(id)+"?live=true", nil, "", &out)
	return out, err
}
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var out struct{ Containers []Container }
	err := c.do(ctx, "GET", "/api/v1/containers", nil, "", &out)
	return out.Containers, err
}
func (c *Client) Action(ctx context.Context, id, action string, body any, idem string) (Operation, error) {
	var op Operation
	switch action {
	case "start", "stop", "restart":
	default:
		return op, fmt.Errorf("unsupported action %q", action)
	}
	if body == nil {
		body = map[string]any{}
	}
	err := c.do(ctx, "POST", "/api/v1/containers/"+url.PathEscape(id)+"/"+action, body, idem, &op)
	return op, err
}
func (c *Client) Delete(ctx context.Context, id, reason string) (Operation, error) {
	var op Operation
	err := c.do(ctx, "DELETE", "/api/v1/containers/"+url.PathEscape(id)+"?force=true&reason="+url.QueryEscape(reason), nil, "", &op)
	return op, err
}
func (c *Client) SetPassword(ctx context.Context, id, password string) error {
	return c.do(ctx, "POST", "/api/v1/containers/"+url.PathEscape(id)+"/credentials", map[string]string{"password": password}, "", nil)
}
func (c *Client) Resize(ctx context.Context, id string, r Resources) (Operation, error) {
	var op Operation
	err := c.do(ctx, "PATCH", "/api/v1/containers/"+url.PathEscape(id)+"/resources", r, "", &op)
	return op, err
}
func (c *Client) Logs(ctx context.Context, id string) (string, error) {
	var out struct{ Log string }
	err := c.do(ctx, "GET", "/api/v1/containers/"+url.PathEscape(id)+"/logs?lines=200", nil, "", &out)
	return out.Log, err
}
func (c *Client) Snapshots(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, "GET", "/api/v1/containers/"+url.PathEscape(id)+"/snapshots", nil, "", &out)
	return out, err
}
func (c *Client) Snapshot(ctx context.Context, id, name string) (Operation, error) {
	var op Operation
	err := c.do(ctx, "POST", "/api/v1/containers/"+url.PathEscape(id)+"/snapshots", map[string]any{"name": name}, "", &op)
	return op, err
}
func (c *Client) Telemetry(ctx context.Context, host string, storage bool) (json.RawMessage, error) {
	var out json.RawMessage
	q := ""
	if storage {
		q = "?storage=true"
	}
	err := c.do(ctx, "GET", "/api/v1/hosts/"+url.PathEscape(host)+"/telemetry"+q, nil, "", &out)
	return out, err
}
func (c *Client) Operation(ctx context.Context, id string) (Operation, error) {
	var op Operation
	err := c.do(ctx, "GET", "/api/v1/operations/"+url.PathEscape(id), nil, "", &op)
	return op, err
}
