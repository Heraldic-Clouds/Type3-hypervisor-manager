package cloud

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"time"
)

func (a *AWS) Ready(ctx context.Context, id string) (bool, error) {
	out, e := a.SSM.SendCommand(ctx, &ssm.SendCommandInput{DocumentName: aws.String("AWS-RunShellScript"), InstanceIds: []string{id}, Parameters: map[string][]string{"commands": {"test -f /run/mea/ready && systemctl is-active --quiet xrdp"}}})
	if e != nil {
		return false, nil
	} // SSM registration normally lags EC2 running.
	for n := 0; n < 3; n++ {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
		status, e := a.SSM.GetCommandInvocation(ctx, &ssm.GetCommandInvocationInput{CommandId: out.Command.CommandId, InstanceId: aws.String(id)})
		if e == nil {
			if string(status.Status) == "Success" {
				return true, nil
			}
			if string(status.Status) == "Failed" {
				return false, nil
			}
		}
	}
	return false, nil
}
