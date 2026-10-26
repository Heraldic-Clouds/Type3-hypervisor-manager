package gateway

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"time"
)

// Registry maps a desktop id to its agent's link address on the LXD bridge.
type Registry interface {
	Lookup(desktop string) (string, error)
}

var ErrUnknownDesktop = errors.New("gateway: desktop not on this host")

// FileRegistry reads {"desktops":{"<id>":"10.20.0.12:47100"}} written by the
// Type3 host engine whenever it starts or stops an Eleet container. The file
// is re-read when its modification time changes.
type FileRegistry struct {
	Path string

	mu    sync.Mutex
	mod   time.Time
	table map[string]string
}

func (r *FileRegistry) Lookup(desktop string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := os.Stat(r.Path)
	if err != nil {
		return "", err
	}
	if !st.ModTime().Equal(r.mod) || r.table == nil {
		b, err := os.ReadFile(r.Path)
		if err != nil {
			return "", err
		}
		var f struct {
			Desktops map[string]string `json:"desktops"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return "", err
		}
		r.table, r.mod = f.Desktops, st.ModTime()
	}
	addr, ok := r.table[desktop]
	if !ok {
		return "", ErrUnknownDesktop
	}
	return addr, validateAgentAddr(addr)
}

// StaticRegistry is used in tests and single-desktop development.
type StaticRegistry map[string]string

func (s StaticRegistry) Lookup(desktop string) (string, error) {
	a, ok := s[desktop]
	if !ok {
		return "", ErrUnknownDesktop
	}
	return a, nil
}

// validateAgentAddr only allows private bridge addresses so a poisoned
// registry entry cannot make the gateway connect to arbitrary hosts or the
// instance metadata service.
func validateAgentAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsPrivate() || ip.IsLoopback()) || ip.IsLinkLocalUnicast() {
		return errors.New("gateway: agent address must be a private bridge address")
	}
	return nil
}
