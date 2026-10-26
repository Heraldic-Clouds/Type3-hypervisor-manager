// mea-stream-registry keeps the gateway's agent registry in step with LXD.
//
// It lists the instances on this host through the LXD API socket, picks the
// running ones tagged user.mea.desktop_id, and writes
// {"desktops":{"<desktop id>":"<container ip>:47100"}} atomically to
// MEA_REGISTRY_FILE, which the gateway re-reads on every session.
//
// Type3 sets the tag when it creates an Eleet desktop container:
//
//	lxc config set <name> user.mea.desktop_id <MEA desktop id>
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/internal/env"
)

// Instance is the part of LXD's /1.0/instances?recursion=2 we use.
type Instance struct {
	Name   string            `json:"name"`
	Status string            `json:"status"`
	Config map[string]string `json:"config"`
	State  *struct {
		Network map[string]struct {
			Addresses []struct {
				Family  string `json:"family"`
				Address string `json:"address"`
				Scope   string `json:"scope"`
			} `json:"addresses"`
		} `json:"network"`
	} `json:"state"`
}

const tag = "user.mea.desktop_id"

// Desktops maps desktop ids to agent addresses for running, tagged instances.
func Desktops(list []Instance, port int) map[string]string {
	out := map[string]string{}
	for _, in := range list {
		id := in.Config[tag]
		if id == "" || in.Status != "Running" || in.State == nil {
			continue
		}
		names := make([]string, 0, len(in.State.Network))
		for n := range in.State.Network {
			names = append(names, n)
		}
		sort.Strings(names) // eth0 before others, deterministic
		for _, n := range names {
			if n == "lo" {
				continue
			}
			for _, a := range in.State.Network[n].Addresses {
				if a.Family == "inet" && a.Scope == "global" {
					if _, ok := out[id]; !ok {
						out[id] = net.JoinHostPort(a.Address, fmt.Sprint(port))
					}
				}
			}
		}
	}
	return out
}

func lxdClient(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
}

func list(c *http.Client) ([]Instance, error) {
	res, err := c.Get("http://lxd/1.0/instances?recursion=2")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var body struct {
		Metadata []Instance `json:"metadata"`
		Error    string     `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("lxd: %s", body.Error)
	}
	return body.Metadata, nil
}

// write replaces path atomically so the gateway never reads a partial file.
func write(path string, desktops map[string]string) error {
	b, err := json.MarshalIndent(map[string]any{"desktops": desktops}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agents-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	socket := env.String("LXD_SOCKET", "/var/snap/lxd/common/lxd/unix.socket")
	path := env.String("MEA_REGISTRY_FILE", "/var/lib/type3/stream-agents.json")
	port := env.Int("MEA_AGENT_PORT", 47100)
	every := time.Duration(env.Int("MEA_REGISTRY_INTERVAL_SECONDS", 5)) * time.Second
	once := len(os.Args) > 1 && os.Args[1] == "once"

	c := lxdClient(socket)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	last := ""
	for {
		instances, err := list(c)
		if err != nil {
			log.Error("list instances", "err", err)
			if once {
				os.Exit(1)
			}
		} else {
			d := Desktops(instances, port)
			sig, _ := json.Marshal(d)
			if string(sig) != last {
				if err := write(path, d); err != nil {
					log.Error("write registry", "err", err)
				} else {
					last = string(sig)
					log.Info("registry updated", "desktops", len(d))
				}
			}
		}
		if once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
