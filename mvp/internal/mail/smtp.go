// Package mail sends transactional messages without exposing transport credentials
// to the API, database, workflow state or browser.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"net"
	stdmail "net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

type Message struct {
	To, Subject, Text string
}

// SMTPConfig is the JSON stored in Secrets Manager, never in Terraform state.
// TLSMode is "starttls" (normally port 587) or "tls" (normally port 465).
type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLSMode  string `json:"tlsMode"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

type SMTP struct {
	config SMTPConfig
	roots  *x509.CertPool // nil uses system trust; tests supply their own CA.
}

func NewSMTP(c SMTPConfig) (*SMTP, error) {
	if c.TLSMode == "" {
		c.TLSMode = "starttls"
	}
	if c.TLSMode != "starttls" && c.TLSMode != "tls" {
		return nil, errors.New("SMTP requires starttls or tls")
	}
	if c.Port == 0 {
		c.Port = 587
		if c.TLSMode == "tls" {
			c.Port = 465
		}
	}
	if c.Host == "" || strings.ContainsAny(c.Host, " /\\\t\r\n:@") || c.Port < 1 || c.Port > 65535 {
		return nil, errors.New("invalid SMTP endpoint")
	}
	if c.Username == "" || c.Password == "" || strings.ContainsRune(c.Username+c.Password, 0) {
		return nil, errors.New("SMTP username and password are required")
	}
	if !bareAddress(c.From) {
		return nil, errors.New("SMTP from must be a single bare email address")
	}
	return &SMTP{config: c}, nil
}

func bareAddress(value string) bool {
	if strings.ContainsAny(value, "\r\n") {
		return false
	}
	a, err := stdmail.ParseAddress(value)
	return err == nil && a.Address == value && strings.Contains(value, "@")
}

func (s *SMTP) message(m Message) ([]byte, error) {
	if !bareAddress(m.To) || m.Subject == "" || strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("invalid transactional email headers")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	var body bytes.Buffer
	domain := s.config.From[strings.LastIndexByte(s.config.From, '@')+1:]
	fmt.Fprintf(&body, "From: Mise En Abyme <%s>\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", s.config.From, m.To, m.Subject, time.Now().UTC().Format(time.RFC1123Z), hex.EncodeToString(id[:]), domain)
	encoded := quotedprintable.NewWriter(&body)
	if _, err := encoded.Write([]byte(m.Text)); err != nil {
		return nil, err
	}
	if err := encoded.Close(); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	message, err := s.message(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := s.config
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	tlsConfig := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12, RootCAs: s.roots}
	if c.TLSMode == "tls" {
		secured := tls.Client(conn, tlsConfig)
		if err = secured.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = secured
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if c.TLSMode == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server did not offer required STARTTLS")
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if err = client.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
		return err
	}
	if err = client.Mail(c.From); err != nil {
		return err
	}
	if err = client.Rcpt(m.To); err != nil {
		return err
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = data.Write(message); err != nil {
		return err
	}
	if err = data.Close(); err != nil {
		return err
	}
	// DATA acknowledgement means the server accepted the message. A lost QUIT
	// response must not cause another delivery of the same verification link.
	_ = client.Quit()
	return nil
}
