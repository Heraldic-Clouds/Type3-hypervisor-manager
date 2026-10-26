package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/cloud"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/mail"
	lambdaruntime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/sfn"
	"github.com/aws/smithy-go"
	"log/slog"
	"net/url"
	"os"
	"time"
)

type event struct {
	Action string          `json:"action"`
	ID     string          `json:"id"`
	Error  json.RawMessage `json:"error,omitempty"`
}
type worker struct {
	cloud  cloud.Compute
	invoke invokeAPI
	sfn    *sfn.Client
	mail   mailSender
	// gpuHost is nil when Power/Eleet are not deployed.
	gpuHost *gpuHost
}
type invokeAPI interface {
	Invoke(context.Context, *lambda.InvokeInput, ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

func (w *worker) db(ctx context.Context, r control.Request) (control.Result, error) {
	b, _ := json.Marshal(r)
	out, e := w.invoke.Invoke(ctx, &lambda.InvokeInput{FunctionName: aws.String(os.Getenv("CONTROL_FUNCTION")), Payload: b})
	if e != nil {
		return control.Result{}, e
	}
	if out.FunctionError != nil {
		return control.Result{}, fmt.Errorf("control operation %s failed", r.Action)
	}
	var result control.Result
	e = json.Unmarshal(out.Payload, &result)
	return result, e
}
func (w *worker) cleanup(ctx context.Context, id string) error {
	instances, e := w.cloud.Find(ctx, id)
	if e != nil {
		return e
	}
	allGone := true
	for _, i := range instances {
		allGone = false
		if e = w.cloud.Terminate(ctx, i.ID); e != nil {
			return e
		}
	}
	if e = w.cloud.DeleteCredential(ctx, id); e != nil {
		return e
	}
	if allGone {
		_, e = w.db(ctx, control.Request{Action: "terminated", ID: id})
	}
	return e
}
func (w *worker) handle(ctx context.Context, e event) (map[string]any, error) {
	result := map[string]any{"id": e.ID, "ready": false}
	switch e.Action {
	case "launch":
		state, err := w.db(ctx, control.Request{Action: "lookup", ID: e.ID})
		if err != nil {
			return nil, err
		}
		if state.Skip {
			return nil, fmt.Errorf("desktop lease revoked")
		}
		if state.Ready {
			result["ready"] = true
			return result, nil
		}
		instance, err := w.cloud.Launch(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		state, err = w.db(ctx, control.Request{Action: "assign", ID: e.ID, InstanceID: instance.ID, IP: instance.IP})
		if err != nil {
			return nil, err
		}
		if state.Skip {
			_ = w.cloud.Terminate(ctx, instance.ID)
			return nil, fmt.Errorf("desktop lease revoked")
		}
		return result, nil
	case "probe":
		state, err := w.db(ctx, control.Request{Action: "lookup", ID: e.ID})
		if err != nil {
			return nil, err
		}
		if state.Skip {
			return nil, fmt.Errorf("desktop lease revoked")
		}
		if state.Ready {
			result["ready"] = true
			return result, nil
		}
		instances, err := w.cloud.Find(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if len(instances) == 0 {
			return nil, fmt.Errorf("desktop instance missing")
		}
		if instances[0].State != "running" {
			return result, nil
		}
		booted, err := w.cloud.Ready(ctx, instances[0].ID)
		if err != nil {
			return nil, err
		}
		if !booted {
			return result, nil
		}
		password, err := w.cloud.Password(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		state, err = w.db(ctx, control.Request{Action: "ready", ID: e.ID, IP: instances[0].IP, Password: password})
		if err != nil {
			return nil, err
		}
		if state.Skip {
			return nil, fmt.Errorf("desktop lease revoked")
		}
		if state.Ready {
			if err = w.cloud.DeleteCredential(ctx, e.ID); err != nil {
				return nil, err
			}
		}
		result["ready"] = state.Ready
		return result, nil
	case "failure":
		if _, err := w.db(ctx, control.Request{Action: "terminating", ID: e.ID, Error: "Desktop provisioning failed. Please create a fresh desktop."}); err != nil {
			return nil, err
		}
		return result, w.cleanup(ctx, e.ID)
	case "watchdog", "":
		plan, err := w.db(ctx, control.Request{Action: "reconcile"})
		if err != nil {
			return nil, err
		}
		for _, job := range plan.Jobs {
			switch job.State {
			case "queued":
				arn := fmt.Sprintf("arn:aws:states:%s:%s:execution:%s:%s", os.Getenv("AWS_REGION"), os.Getenv("ACCOUNT_ID"), os.Getenv("STATE_MACHINE_NAME"), job.ID)
				claimed, err := w.db(ctx, control.Request{Action: "claim", ID: job.ID, ARN: arn})
				if err != nil {
					return nil, err
				}
				if claimed.Skip {
					continue
				}
				payload, _ := json.Marshal(map[string]string{"id": job.ID})
				_, err = w.sfn.StartExecution(ctx, &sfn.StartExecutionInput{StateMachineArn: aws.String(os.Getenv("STATE_MACHINE_ARN")), Name: aws.String(job.ID), Input: aws.String(string(payload))})
				if err != nil {
					var ae smithy.APIError
					if !errors.As(err, &ae) || ae.ErrorCode() != "ExecutionAlreadyExists" {
						slog.Error("start workflow failed", "desktop", job.ID)
						_, _ = w.db(ctx, control.Request{Action: "terminating", ID: job.ID, Error: "Unable to start desktop workflow"})
					}
				}
			case "provisioning":
				arn := fmt.Sprintf("arn:aws:states:%s:%s:execution:%s:%s", os.Getenv("AWS_REGION"), os.Getenv("ACCOUNT_ID"), os.Getenv("STATE_MACHINE_NAME"), job.ID)
				execution, err := w.sfn.DescribeExecution(ctx, &sfn.DescribeExecutionInput{ExecutionArn: aws.String(arn)})
				if err != nil {
					var ae smithy.APIError
					if errors.As(err, &ae) && ae.ErrorCode() == "ExecutionDoesNotExist" {
						payload, _ := json.Marshal(map[string]string{"id": job.ID})
						_, err = w.sfn.StartExecution(ctx, &sfn.StartExecutionInput{StateMachineArn: aws.String(os.Getenv("STATE_MACHINE_ARN")), Name: aws.String(job.ID), Input: aws.String(string(payload))})
					}
					if err != nil {
						slog.Error("workflow reconciliation failed", "desktop", job.ID)
					}
				} else if string(execution.Status) != "RUNNING" {
					_, _ = w.db(ctx, control.Request{Action: "terminating", ID: job.ID, Error: "Desktop workflow did not finish"})
				}
			case "terminating":
				if err = w.cleanup(ctx, job.ID); err != nil {
					slog.Error("cleanup failed", "desktop", job.ID)
				}
			}
		}
		instances, err := w.cloud.Find(ctx, "")
		if err != nil {
			return nil, err
		}
		for _, instance := range instances {
			state, err := w.db(ctx, control.Request{Action: "lookup", ID: instance.DesktopID})
			if err != nil {
				return nil, err
			}
			if state.Skip {
				if err = w.cloud.Terminate(ctx, instance.ID); err != nil {
					return nil, err
				}
				_ = w.cloud.DeleteCredential(ctx, instance.DesktopID)
			}
		}
		for _, mail := range plan.Mails {
			// Leave unsent messages queued if reconciliation used most of this
			// invocation. Each SMTP attempt has its own ten-second deadline.
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 15*time.Second {
				break
			}
			var payload struct{ Email, Kind, Token string }
			if err = json.Unmarshal(mail.Payload, &payload); err != nil {
				return nil, err
			}
			page := "verify"
			subject := "Verify your Mise En Abyme account"
			if payload.Kind == "reset" {
				page = "reset"
				subject = "Reset your Mise En Abyme password"
			}
			link := os.Getenv("APP_ORIGIN") + "/" + page + "#token=" + url.QueryEscape(payload.Token)
			err = w.sendMail(ctx, payload.Email, subject, subject+"\n\n"+link+"\n\nThis link expires in one hour. If you did not request this, ignore this email.")
			if err != nil {
				slog.Error("email delivery failed", "outbox", mail.ID)
				_, _ = w.db(ctx, control.Request{Action: "mail-failed", OutboxID: mail.ID})
			} else {
				_, _ = w.db(ctx, control.Request{Action: "mail-sent", OutboxID: mail.ID})
			}
		}
		_, err = w.db(ctx, control.Request{Action: "orphans"})
		slog.Info("capacity", "environment", os.Getenv("RESOURCE_PREFIX"), "activeDesktops", plan.Count)
		if err != nil {
			return nil, err
		}
		if _, gerr := w.gpu(ctx); gerr != nil {
			slog.Error("gpu host lifecycle failed", "error", gerr)
		}
		return map[string]any{"count": plan.Count}, nil
	default:
		return nil, fmt.Errorf("unknown worker action")
	}
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(err)
	}
	w := &worker{cloud: cloud.NewAWS(cfg), invoke: lambda.NewFromConfig(cfg), sfn: sfn.NewFromConfig(cfg), mail: newMailer(cfg), gpuHost: newGPUHost(cfg)}
	lambdaruntime.Start(w.handle)
}

func (w *worker) sendMail(ctx context.Context, to, subject, text string) error {
	return w.mail.Send(ctx, mail.Message{To: to, Subject: subject, Text: text})
}
