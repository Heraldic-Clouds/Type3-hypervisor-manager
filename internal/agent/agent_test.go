package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type call struct {
	stdin string
	argv  string
}

func fake(responses map[string]string) (*Agent, *[]call) {
	calls := &[]call{}
	a := &Agent{GuestUID: 1000, HomeRoot: "/mnt/mea-efs/users"}
	a.Run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		argv := name + " " + strings.Join(args, " ")
		*calls = append(*calls, call{string(stdin), argv})
		for k, v := range responses {
			if strings.Contains(argv, k) {
				if v == "ERR" {
					return nil, errors.New("failed")
				}
				return []byte(v), nil
			}
		}
		return nil, nil
	}
	return a, calls
}

func TestSetPasswordUsesStdin(t *testing.T) {
	a, calls := fake(map[string]string{"id -u": "1000\n"})
	pw := "abcdefghijklmnopqrstuvwxyz012345"
	if err := a.SetPassword(context.Background(), "mea-power-1", "mea", pw); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if last.stdin != "mea:"+pw+"\n" || strings.Contains(last.argv, pw) {
		t.Fatalf("password must go through stdin only: %+v", last)
	}
	for _, bad := range []string{"short", "has:colon-and-is-long-enough", "new\nline-and-is-long-enough!"} {
		if a.SetPassword(context.Background(), "mea-power-1", "mea", bad) == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if a.SetPassword(context.Background(), "../etc", "mea", pw) == nil {
		t.Fatal("accepted unsafe container name")
	}
}

func TestPrepareHomeIsolation(t *testing.T) {
	a, _ := fake(map[string]string{"volatile.idmap.base": "1065536\n"})
	var made, owned string
	var uid int
	var mode os.FileMode
	a.MkdirAll = func(p string, m os.FileMode) error { made = p; return nil }
	a.Chown = func(p string, u, g int) error { owned, uid = p, u; return nil }
	a.Chmod = func(p string, m os.FileMode) error { mode = m; return nil }
	p, err := a.PrepareHome(context.Background(), "mea-eleet-1", "8664cac9-34fe-494e-a33a-afa46a195b57")
	if err != nil {
		t.Fatal(err)
	}
	if p != made || p != owned || uid != 1066536 || mode != 0o700 || !strings.HasSuffix(p, "8664cac9-34fe-494e-a33a-afa46a195b57") {
		t.Fatalf("home %s uid %d mode %o", p, uid, mode)
	}
	for _, bad := range []string{"../other", "a/b", "", ".hidden"} {
		if _, err := a.HomePath(bad); err == nil {
			t.Fatalf("owner %q accepted", bad)
		}
	}
	b, _ := fake(map[string]string{"volatile.idmap.base": "\n"})
	if _, err := b.PrepareHome(context.Background(), "mea-eleet-1", "owner"); err == nil {
		t.Fatal("prepared home without an isolated idmap")
	}
}

func TestParseSMI(t *testing.T) {
	u, err := ParseSMI("GPU-1234, 37, 2048, 23034, 12, 0, 2\nGPU-5678, [N/A], 0, 23034, [N/A], [N/A], 0\n")
	if err != nil || len(u) != 2 {
		t.Fatal(err, u)
	}
	if u[0].GPUPercent != 37 || u[0].MemoryUsedBytes != 2048<<20 || u[0].EncoderSessions != 2 || u[1].GPUPercent != 0 {
		t.Fatalf("%+v", u)
	}
	if _, err := ParseSMI("garbage"); err == nil {
		t.Fatal("garbage parsed")
	}
}
