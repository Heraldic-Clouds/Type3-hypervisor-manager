package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/mvp/internal/mail"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

type mailSender interface {
	Send(context.Context, mail.Message) error
}

type secretAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

type smtpMailer struct {
	secrets secretAPI
	arn     string
}

func (s *smtpMailer) Send(ctx context.Context, m mail.Message) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Read the current secret for each attempt, so rotation does not require a
	// deployment and invalid mail credentials cannot stop desktop reconciliation.
	out, err := s.secrets.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(s.arn)})
	if err != nil {
		return errors.New("SMTP secret unavailable")
	}
	var config mail.SMTPConfig
	if err = json.Unmarshal([]byte(aws.ToString(out.SecretString)), &config); err != nil {
		return errors.New("SMTP secret must contain valid JSON")
	}
	sender, err := mail.NewSMTP(config)
	if err != nil {
		return err
	}
	return sender.Send(ctx, m)
}

type sesMailer struct {
	client *sesv2.Client
	from   string
}

func (s *sesMailer) Send(ctx context.Context, m mail.Message) error {
	_, err := s.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.from),
		Destination:      &sestypes.Destination{ToAddresses: []string{m.To}},
		Content: &sestypes.EmailContent{Simple: &sestypes.Message{
			Subject: &sestypes.Content{Data: aws.String(m.Subject), Charset: aws.String("UTF-8")},
			Body:    &sestypes.Body{Text: &sestypes.Content{Data: aws.String(m.Text), Charset: aws.String("UTF-8")}},
		}},
	})
	return err
}

func newMailer(cfg aws.Config) mailSender {
	if arn := os.Getenv("SMTP_SECRET_ARN"); arn != "" {
		return &smtpMailer{secrets: secretsmanager.NewFromConfig(cfg), arn: arn}
	}
	return &sesMailer{client: sesv2.NewFromConfig(cfg), from: os.Getenv("EMAIL_FROM")}
}
