package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

type Mail struct {
	To, Subject, Text string
	ICS               []byte
}

func buildMessage(from string, m Mail, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_, domain, _ := strings.Cut(from, "@")
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%s\r\n\r\n",
		from, m.To, mime.QEncoding.Encode("utf-8", m.Subject), now.Format(time.RFC1123Z), rand.Text(), domain, mw.Boundary())
	w, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return nil, err
	}
	qp := quotedprintable.NewWriter(w)
	qp.Write([]byte(m.Text))
	qp.Close()
	if m.ICS != nil {
		w, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/calendar; charset=utf-8; method=PUBLISH"},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {`attachment; filename="invite.ics"`},
		})
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(m.ICS)
		for len(enc) > 76 {
			fmt.Fprintf(w, "%s\r\n", enc[:76])
			enc = enc[76:]
		}
		fmt.Fprintf(w, "%s\r\n", enc)
	}
	mw.Close()
	return b.Bytes(), nil
}

type smtpConfig struct {
	Addr, User, Password, From string
	TLS                        string
}

func (c smtpConfig) dial() (*smtp.Client, error) {
	host, _, _ := strings.Cut(c.Addr, ":")
	conn, err := net.DialTimeout("tcp", c.Addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if c.TLS != "none" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			cl.Close()
			return nil, errors.New("smtp: server does not support STARTTLS")
		}
		if err := cl.StartTLS(&tls.Config{ServerName: host}); err != nil {
			cl.Close()
			return nil, err
		}
	}
	if c.User != "" {
		if err := cl.Auth(smtp.PlainAuth("", c.User, c.Password, host)); err != nil {
			cl.Close()
			return nil, err
		}
	}
	return cl, nil
}

func (c smtpConfig) send(m Mail) error {
	msg, err := buildMessage(c.From, m, time.Now())
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return fmt.Errorf("smtp connect: %v", err) // %v: a 5xx here (auth, STARTTLS) is our config, not permanent
	}
	defer cl.Close()
	if err := cl.Mail(c.From); err != nil {
		return err
	}
	if err := cl.Rcpt(m.To); err != nil {
		return err
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}
