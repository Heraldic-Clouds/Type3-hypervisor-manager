package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	stdmail "net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

type smtpResult struct {
	message string
	auth    bool
	err     error
}

func smtpFixture(t *testing.T, mode string, rejectRecipient, untrusted bool) (*SMTP, <-chan smtpResult) {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	tlsMode := mode
	if mode == "no-starttls" {
		tlsMode = "starttls"
	}
	sender, err := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: n, TLSMode: tlsMode, From: "hello@example.test", Username: "mea-user", Password: "test-only-password"})
	if err != nil {
		t.Fatal(err)
	}
	if !untrusted {
		sender.roots = x509.NewCertPool()
		cert, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		sender.roots.AddCert(cert)
	}
	result := make(chan smtpResult, 1)
	go func() {
		r := smtpResult{}
		defer func() { result <- r }()
		conn, err := listener.Accept()
		if err != nil {
			r.err = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		secured := mode == "tls"
		serverTLS := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
		if secured {
			conn = tls.Server(conn, serverTLS)
		}
		p := textproto.NewConn(conn)
		_ = p.PrintfLine("220 smtp.example.test")
		for {
			line, err := p.ReadLine()
			if err != nil {
				return // Expected when a client rejects TLS or the missing extension.
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				_, _ = fmt.Fprint(conn, "250-smtp.example.test\r\n")
				if !secured && mode != "no-starttls" {
					_, _ = fmt.Fprint(conn, "250-STARTTLS\r\n")
				}
				_, _ = fmt.Fprint(conn, "250 AUTH PLAIN\r\n")
			case line == "STARTTLS":
				_ = p.PrintfLine("220 Begin TLS")
				conn = tls.Server(conn, serverTLS)
				p = textproto.NewConn(conn)
				secured = true
			case strings.HasPrefix(line, "AUTH PLAIN "):
				decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				if !secured || string(decoded) != "\x00mea-user\x00test-only-password" {
					r.err = fmt.Errorf("authentication sent without TLS or incorrect fixture credentials")
					return
				}
				r.auth = true
				_ = p.PrintfLine("235 Authenticated")
			case strings.HasPrefix(line, "MAIL FROM:"):
				if !r.auth {
					r.err = fmt.Errorf("MAIL before authentication")
					return
				}
				_ = p.PrintfLine("250 Sender accepted")
			case strings.HasPrefix(line, "RCPT TO:"):
				if rejectRecipient {
					_ = p.PrintfLine("550 Recipient rejected")
				} else {
					_ = p.PrintfLine("250 Recipient accepted")
				}
			case line == "DATA":
				_ = p.PrintfLine("354 Send message")
				data, err := io.ReadAll(p.DotReader())
				if err != nil {
					r.err = err
					return
				}
				r.message = string(data)
				_ = p.PrintfLine("250 Queued")
			case line == "QUIT":
				return // A lost QUIT response must not retry accepted DATA.
			default:
				r.err = fmt.Errorf("unexpected SMTP command")
				return
			}
		}
	}()
	return sender, result
}

func TestSMTPEncryptedDelivery(t *testing.T) {
	for _, mode := range []string{"starttls", "tls"} {
		t.Run(mode, func(t *testing.T) {
			sender, results := smtpFixture(t, mode, false, false)
			text := "Verify your account\n\nhttps://miseenaby.me/verify#token=one-use-test-token\n\nCafé"
			if err := sender.Send(context.Background(), Message{To: "user@example.test", Subject: "Verify your Mise En Abyme account", Text: text}); err != nil {
				t.Fatal(err)
			}
			r := <-results
			if r.err != nil || !r.auth {
				t.Fatalf("SMTP exchange failed: %v", r.err)
			}
			message, err := stdmail.ReadMessage(strings.NewReader(r.message))
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(quotedprintable.NewReader(message.Body))
			if strings.TrimSuffix(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") != text || message.Header.Get("To") != "user@example.test" || message.Header.Get("Message-ID") == "" {
				t.Fatal("message headers or verification link did not survive SMTP")
			}
		})
	}
}

func TestSMTPFailsClosed(t *testing.T) {
	for _, scenario := range []string{"untrusted-certificate", "no-starttls", "recipient-rejected"} {
		t.Run(scenario, func(t *testing.T) {
			mode := "starttls"
			if scenario == "no-starttls" {
				mode = scenario
			}
			sender, results := smtpFixture(t, mode, scenario == "recipient-rejected", scenario == "untrusted-certificate")
			if err := sender.Send(context.Background(), Message{To: "user@example.test", Subject: "Verify", Text: "test"}); err == nil {
				t.Fatal("unsafe or rejected SMTP delivery succeeded")
			}
			r := <-results
			if r.message != "" || (scenario != "recipient-rejected" && r.auth) {
				t.Fatal("message or credentials sent before required TLS validation")
			}
		})
	}
}

func TestSMTPRejectsHeaderInjection(t *testing.T) {
	sender, err := NewSMTP(SMTPConfig{Host: "smtp.example.test", Username: "mea-user", Password: "test-only-password", From: "hello@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []Message{
		{To: "user@example.test\r\nBcc: other@example.test", Subject: "Verify"},
		{To: "user@example.test", Subject: "Verify\r\nBcc: other@example.test"},
	} {
		if _, err := sender.message(message); err == nil {
			t.Fatal("accepted injected headers")
		}
	}
	if _, err := NewSMTP(SMTPConfig{Host: "smtp.example.test", Username: "u", Password: "p", From: "hello@example.test", TLSMode: "none"}); err == nil {
		t.Fatal("accepted plaintext SMTP configuration")
	}
}

func TestSMTPCancellationInterruptsBlockedServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(conn, "220 blocked.example.test\r\n")
		_, _ = io.Copy(io.Discard, bufio.NewReader(conn)) // Never answer EHLO.
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	sender, _ := NewSMTP(SMTPConfig{Host: "127.0.0.1", Port: n, From: "hello@example.test", Username: "u", Password: "p"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := sender.Send(ctx, Message{To: "user@example.test", Subject: "Verify", Text: "test"}); err == nil || time.Since(start) > 2*time.Second {
		t.Fatal("SMTP did not stop on cancellation")
	}
	<-done
}
