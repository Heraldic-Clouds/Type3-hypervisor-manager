package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/cloud"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/mail"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"testing"
)

type fakeCompute struct {
	instances                                              []cloud.Instance
	launchError                                            error
	launched, terminated, deleted, passwordRead, readyRead bool
	ready                                                  bool
}

func (f *fakeCompute) Launch(context.Context, string) (cloud.Instance, error) {
	f.launched = true
	return cloud.Instance{ID: "i-test", IP: "10.0.0.4"}, f.launchError
}
func (f *fakeCompute) Find(context.Context, string) ([]cloud.Instance, error) {
	return f.instances, nil
}
func (f *fakeCompute) Terminate(context.Context, string) error        { f.terminated = true; return nil }
func (f *fakeCompute) DeleteCredential(context.Context, string) error { f.deleted = true; return nil }
func (f *fakeCompute) Password(context.Context, string) (string, error) {
	f.passwordRead = true
	return "disposable-test", nil
}
func (f *fakeCompute) Ready(context.Context, string) (bool, error) {
	f.readyRead = true
	return f.ready, nil
}

type fakeInvoke struct {
	lookup, reconcile control.Result
	actions           []string
	ready             bool
}

func (f *fakeInvoke) Invoke(ctx context.Context, r *lambda.InvokeInput, _ ...func(*lambda.Options)) (*lambda.InvokeOutput, error) {
	var request control.Request
	_ = json.Unmarshal(r.Payload, &request)
	f.actions = append(f.actions, request.Action)
	result := control.Result{}
	if request.Action == "lookup" {
		result = f.lookup
	}
	if request.Action == "ready" {
		result.Ready = f.ready
	}
	if request.Action == "reconcile" {
		result = f.reconcile
	}
	b, _ := json.Marshal(result)
	return &lambda.InvokeOutput{Payload: b}, nil
}
func TestLaunchFailureDoesNotAssign(t *testing.T) {
	compute := &fakeCompute{launchError: errors.New("capacity unavailable")}
	db := &fakeInvoke{}
	w := worker{cloud: compute, invoke: db}
	if _, e := w.handle(context.Background(), event{Action: "launch", ID: "test"}); e == nil {
		t.Fatal("launch error swallowed")
	}
	if len(db.actions) != 1 || !compute.launched {
		t.Fatal("assigned failed launch")
	}
}
func TestCleanupWaitsForActualTermination(t *testing.T) {
	compute := &fakeCompute{instances: []cloud.Instance{{ID: "i-test"}}}
	db := &fakeInvoke{}
	w := worker{cloud: compute, invoke: db}
	if e := w.cleanup(context.Background(), "test"); e != nil {
		t.Fatal(e)
	}
	if !compute.terminated || !compute.deleted || len(db.actions) != 0 {
		t.Fatal("marked terminated before EC2 inventory confirmed deletion")
	}
	compute.instances = nil
	if e := w.cleanup(context.Background(), "test"); e != nil {
		t.Fatal(e)
	}
	if db.actions[len(db.actions)-1] != "terminated" {
		t.Fatal("cleanup did not finalize")
	}
}
func TestRetryAfterReadyDoesNotReadDeletedSecret(t *testing.T) {
	compute := &fakeCompute{}
	db := &fakeInvoke{lookup: control.Result{Ready: true}}
	w := worker{cloud: compute, invoke: db}
	r, e := w.handle(context.Background(), event{Action: "probe", ID: "test"})
	if e != nil || r["ready"] != true || compute.passwordRead || compute.readyRead {
		t.Fatal("ready retry was not idempotent")
	}
}
func TestRevokedLeaseNeverLaunches(t *testing.T) {
	compute := &fakeCompute{}
	db := &fakeInvoke{lookup: control.Result{Skip: true}}
	w := worker{cloud: compute, invoke: db}
	if _, e := w.handle(context.Background(), event{Action: "launch", ID: "test"}); e == nil || compute.launched {
		t.Fatal("revoked lease launched compute")
	}
}
func TestProbeWaitsForCredentialBootstrap(t *testing.T) {
	compute := &fakeCompute{instances: []cloud.Instance{{ID: "i-test", State: "running"}}}
	db := &fakeInvoke{ready: true}
	w := worker{cloud: compute, invoke: db}
	r, e := w.handle(context.Background(), event{Action: "probe", ID: "test"})
	if e != nil || r["ready"] != false || compute.passwordRead {
		t.Fatal("registered desktop before bootstrap")
	}
	compute.ready = true
	r, e = w.handle(context.Background(), event{Action: "probe", ID: "test"})
	if e != nil || r["ready"] != true || !compute.deleted {
		t.Fatal("ready desktop credentials were not removed")
	}
}

type fakeMail struct {
	message mail.Message
	err     error
}

func (f *fakeMail) Send(_ context.Context, m mail.Message) error {
	f.message = m
	return f.err
}

func TestMailAcknowledgedOnlyAfterTransportAcceptance(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			t.Setenv("APP_ORIGIN", "https://miseenaby.me")
			payload := json.RawMessage(`{"Email":"test@example.test","Kind":"reset","Token":"test-token"}`)
			db := &fakeInvoke{reconcile: control.Result{Mails: []control.Mail{{ID: "test-outbox", Payload: payload}}}}
			sender := &fakeMail{}
			if rejected {
				sender.err = errors.New("SMTP rejected recipient")
			}
			w := worker{cloud: &fakeCompute{}, invoke: db, mail: sender}
			if _, err := w.handle(context.Background(), event{Action: "watchdog"}); err != nil {
				t.Fatal(err)
			}
			want := "mail-sent"
			if rejected {
				want = "mail-failed"
			}
			if len(db.actions) != 3 || db.actions[1] != want || sender.message.To != "test@example.test" || sender.message.Subject != "Reset your Mise En Abyme password" {
				t.Fatal("delivery acknowledgement did not match transport result")
			}
		})
	}
}
