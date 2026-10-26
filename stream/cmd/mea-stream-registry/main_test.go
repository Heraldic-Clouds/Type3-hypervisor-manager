package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const sample = `[
 {"name":"eleet-a","status":"Running","config":{"user.mea.desktop_id":"desk-a"},
  "state":{"network":{"lo":{"addresses":[{"family":"inet","address":"127.0.0.1","scope":"local"}]},
   "eth0":{"addresses":[{"family":"inet6","address":"fd42::1","scope":"global"},{"family":"inet","address":"10.10.0.5","scope":"global"}]}}}},
 {"name":"eleet-b","status":"Stopped","config":{"user.mea.desktop_id":"desk-b"},"state":null},
 {"name":"power-c","status":"Running","config":{},
  "state":{"network":{"eth0":{"addresses":[{"family":"inet","address":"10.10.0.6","scope":"global"}]}}}}
]`

func TestDesktops(t *testing.T) {
	var list []Instance
	if err := json.Unmarshal([]byte(sample), &list); err != nil {
		t.Fatal(err)
	}
	got := Desktops(list, 47100)
	if len(got) != 1 || got["desk-a"] != "10.10.0.5:47100" {
		t.Fatalf("%v", got)
	}
}

func TestWriteAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	if err := write(path, map[string]string{"d": "10.0.0.1:47100"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var v struct{ Desktops map[string]string }
	if err := json.Unmarshal(b, &v); err != nil || v.Desktops["d"] != "10.0.0.1:47100" {
		t.Fatalf("%s %v", b, err)
	}
}
