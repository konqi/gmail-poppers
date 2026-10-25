// Package mailconn dials POP3/IMAP servers and decides how TLS is used.
package mailconn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"gmail-poppers/internal/config"
)

type Protocol struct {
	Name      string
	PlainPort int
	TLSPort   int
}

var (
	POP3 = Protocol{Name: "pop3", PlainPort: 110, TLSPort: 995}
	IMAP = Protocol{Name: "imap", PlainPort: 143, TLSPort: 993}
)

type Security int

const (
	Implicit Security = iota
	StartTLS
	Plain
)

func (s Security) String() string {
	switch s {
	case Implicit:
		return "implicit TLS"
	case StartTLS:
		return "STARTTLS"
	default:
		return "plaintext"
	}
}

type Endpoint struct {
	Host     string
	Port     int
	Security Security
}

func (e Endpoint) Addr() string { return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)) }

func (e Endpoint) String() string { return fmt.Sprintf("%s (%s)", e.Addr(), e.Security) }

// Candidates lists the endpoints to try in order. Plaintext is only ever
// returned when mode is "none".
func Candidates(p Protocol, host string, port int, mode string) []Endpoint {
	ep := func(port int, s Security) Endpoint { return Endpoint{Host: host, Port: port, Security: s} }
	switch mode {
	case config.TLSImplicit:
		return []Endpoint{ep(orDefault(port, p.TLSPort), Implicit)}
	case config.TLSStartTLS:
		return []Endpoint{ep(orDefault(port, p.PlainPort), StartTLS)}
	case config.TLSNone:
		return []Endpoint{ep(orDefault(port, p.PlainPort), Plain)}
	}
	switch port {
	case 0:
		return []Endpoint{ep(p.TLSPort, Implicit), ep(p.PlainPort, StartTLS)}
	case p.TLSPort:
		return []Endpoint{ep(port, Implicit)}
	case p.PlainPort:
		return []Endpoint{ep(port, StartTLS)}
	default:
		return []Endpoint{ep(port, Implicit), ep(port, StartTLS)}
	}
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func TLSConfig(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// Dial opens a TCP connection and performs the TLS handshake for implicit TLS.
// STARTTLS upgrades are protocol specific and left to the caller.
func Dial(ctx context.Context, e Endpoint, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	if e.Security != Implicit {
		return d.DialContext(ctx, "tcp", e.Addr())
	}
	td := &tls.Dialer{NetDialer: d, Config: TLSConfig(e.Host)}
	return td.DialContext(ctx, "tcp", e.Addr())
}

// Connect tries each candidate in order and returns the first success.
func Connect[T any](ctx context.Context, cands []Endpoint, try func(Endpoint) (T, error)) (T, Endpoint, error) {
	var zero T
	var errs []error
	for _, e := range cands {
		if err := ctx.Err(); err != nil {
			return zero, Endpoint{}, err
		}
		v, err := try(e)
		if err == nil {
			return v, e, nil
		}
		if len(cands) == 1 {
			return zero, e, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", e, err))
	}
	return zero, Endpoint{}, fmt.Errorf("tls auto-detection failed: %w", errors.Join(errs...))
}
