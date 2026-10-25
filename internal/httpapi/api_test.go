package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/canonical/lxd/shared/api"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/audit"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/auth"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/config"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/containers"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/hosts"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/images"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/logging"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/lxdclient/fake"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/metrics"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/model"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/operations"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/profiles"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/store"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/internal/testutil"
)

func init() { lxdclient.PollInterval = 10 * time.Millisecond }

const (
	adminKey    = "t3_admin_key_for_tests_only"
	operatorKey = "t3_operator_key_for_tests_only"
	viewerKey   = "t3_viewer_key_for_tests_only"
)

type apiEnv struct {
	t          *testing.T
	srv        *httptest.Server
	cpu        *fake.Server
	auditBuf   *bytes.Buffer
	serverCert string
	conn       *fake.Connector
}

func newAPI(t *testing.T) *apiEnv {
	t.Helper()
	certPath, keyPath := testutil.ClientIdentityFiles(t)
	serverCert, _ := testutil.SelfSignedCert(t, "lxd")
	st := store.NewMemoryStore()
	conn := fake.NewConnector()
	cpu := fake.New("node1")
	conn.Add("https://10.0.0.1:8443", cpu)
	log := logging.Discard()
	reg := hosts.NewRegistry(st, conn, nil, hosts.Options{
		ClientCertRef: certPath, ClientKeyRef: keyPath, RequestTimeout: 2 * time.Second, FailureThreshold: 1,
		DefaultNetwork: "lxdbr0", DefaultStoragePool: "default", CPUOvercommit: 4, MemoryOvercommit: 1,
	}, log)
	ops := operations.NewManager(st, operations.Options{DefaultTimeout: 5 * time.Second}, log)
	im := images.NewManager(reg, ops, nil, images.Options{
		Servers:       map[string]config.ImageServer{"ubuntu": {URL: "https://cloud-images.ubuntu.com/releases/", Protocol: "simplestreams"}},
		DefaultServer: "ubuntu",
	}, log)
	cat, _ := profiles.NewCatalog(profiles.DefaultTiers())
	cm := containers.NewManager(st, reg, im, ops, cat, containers.Options{GracefulStopTimeout: time.Second}, log)
	authn, err := auth.New([]auth.Key{
		{Name: "admin", Role: "admin", SHA256: auth.HashKey(adminKey)},
		{Name: "ops", Role: "operator", SHA256: auth.HashKey(operatorKey)},
		{Name: "ro", Role: "viewer", SHA256: auth.HashKey(viewerKey)},
	})
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	al, _ := audit.Open("", buf)
	h := New(Deps{Hosts: reg, Containers: cm, Images: im, Ops: ops, Catalog: cat, Auth: authn,
		Audit: al, Metrics: metrics.New(), Store: st, Log: log})
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		_ = ops.Shutdown(context.Background())
	})
	return &apiEnv{t: t, srv: srv, cpu: cpu, auditBuf: buf, serverCert: serverCert, conn: conn}
}

