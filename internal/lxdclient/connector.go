package lxdclient

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	lxd "github.com/canonical/lxd/client"
)

func newTicker() *time.Ticker { return time.NewTicker(PollInterval) }

// Target describes how to reach one LXD daemon. PEM fields hold resolved
// secret material in memory only.
type Target struct {
	Endpoint      string
	Project       string
	ServerCertPEM string
	CACertPEM     string
	ClientCertPEM string
	ClientKeyPEM  string
	UserAgent     string
}

// Connector opens connections to LXD daemons.
type Connector interface {
	Connect(ctx context.Context, t Target) (Server, error)
}

// TLSConnector connects over HTTPS with mutual TLS, or over the local unix
// socket. Certificate verification is never disabled: the server must
// present either the pinned certificate or one signed by the configured CA.
type TLSConnector struct{}

// Connect implements Connector.
func (TLSConnector) Connect(ctx context.Context, t Target) (Server, error) {
	scheme, path, err := ParseEndpoint(t.Endpoint)
	if err != nil {
		return nil, err
	}
	var srv lxd.InstanceServer
	switch scheme {
	case "unix":
		srv, err = lxd.ConnectLXDUnixWithContext(ctx, path, &lxd.ConnectionArgs{UserAgent: t.UserAgent})
	case "https":
		if t.ClientCertPEM == "" || t.ClientKeyPEM == "" {
			return nil, errors.New("an LXD client certificate and key are required for https endpoints")
		}
		if t.ServerCertPEM == "" && t.CACertPEM == "" {
			return nil, errors.New("a pinned server certificate or a CA certificate is required for https endpoints")
		}
		srv, err = lxd.ConnectLXDWithContext(ctx, t.Endpoint, &lxd.ConnectionArgs{
			TLSClientCert: t.ClientCertPEM,
			TLSClientKey:  t.ClientKeyPEM,
			TLSServerCert: t.ServerCertPEM,
			TLSCA:         t.CACertPEM,
			UserAgent:     t.UserAgent,
		})
	}
	if err != nil {
		return nil, err
	}
	if t.Project != "" && t.Project != "default" {
		srv = srv.UseProject(t.Project)
	}
	return Wrap(srv), nil
}

// ParseEndpoint validates an LXD endpoint and returns its scheme ("https" or
// "unix") and, for unix sockets, the socket path ("" = LXD default).
func ParseEndpoint(endpoint string) (scheme, socketPath string, err error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("invalid endpoint: %w", err)
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return "", "", errors.New("https endpoint needs a host")
		}
		if u.User != nil {
			return "", "", errors.New("endpoint must not contain credentials")
		}
		if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", errors.New("endpoint must be scheme://host[:port] only")
		}
		return "https", "", nil
	case "unix":
		p := u.Path
		if u.Host != "" {
			p = "/" + u.Host + u.Path
		}
		return "unix", p, nil
	default:
		return "", "", fmt.Errorf("endpoint scheme %q not supported, use https:// or unix://", u.Scheme)
	}
}

// CertFingerprint returns the SHA-256 fingerprint of a PEM certificate, the
// same value `lxc config trust list` shows.
func CertFingerprint(certPEM string) (string, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(certPEM)))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", fmt.Errorf("parse certificate: %w", err)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}
