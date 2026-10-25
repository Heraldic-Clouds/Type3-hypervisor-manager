// Package config loads Type3 configuration from environment variables, with an
// optional .env file for local development. No secret values are ever held in
// configuration directly: certificates and keys are referenced by path or URI
// and resolved by package secrets.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the complete runtime configuration.
type Config struct {
	ListenAddr  string
	TLSCertFile string
	TLSKeyFile  string

	StateFile    string
	ProfilesFile string

	APIKeysFile string
	// APIKeys holds "name:role:sha256hex" entries; only hashes, never raw keys.
	APIKeys []string

	LogLevel     string
	LogFormat    string
	AuditLogFile string

	// LXD client identity as secret references (path, file://, env://, s3://).
	LXDClientCertRef string
	LXDClientKeyRef  string
	LXDCACertRef     string

	LXDRequestTimeout     time.Duration
	OperationTimeout      time.Duration
	ShutdownTimeout       time.Duration
	HealthInterval        time.Duration
	ReconcileInterval     time.Duration
	OperationRetention    time.Duration
	GracefulStopTimeout   time.Duration
	HostFailureThreshold  int
	AllowPublicEndpoints  bool
	AllowUnixSocket       bool
	DefaultNetwork        string
	DefaultStoragePool    string
	CPUOvercommit         float64
	MemoryOvercommit      float64
	MaxContainersPerGPU   int
	ImageServers          map[string]ImageServer
	DefaultImageServer    string
	AWSRegion             string
	IdmapIsolated         bool
	NICPortIsolation      bool
	MaxRequestBodyBytes   int64
	HTTPReadHeaderTimeout time.Duration
}

// ImageServer is a remote image source LXD can pull from.
type ImageServer struct {
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
}