type resp struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (e *apiEnv) do(method, path, key string, body any, hdr ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		if s, ok := body.(string); ok {
			rd = strings.NewReader(s)
		} else {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: string(raw), header: res.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (e *apiEnv) waitOp(id string) map[string]any {
	e.t.Helper()
	for i := 0; i < 500; i++ {
		r := e.do("GET", "/api/v1/operations/"+id, viewerKey, nil)
		if s, _ := r.body["status"].(string); model.OperationStatus(s).Final() {
			return r.body
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("operation %s did not finish", id)
	return nil
}

func opID(r resp) string {
	op, _ := r.body["operation"].(map[string]any)
	id, _ := op["id"].(string)
	return id
}

func errCode(r resp) string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (e *apiEnv) registerHost() {
	e.t.Helper()
	r := e.do("POST", "/api/v1/hosts", adminKey, map[string]any{"name": "node1", "endpoint": "https://10.0.0.1:8443", "server_cert": e.serverCert})
	if r.status != http.StatusCreated {
		e.t.Fatalf("register host: %d %s", r.status, r.raw)
	}
}

func TestHealthIsPublic(t *testing.T) {
	e := newAPI(t)
	r := e.do("GET", "/api/v1/health", "", nil)
	if r.status != 200 || r.body["status"] != "ok" {
		t.Fatalf("health = %d %s", r.status, r.raw)
	}
	if r.header.Get("X-Request-ID") == "" || r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing headers: %v", r.header)
	}
}

func TestUnauthorizedAccess(t *testing.T) {
	e := newAPI(t)
	for _, tc := range []struct {
		method, path, key string
		want              int
	}{
		{"GET", "/api/v1/hosts", "", 401},
		{"GET", "/api/v1/hosts", "wrong-key", 401},
		{"GET", "/api/v1/metrics", "", 401},
		{"POST", "/api/v1/hosts", viewerKey, 403},
		{"POST", "/api/v1/hosts", operatorKey, 403},
		{"POST", "/api/v1/containers", viewerKey, 403},
		{"DELETE", "/api/v1/containers/x1", viewerKey, 403},
		{"POST", "/api/v1/images/import", operatorKey, 403},
		{"DELETE", "/api/v1/hosts/node1", operatorKey, 403},
		{"GET", "/api/v1/hosts", viewerKey, 200},
	} {
		r := e.do(tc.method, tc.path, tc.key, nil)
		if r.status != tc.want {
			t.Errorf("%s %s with %q = %d, want %d (%s)", tc.method, tc.path, tc.key, r.status, tc.want, r.raw)
		}
		if tc.want >= 400 {
			if errCode(r) == "" {
				t.Errorf("%s %s: error body missing code: %s", tc.method, tc.path, r.raw)
			}
		}
	}
	if !strings.Contains(e.auditBuf.String(), `"outcome":"denied"`) {
		t.Error("denied requests should be audited")
	}
	// The raw key must never appear in logs.
	if strings.Contains(e.auditBuf.String(), "wrong-key") {
		t.Error("audit log contains a raw key")
	}
}

func TestErrorShapeAndValidation(t *testing.T) {
	e := newAPI(t)
	r := e.do("POST", "/api/v1/hosts", adminKey, `{"name":"n1","bogus":1}`)
	if r.status != 400 || errCode(r) != "invalid_json" {
		t.Fatalf("unknown field: %d %s", r.status, r.raw)
	}
	e2 := r.body["error"].(map[string]any)
	if e2["request_id"] == "" || e2["message"] == "" {
		t.Fatalf("error shape: %s", r.raw)
	}
	r = e.do("POST", "/api/v1/hosts", adminKey, `{"name":`)
	if r.status != 400 {
		t.Fatalf("bad json: %d", r.status)
	}
	r = e.do("GET", "/api/v1/nope", adminKey, nil)
	if r.status != 404 || errCode(r) != "route_not_found" {
		t.Fatalf("unknown route: %d %s", r.status, r.raw)
	}
	r = e.do("GET", "/api/v1/containers/does-not-exist", viewerKey, nil)
	if r.status != 404 || errCode(r) != "container_not_found" {
		t.Fatalf("missing container: %d %s", r.status, r.raw)
	}
	r = e.do("POST", "/api/v1/containers", operatorKey, map[string]any{"name": "ok-1", "owner_id": "c", "tier": "power"}, "Idempotency-Key", "bad key!")
	if r.status != 400 || errCode(r) != "invalid_idempotency_key" {
		t.Fatalf("bad idempotency key: %d %s", r.status, r.raw)
	}
}

func TestHostEndpoints(t *testing.T) {
	e := newAPI(t)
	e.cpu.AddNvidiaGPU("RTX 4090")
	e.registerHost()

	r := e.do("POST", "/api/v1/hosts", adminKey, map[string]any{"name": "node1", "endpoint": "https://10.0.0.1:8443", "server_cert": e.serverCert})
	if r.status != 409 {
		t.Fatalf("duplicate host: %d", r.status)
	}
	r = e.do("POST", "/api/v1/hosts", adminKey, map[string]any{"name": "pub", "endpoint": "https://1.1.1.1:8443", "server_cert": e.serverCert})
	if r.status != 400 || errCode(r) != "public_endpoint_rejected" {
		t.Fatalf("public endpoint: %d %s", r.status, r.raw)
	}
	r = e.do("GET", "/api/v1/hosts", viewerKey, nil)
	if hs := r.body["hosts"].([]any); len(hs) != 1 {
		t.Fatalf("hosts = %s", r.raw)
	}
	r = e.do("GET", "/api/v1/hosts/node1/resources", viewerKey, nil)
	if r.status != 200 || r.body["gpu_capable"] != true {
		t.Fatalf("resources: %d %s", r.status, r.raw)
	}
	cpu := r.body["cpu"].(map[string]any)
	if cpu["threads"].(float64) != 8 {
		t.Fatalf("cpu = %v", cpu)
	}
	r = e.do("GET", "/api/v1/hosts/node1/health", viewerKey, nil)
	if r.status != 200 || r.body["status"] != "online" {
		t.Fatalf("health: %d %s", r.status, r.raw)
	}
	e.cpu.SetDown(true)
	r = e.do("GET", "/api/v1/hosts/node1/health", viewerKey, nil)
	if r.status != 503 || r.body["status"] != "offline" {
		t.Fatalf("offline health: %d %s", r.status, r.raw)
	}
	e.cpu.SetDown(false)
	r = e.do("DELETE", "/api/v1/hosts/node1", adminKey, nil)
	if r.status != 200 {
		t.Fatalf("unregister: %d %s", r.status, r.raw)
	}
	if !strings.Contains(e.auditBuf.String(), `"action":"host.unregister"`) {
		t.Error("host.unregister not audited")
	}
}

func TestContainerLifecycleOverAPI(t *testing.T) {
	e := newAPI(t)
	e.registerHost()
	body := map[string]any{"name": "web-1", "owner_id": "cust-42", "tier": "power", "resources": map[string]any{"memory_mib": 4096}}

	r := e.do("POST", "/api/v1/containers", operatorKey, body, "Idempotency-Key", "create-web-1-abc")
	if r.status != 202 {
		t.Fatalf("create: %d %s", r.status, r.raw)
	}
	if op := e.waitOp(opID(r)); op["status"] != "succeeded" {
		t.Fatalf("create op: %v", op)
	}
	again := e.do("POST", "/api/v1/containers", operatorKey, body, "Idempotency-Key", "create-web-1-abc")
	if again.status != 200 || again.body["existing"] != true {
		t.Fatalf("duplicate create: %d %s", again.status, again.raw)
	}

	r = e.do("GET", "/api/v1/containers/web-1", viewerKey, nil)
	if r.status != 200 || r.body["status"] != "running" || r.body["host_id"] != "node1" {
		t.Fatalf("get: %d %s", r.status, r.raw)
	}
	if res := r.body["resources"].(map[string]any); res["memory_mib"].(float64) != 4096 || res["cpu"].(float64) != 2 {
		t.Fatalf("resources = %v", res)
	}
	if ips := r.body["ipv4"].([]any); len(ips) != 1 {
		t.Fatalf("ipv4 = %v", r.body["ipv4"])
	}

	r = e.do("POST", "/api/v1/containers/web-1/stop", operatorKey, map[string]any{"force": true, "reason": "x"})
	if r.status != 403 {
		t.Fatalf("operator forced stop: %d", r.status)
	}
	r = e.do("POST", "/api/v1/containers/web-1/stop", operatorKey, nil)
	if r.status != 202 || r.header.Get("Location") == "" {
		t.Fatalf("stop: %d %s", r.status, r.raw)
	}
	e.waitOp(opID(r))
	if r = e.do("GET", "/api/v1/containers/web-1", viewerKey, nil); r.body["status"] != "stopped" {
		t.Fatalf("after stop: %s", r.raw)
	}
	r = e.do("POST", "/api/v1/containers/web-1/start", operatorKey, nil)
	e.waitOp(opID(r))
	r = e.do("POST", "/api/v1/containers/web-1/restart", operatorKey, nil)
	if op := e.waitOp(opID(r)); op["status"] != "succeeded" {
		t.Fatalf("restart: %v", op)
	}

	r = e.do("PATCH", "/api/v1/containers/web-1/resources", operatorKey, map[string]any{"cpu": 3})
	if op := e.waitOp(opID(r)); op["status"] != "succeeded" {
		t.Fatalf("patch: %v", op)
	}
	r = e.do("PATCH", "/api/v1/containers/web-1/resources", operatorKey, map[string]any{"cpu": 99})
	if r.status != 400 || errCode(r) != "resource_limit_exceeded" {
		t.Fatalf("over limit: %d %s", r.status, r.raw)
	}

	r = e.do("POST", "/api/v1/containers/web-1/snapshots", operatorKey, map[string]any{"name": "s1"})
	e.waitOp(opID(r))
	r = e.do("GET", "/api/v1/containers/web-1/snapshots", viewerKey, nil)
	if snaps := r.body["snapshots"].([]any); len(snaps) != 1 {
		t.Fatalf("snapshots = %s", r.raw)
	}
	r = e.do("POST", "/api/v1/containers/web-1/snapshots/s1/restore", operatorKey, nil)
	if op := e.waitOp(opID(r)); op["status"] != "succeeded" {
		t.Fatalf("restore: %v", op)
	}

	r = e.do("DELETE", "/api/v1/containers/web-1", operatorKey, nil)
	if r.status != 409 || errCode(r) != "container_running" {
		t.Fatalf("delete running: %d %s", r.status, r.raw)
	}
	r = e.do("DELETE", "/api/v1/containers/web-1?force=true&reason=cleanup", operatorKey, nil)
	if r.status != 403 {
		t.Fatalf("operator force delete: %d", r.status)
	}
	r = e.do("DELETE", "/api/v1/containers/web-1", adminKey, map[string]any{"force": true, "reason": "test cleanup"})
	if op := e.waitOp(opID(r)); op["status"] != "succeeded" {
		t.Fatalf("delete: %v", op)
	}
	if r = e.do("GET", "/api/v1/containers/web-1", viewerKey, nil); r.status != 404 {
		t.Fatalf("after delete: %d", r.status)
	}
	log := e.auditBuf.String()
	for _, action := range []string{"container.create", "container.stop", "container.delete", "container.update_resources"} {
		if !strings.Contains(log, `"action":"`+action+`"`) {
			t.Errorf("%s not audited", action)
		}
	}
	if !strings.Contains(log, `"reason":"test cleanup"`) {
		t.Error("delete reason not audited")
	}
}

func TestGPUDeniedForPowerOverAPI(t *testing.T) {
	e := newAPI(t)
	e.registerHost()
	r := e.do("POST", "/api/v1/containers", operatorKey, map[string]any{"name": "p-1", "owner_id": "c", "tier": "power", "gpu": true})
	if r.status != 403 || errCode(r) != "gpu_not_allowed" {
		t.Fatalf("power+gpu: %d %s", r.status, r.raw)
	}
	r = e.do("POST", "/api/v1/containers", operatorKey, map[string]any{"name": "e-1", "owner_id": "c", "tier": "eleet"})
	if r.status != 409 || errCode(r) != "no_host_available" {
		t.Fatalf("eleet without GPU host: %d %s", r.status, r.raw)
	}
	r = e.do("POST", "/api/v1/containers", operatorKey, map[string]any{"name": "img-1", "owner_id": "c", "tier": "power", "image": map[string]any{"alias": "ubuntu/24.04"}})
	if r.status != 403 {
		t.Fatalf("operator image override: %d", r.status)
	}
}

func TestImagesOverAPI(t *testing.T) {
	e := newAPI(t)
	e.registerHost()
	r := e.do("GET", "/api/v1/images?host_id=node1", viewerKey, nil)
	if imgs := r.body["images"].([]any); r.status != 200 || len(imgs) != 1 {
		t.Fatalf("list: %d %s", r.status, r.raw)
	}
	want := fake.RemoteFingerprint("https://cloud-images.ubuntu.com/releases/", "24.04")
	r = e.do("POST", "/api/v1/images/import", adminKey, map[string]any{
		"host_id": "node1", "alias": "24.04", "aliases": []string{"mea-base"}, "expected_fingerprint": want[:16],
	})
	if r.status != 202 {
		t.Fatalf("import: %d %s", r.status, r.raw)
	}
	op := e.waitOp(opID(r))
	if op["status"] != "succeeded" || op["result"].(map[string]any)["fingerprint"] != want {
		t.Fatalf("import op: %v", op)
	}
	r = e.do("POST", "/api/v1/images/import", adminKey, map[string]any{
		"host_id": "node1", "alias": "22.04", "aliases": []string{"old"}, "expected_fingerprint": strings.Repeat("0", 64),
	})
	op = e.waitOp(opID(r))
	if op["status"] != "failed" || op["error_code"] != "fingerprint_mismatch" {
		t.Fatalf("mismatch op: %v", op)
	}
	r = e.do("GET", "/api/v1/images?host_id=node1", viewerKey, nil)
	if imgs := r.body["images"].([]any); len(imgs) != 2 {
		t.Fatalf("mismatched image should be rolled back: %s", r.raw)
	}

	// Create a container from the imported image, then refuse to delete it while in use.
	e.cpu.InjectInstance(fakeInstanceUsing("user-1", want))
	r = e.do("DELETE", "/api/v1/images/"+want+"?host_id=node1", adminKey, nil)
	if r.status != 409 || errCode(r) != "image_in_use" {
		t.Fatalf("delete in-use image: %d %s", r.status, r.raw)
	}
	r = e.do("DELETE", "/api/v1/images/"+want+"?host_id=node1&force=true", adminKey, nil)
	if op := e.waitOp(opID(r)); op["status"] != "succeeded" {
		t.Fatalf("force delete image: %v", op)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	e := newAPI(t)
	e.do("GET", "/api/v1/hosts", viewerKey, nil)
	r := e.do("GET", "/api/v1/metrics", viewerKey, nil)
	if r.status != 200 || !strings.Contains(r.raw, `type3_http_requests_total{code="200",route="GET /api/v1/hosts"} 1`) {
		t.Fatalf("metrics: %d\n%s", r.status, r.raw)
	}
}

func fakeInstanceUsing(name, fp string) api.Instance {
	return api.Instance{Name: name, Config: map[string]string{"volatile.base_image": fp}}
}
