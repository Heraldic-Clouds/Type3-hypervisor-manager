package lxdclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/canonical/lxd/shared/api"
)

func TestParseEndpoint(t *testing.T) {
	for ep, want := range map[string]string{
		"https://10.0.0.1:8443": "https", "https://lxd.internal": "https",
		"unix:///var/snap/lxd/common/lxd/unix.socket": "unix", "unix://": "unix",
	} {
		got, _, err := ParseEndpoint(ep)
		if err != nil || got != want {
			t.Errorf("ParseEndpoint(%q) = %q %v", ep, got, err)
		}
	}
	for _, bad := range []string{"http://10.0.0.1:8443", "https://u:p@h:8443", "https://h:8443/1.0", "ftp://x", "https://"} {
		if _, _, err := ParseEndpoint(bad); err == nil {
			t.Errorf("ParseEndpoint(%q) accepted", bad)
		}
	}
}

func TestConnectRequiresTrustMaterial(t *testing.T) {
	_, err := TLSConnector{}.Connect(context.Background(), Target{Endpoint: "https://10.0.0.1:8443"})
	if err == nil {
		t.Fatal("connected without client certificate")
	}
	_, err = TLSConnector{}.Connect(context.Background(), Target{Endpoint: "https://10.0.0.1:8443", ClientCertPEM: "c", ClientKeyPEM: "k"})
	if err == nil {
		t.Fatal("connected without a pinned server certificate or CA")
	}
}

func TestIsTransport(t *testing.T) {
	if !IsTransport(&url.Error{Op: "Get", URL: "x", Err: errors.New("refused")}) {
		t.Error("url.Error is transport")
	}
	if !IsTransport(&net.OpError{Op: "dial", Err: errors.New("refused")}) {
		t.Error("net.OpError is transport")
	}
	if IsTransport(api.StatusErrorf(http.StatusNotFound, "not found")) || IsTransport(errors.New("x")) || IsTransport(nil) {
		t.Error("API errors are not transport")
	}
}

func TestPollUntilDone(t *testing.T) {
	PollInterval = time.Millisecond
	n := 0
	err := PollUntilDone(context.Background(), func() (api.Operation, error) {
		n++
		if n < 3 {
			return api.Operation{StatusCode: api.Running}, nil
		}
		return api.Operation{StatusCode: api.Success}, nil
	}, nil)
	if err != nil || n != 3 {
		t.Fatalf("PollUntilDone = %v after %d", err, n)
	}
	err = PollUntilDone(context.Background(), func() (api.Operation, error) {
		return api.Operation{StatusCode: api.Failure, Err: "disk full"}, nil
	}, nil)
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("failure = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	err = PollUntilDone(ctx, func() (api.Operation, error) { return api.Operation{StatusCode: api.Running}, nil }, nil)
	if !errors.Is(err, ErrOperationTimeout) {
		t.Fatalf("timeout = %v", err)
	}
}