// Load reads configuration from the environment. If dotenvPath exists its
// variables are applied first without overriding variables already set.
func Load(dotenvPath string) (*Config, error) {
	if dotenvPath != "" {
		if err := loadDotenv(dotenvPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", dotenvPath, err)
		}
	}

	var errs []error
	e := envReader{errs: &errs}

	cfg := &Config{
		ListenAddr:            e.str("TYPE3_LISTEN_ADDR", "127.0.0.1:8080"),
		TLSCertFile:           e.str("TYPE3_TLS_CERT_FILE", ""),
		TLSKeyFile:            e.str("TYPE3_TLS_KEY_FILE", ""),
		StateFile:             e.str("TYPE3_STATE_FILE", "./data/state.json"),
		ProfilesFile:          e.str("TYPE3_PROFILES_FILE", ""),
		APIKeysFile:           e.str("TYPE3_API_KEYS_FILE", ""),
		APIKeys:               e.list("TYPE3_API_KEYS"),
		LogLevel:              e.str("TYPE3_LOG_LEVEL", "info"),
		LogFormat:             e.str("TYPE3_LOG_FORMAT", "json"),
		AuditLogFile:          e.str("TYPE3_AUDIT_LOG_FILE", ""),
		LXDClientCertRef:      e.str("TYPE3_LXD_CLIENT_CERT", ""),
		LXDClientKeyRef:       e.str("TYPE3_LXD_CLIENT_KEY", ""),
		LXDCACertRef:          e.str("TYPE3_LXD_CA_CERT", ""),
		LXDRequestTimeout:     e.dur("TYPE3_LXD_REQUEST_TIMEOUT", 30*time.Second),
		OperationTimeout:      e.dur("TYPE3_OPERATION_TIMEOUT", 15*time.Minute),
		ShutdownTimeout:       e.dur("TYPE3_SHUTDOWN_TIMEOUT", 30*time.Second),
		HealthInterval:        e.dur("TYPE3_HEALTH_INTERVAL", 30*time.Second),
		ReconcileInterval:     e.dur("TYPE3_RECONCILE_INTERVAL", 5*time.Minute),
		OperationRetention:    e.dur("TYPE3_OPERATION_RETENTION", 24*time.Hour),
		GracefulStopTimeout:   e.dur("TYPE3_GRACEFUL_STOP_TIMEOUT", 60*time.Second),
		HostFailureThreshold:  e.int("TYPE3_HOST_FAILURE_THRESHOLD", 3),
		AllowPublicEndpoints:  e.bool("TYPE3_ALLOW_PUBLIC_LXD_ENDPOINTS", false),
		AllowUnixSocket:       e.bool("TYPE3_ALLOW_UNIX_SOCKET", true),
		DefaultNetwork:        e.str("TYPE3_DEFAULT_NETWORK", "lxdbr0"),
		DefaultStoragePool:    e.str("TYPE3_DEFAULT_STORAGE_POOL", "default"),
		CPUOvercommit:         e.float("TYPE3_CPU_OVERCOMMIT", 4.0),
		MemoryOvercommit:      e.float("TYPE3_MEMORY_OVERCOMMIT", 1.0),
		MaxContainersPerGPU:   e.int("TYPE3_MAX_CONTAINERS_PER_GPU", 4),
		DefaultImageServer:    e.str("TYPE3_DEFAULT_IMAGE_SERVER", "ubuntu"),
		AWSRegion:             e.str("AWS_REGION", ""),
		IdmapIsolated:         e.bool("TYPE3_IDMAP_ISOLATED", true),
		NICPortIsolation:      e.bool("TYPE3_NIC_PORT_ISOLATION", true),
		MaxRequestBodyBytes:   int64(e.int("TYPE3_MAX_REQUEST_BODY_BYTES", 1<<20)),
		HTTPReadHeaderTimeout: e.dur("TYPE3_HTTP_READ_HEADER_TIMEOUT", 10*time.Second),
		ImageServers: map[string]ImageServer{
			"ubuntu":         {URL: "https://cloud-images.ubuntu.com/releases/", Protocol: "simplestreams"},
			"ubuntu-minimal": {URL: "https://cloud-images.ubuntu.com/minimal/releases/", Protocol: "simplestreams"},
		},
	}

	// TYPE3_IMAGE_SERVERS=name=https://url,name2=https://url2 (simplestreams).
	for _, item := range e.list("TYPE3_IMAGE_SERVERS") {
		name, url, ok := strings.Cut(item, "=")
		if !ok || name == "" || url == "" {
			errs = append(errs, fmt.Errorf("TYPE3_IMAGE_SERVERS: invalid entry %q, want name=url", item))
			continue
		}
		cfg.ImageServers[name] = ImageServer{URL: url, Protocol: "simplestreams"}
	}

	if err := cfg.validate(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var errs []error
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("TYPE3_LISTEN_ADDR: %w", err))
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("TYPE3_TLS_CERT_FILE and TYPE3_TLS_KEY_FILE must be set together"))
	}
	if (c.LXDClientCertRef == "") != (c.LXDClientKeyRef == "") {
		errs = append(errs, errors.New("TYPE3_LXD_CLIENT_CERT and TYPE3_LXD_CLIENT_KEY must be set together"))
	}
	if c.StateFile == "" {
		errs = append(errs, errors.New("TYPE3_STATE_FILE must not be empty"))
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("TYPE3_LOG_FORMAT: %q, want json or text", c.LogFormat))
	}
	for name, d := range map[string]time.Duration{
		"TYPE3_LXD_REQUEST_TIMEOUT": c.LXDRequestTimeout,
		"TYPE3_OPERATION_TIMEOUT":   c.OperationTimeout,
		"TYPE3_SHUTDOWN_TIMEOUT":    c.ShutdownTimeout,
		"TYPE3_HEALTH_INTERVAL":     c.HealthInterval,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	if c.HostFailureThreshold < 1 {
		errs = append(errs, errors.New("TYPE3_HOST_FAILURE_THRESHOLD must be >= 1"))
	}
	if c.MaxContainersPerGPU < 1 {
		errs = append(errs, errors.New("TYPE3_MAX_CONTAINERS_PER_GPU must be >= 1"))
	}
	if c.CPUOvercommit <= 0 || c.MemoryOvercommit <= 0 {
		errs = append(errs, errors.New("overcommit ratios must be positive"))
	}
	if _, ok := c.ImageServers[c.DefaultImageServer]; !ok {
		errs = append(errs, fmt.Errorf("TYPE3_DEFAULT_IMAGE_SERVER %q is not a configured image server", c.DefaultImageServer))
	}
	return errors.Join(errs...)
}

// IsLoopback reports whether the listen address binds only to loopback.
func (c *Config) IsLoopback() bool {
	host, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type envReader struct{ errs *[]error }

func (e envReader) str(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return def
}

func (e envReader) list(key string) []string {
	v := e.str(key, "")
	if v == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (e envReader) dur(key string, def time.Duration) time.Duration {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		*e.errs = append(*e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

func (e envReader) int(key string, def int) int {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*e.errs = append(*e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

func (e envReader) float(key string, def float64) float64 {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*e.errs = append(*e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return f
}

func (e envReader) bool(key string, def bool) bool {
	v := e.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*e.errs = append(*e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

// loadDotenv applies KEY=VALUE lines from path without overriding variables
// that are already set. Quotes around values are stripped.
func loadDotenv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}
