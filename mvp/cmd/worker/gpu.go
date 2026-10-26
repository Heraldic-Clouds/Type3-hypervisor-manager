package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/control"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/paid"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// gpuAPI is the EC2/SSM surface the GPU host lifecycle needs. The worker's
// IAM policy allows launching only from the GPU launch template, starting and
// stopping only the tagged host, and never terminating it.
type gpuAPI interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
}
type paramAPI interface {
	GetParameter(context.Context, *ssm.GetParameterInput, ...func(*ssm.Options)) (*ssm.GetParameterOutput, error)
}

type gpuHost struct {
	ec2      gpuAPI
	ssm      paramAPI
	prefix   string
	template string
}

// gpu drives the single GPU host from the database plan: launch once, start
// when stopped, stop when idle. It never creates a second host: the database
// slot, the lease, the 16-vCPU account quota and IAM all prevent it, and if
// two ever exist the extra one is stopped and an alarm fires.
func (w *worker) gpu(ctx context.Context) (map[string]any, error) {
	g := w.gpuHost
	if g == nil || g.template == "" {
		return nil, nil
	}
	res, err := w.db(ctx, control.Request{Action: "gpu-plan"})
	if err != nil || res.Plan == nil {
		return nil, fmt.Errorf("gpu plan: %v", err)
	}
	plan := *res.Plan
	out, err := g.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:MEAGpuHost"), Values: []string{g.prefix}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
	}})
	if err != nil {
		return nil, err
	}
	var hosts []ec2types.Instance
	for _, r := range out.Reservations {
		hosts = append(hosts, r.Instances...)
	}
	running := 0
	for _, h := range hosts {
		if h.State.Name == ec2types.InstanceStateNameRunning || h.State.Name == ec2types.InstanceStateNamePending {
			running++
		}
	}
	slog.Info("gpu host", "environment", g.prefix, "gpuHosts", len(hosts), "gpuRunning", running, "desired", plan.Host.Desired)
	obs := paid.Observation{}
	if len(hosts) > 1 {
		// Keep the recorded host, stop any other: never more than one running.
		keep := aws.ToString(plan.Host.InstanceID)
		for _, h := range hosts {
			if id := aws.ToString(h.InstanceId); id != keep && h.State.Name != ec2types.InstanceStateNameStopped {
				_, _ = g.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{id}})
			}
		}
		obs.Error = "more than one GPU host exists; extra instances were stopped"
		slog.Error("gpu host limit exceeded", "count", len(hosts))
		for _, h := range hosts {
			if aws.ToString(h.InstanceId) == keep {
				hosts = []ec2types.Instance{h}
				break
			}
		}
		if len(hosts) > 1 {
			hosts = hosts[:1]
		}
	}
	var h *ec2types.Instance
	if len(hosts) == 1 {
		h = &hosts[0]
		obs.InstanceID, obs.State, obs.PrivateIP = aws.ToString(h.InstanceId), string(h.State.Name), aws.ToString(h.PrivateIpAddress)
	}
	lease := func() bool {
		r, err := w.db(ctx, control.Request{Action: "gpu-lease", ID: "worker"})
		return err == nil && r.Ready
	}
	switch {
	case plan.Host.Desired == "running" && h == nil && len(hosts) == 0:
		if !lease() {
			break
		}
		token := fmt.Sprintf("%s-gpu-%d", g.prefix, plan.Host.Launches)
		run, err := g.ec2.RunInstances(ctx, &ec2.RunInstancesInput{MinCount: aws.Int32(1), MaxCount: aws.Int32(1), ClientToken: aws.String(token),
			LaunchTemplate: &ec2types.LaunchTemplateSpecification{LaunchTemplateId: aws.String(g.template), Version: aws.String("$Latest")}})
		if err != nil {
			obs.Error = "launch failed: " + err.Error()
			slog.Error("gpu host launch failed", "error", err)
			break
		}
		i := run.Instances[0]
		obs = paid.Observation{InstanceID: aws.ToString(i.InstanceId), State: string(i.State.Name), PrivateIP: aws.ToString(i.PrivateIpAddress), Launched: true}
	case plan.Host.Desired == "running" && h != nil && h.State.Name == ec2types.InstanceStateNameStopped:
		if !lease() {
			break
		}
		if _, err := g.ec2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{obs.InstanceID}}); err != nil {
			obs.Error = "start failed: " + err.Error()
			slog.Error("gpu host start failed", "error", err)
			break
		}
		obs.State = "pending"
	case plan.Host.Desired == "stopped" && plan.StopNow && h != nil && (h.State.Name == ec2types.InstanceStateNameRunning || h.State.Name == ec2types.InstanceStateNamePending):
		// StopInstances keeps the EBS volumes; the host is never terminated.
		if _, err := g.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{obs.InstanceID}}); err != nil {
			obs.Error = "stop failed: " + err.Error()
			break
		}
		obs.State = "stopping"
	}
	if obs.State == "running" {
		if p, err := g.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String("/" + g.prefix + "/type3/fingerprint")}); err == nil {
			obs.Fingerprint = aws.ToString(p.Parameter.Value)
		}
	}
	if _, err = w.db(ctx, control.Request{Action: "gpu-observed", Observation: &obs}); err != nil {
		return nil, err
	}
	summary := map[string]any{"gpuState": obs.State}
	if obs.State == "running" || plan.Needed {
		r, err := w.db(ctx, control.Request{Action: "paid-reconcile"})
		if err != nil {
			slog.Error("paid reconcile failed", "error", err)
		} else if len(r.Summary) > 0 {
			slog.Info("paid reconcile", "summary", r.Summary)
		}
	}
	return summary, nil
}

func newGPUHost(cfg aws.Config) *gpuHost {
	return &gpuHost{ec2: ec2.NewFromConfig(cfg), ssm: ssm.NewFromConfig(cfg), prefix: os.Getenv("RESOURCE_PREFIX"), template: os.Getenv("GPU_LAUNCH_TEMPLATE")}
}

