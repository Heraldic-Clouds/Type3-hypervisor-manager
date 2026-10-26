package cloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/security"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	secretstypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"
	"os"
	"strconv"
	"strings"
)

type AWS struct {
	IdleSeconds, LifetimeSeconds                                       int
	SSM                                                                *ssm.Client
	EC2                                                                *ec2.Client
	Secrets                                                            *secretsmanager.Client
	Prefix, Region, Account, AMI, Subnet, Group, Profile, InstanceType string
}

func (a *AWS) secret(id string) string { return a.Prefix + "/desktop/" + id }
func (a *AWS) Find(ctx context.Context, id string) ([]Instance, error) {
	// Only Newbie desktops carry MEADesktop; the GPU host never matches.
	filters := []ec2types.Filter{{Name: aws.String("tag:MEAEnvironment"), Values: []string{a.Prefix}}, {Name: aws.String("tag-key"), Values: []string{"MEADesktop"}}, {Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}}}
	if id != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:MEADesktop"), Values: []string{id}})
	}
	out, e := a.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
	if e != nil {
		return nil, e
	}
	result := []Instance{}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			d := ""
			for _, tag := range i.Tags {
				if aws.ToString(tag.Key) == "MEADesktop" {
					d = aws.ToString(tag.Value)
				}
			}
			result = append(result, Instance{ID: aws.ToString(i.InstanceId), IP: aws.ToString(i.PrivateIpAddress), DesktopID: d, State: string(i.State.Name)})
		}
	}
	return result, nil
}
func (a *AWS) Launch(ctx context.Context, id string) (Instance, error) {
	if a.InstanceType != "t3a.medium" && a.InstanceType != "t3a.small" {
		return Instance{}, fmt.Errorf("instance type is not allowlisted")
	}
	old, e := a.Find(ctx, id)
	if e != nil {
		return Instance{}, e
	}
	if len(old) > 0 {
		return old[0], a.ensureCredential(ctx, id, old[0].ID)
	}
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
exec > /var/log/mea-bootstrap.log 2>&1
systemctl stop xrdp || true
iptables -I OUTPUT -d 169.254.169.254 -m owner ! --uid-owner 0 -j REJECT
install -d -m 700 /run/mea
for n in $(seq 1 90); do
 if aws secretsmanager get-secret-value --region '%s' --secret-id '%s' --query SecretString --output text > /run/mea/credential.json 2>/dev/null; then break; fi
 sleep 2
done
python3 - <<'PY'
import json,subprocess
with open('/run/mea/credential.json') as f: password=json.load(f)['password']
subprocess.run(['chpasswd'],input='mea:'+password+'\n',text=True,check=True)
PY
rm -f /run/mea/credential.json
systemctl enable --now xrdp
touch /run/mea/ready
install -d -m 700 /etc/mea
echo %d > /etc/mea/idle-ms
sed -i 's/value>=1200000/value>=%d/' /usr/local/sbin/mea-session-monitor
shutdown -h +%d
`, a.Region, a.secret(id), a.IdleSeconds*1000, a.IdleSeconds*1000, (a.LifetimeSeconds+59)/60)
	tags := []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("MEA Newbie " + id)}, {Key: aws.String("Project"), Value: aws.String("MiseEnAbyme")}, {Key: aws.String("MEAEnvironment"), Value: aws.String(a.Prefix)}, {Key: aws.String("MEADesktop"), Value: aws.String(id)}, {Key: aws.String("ManagedBy"), Value: aws.String("mea-worker")}}
	out, e := a.EC2.RunInstances(ctx, &ec2.RunInstancesInput{ImageId: aws.String(a.AMI), InstanceType: ec2types.InstanceType(a.InstanceType), MinCount: aws.Int32(1), MaxCount: aws.Int32(1), ClientToken: aws.String(id), IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Name: aws.String(a.Profile)}, NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), AssociatePublicIpAddress: aws.Bool(true), SubnetId: aws.String(a.Subnet), Groups: []string{a.Group}, DeleteOnTermination: aws.Bool(true)}}, MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{HttpTokens: ec2types.HttpTokensStateRequired, HttpPutResponseHopLimit: aws.Int32(1), HttpEndpoint: ec2types.InstanceMetadataEndpointStateEnabled}, UserData: aws.String(base64.StdEncoding.EncodeToString([]byte(script))), InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate, CreditSpecification: &ec2types.CreditSpecificationRequest{CpuCredits: aws.String("standard")}, BlockDeviceMappings: []ec2types.BlockDeviceMapping{{DeviceName: aws.String("/dev/sda1"), Ebs: &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(12), VolumeType: ec2types.VolumeTypeGp3, Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true)}}}, TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}, {ResourceType: ec2types.ResourceTypeVolume, Tags: tags}}})
	if e != nil {
		return Instance{}, e
	}
	i := out.Instances[0]
	instance := Instance{ID: aws.ToString(i.InstanceId), IP: aws.ToString(i.PrivateIpAddress), DesktopID: id, State: string(i.State.Name)}
	return instance, a.ensureCredential(ctx, id, instance.ID)
}
func (a *AWS) ensureCredential(ctx context.Context, id, instanceID string) error {
	password := security.Random(24)
	payload, _ := json.Marshal(map[string]string{"password": password})
	_, e := a.Secrets.CreateSecret(ctx, &secretsmanager.CreateSecretInput{Name: aws.String(a.secret(id)), SecretString: aws.String(string(payload)), Tags: []secretstypes.Tag{{Key: aws.String("Project"), Value: aws.String("MiseEnAbyme")}, {Key: aws.String("InstanceArn"), Value: aws.String(fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", a.Region, a.Account, instanceID))}}})
	if e != nil {
		var api smithy.APIError
		if !errors.As(e, &api) || api.ErrorCode() != "ResourceExistsException" {
			return e
		}
	}
	return nil
}
func (a *AWS) Password(ctx context.Context, id string) (string, error) {
	out, e := a.Secrets.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(a.secret(id))})
	if e != nil {
		return "", e
	}
	var payload struct{ Password string }
	e = json.Unmarshal([]byte(aws.ToString(out.SecretString)), &payload)
	return payload.Password, e
}
func (a *AWS) DeleteCredential(ctx context.Context, id string) error {
	_, e := a.Secrets.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{SecretId: aws.String(a.secret(id)), ForceDeleteWithoutRecovery: aws.Bool(true)})
	var api smithy.APIError
	if errors.As(e, &api) && api.ErrorCode() == "ResourceNotFoundException" {
		return nil
	}
	return e
}
func (a *AWS) Terminate(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, e := a.EC2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	var api smithy.APIError
	if errors.As(e, &api) && strings.Contains(api.ErrorCode(), "NotFound") {
		return nil
	}
	return e
}
func NewAWS(cfg aws.Config) *AWS {
	return &AWS{IdleSeconds: setting("IDLE_SECONDS", 1200), LifetimeSeconds: setting("LIFETIME_SECONDS", 14400), EC2: ec2.NewFromConfig(cfg), SSM: ssm.NewFromConfig(cfg), Secrets: secretsmanager.NewFromConfig(cfg), Prefix: os.Getenv("RESOURCE_PREFIX"), Region: cfg.Region, Account: os.Getenv("ACCOUNT_ID"), AMI: os.Getenv("DESKTOP_AMI"), Subnet: os.Getenv("DESKTOP_SUBNET"), Group: os.Getenv("DESKTOP_SG"), Profile: os.Getenv("DESKTOP_PROFILE"), InstanceType: os.Getenv("DESKTOP_INSTANCE_TYPE")}
}

func setting(name string, fallback int) int {
	n, e := strconv.Atoi(os.Getenv(name))
	if e != nil || n <= 0 {
		return fallback
	}
	return n
}

var _ Compute = (*AWS)(nil)
