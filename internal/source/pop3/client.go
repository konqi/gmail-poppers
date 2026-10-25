// Package pop3 implements a minimal POP3 client (RFC 1939, RFC 2595 STLS) and source.
package pop3

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const cmdTimeout = 2 * time.Minute

type client struct {
	conn net.Conn
	r    *bufio.Reader
}

func newClient(conn net.Conn) (*client, error) {
	c := &client{conn: conn, r: bufio.NewReader(conn)}
	if _, err := c.readStatus(); err != nil {
		return nil, fmt.Errorf("greeting: %w", err)
	}
	return c, nil
}

func (c *client) startTLS(cfg *tls.Config) error {
	if _, err := c.cmd("STLS"); err != nil {
		return fmt.Errorf("STARTTLS not supported: %w", err)
	}
	tc := tls.Client(c.conn, cfg)
	if err := tc.Handshake(); err != nil {
		return err
	}
	c.conn = tc
	c.r = bufio.NewReader(tc)
	return nil
}

func (c *client) login(user, pass string) error {
	if _, err := c.cmd("USER " + user); err != nil {
		return err
	}
	_, err := c.cmd("PASS " + pass)
	return err
}

type listing struct {
	num int
	uid string
}

func (c *client) uidl() ([]listing, error) {
	if _, err := c.cmd("UIDL"); err != nil {
		return nil, err
	}
	body, err := c.readMultiline()
	if err != nil {
		return nil, err
	}
	var out []listing
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\r\n") {
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("malformed UIDL line %q", line)
		}
		n, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("malformed UIDL line %q", line)
		}
		out = append(out, listing{num: n, uid: f[1]})
	}
	return out, nil
}

func (c *client) retr(n int) ([]byte, error) {
	if _, err := c.cmd("RETR " + strconv.Itoa(n)); err != nil {
		return nil, err
	}
	return c.readMultiline()
}

func (c *client) top(n, lines int) ([]byte, error) {
	if _, err := c.cmd("TOP " + strconv.Itoa(n) + " " + strconv.Itoa(lines)); err != nil {
		return nil, err
	}
	return c.readMultiline()
}

func (c *client) dele(n int) error {
	_, err := c.cmd("DELE " + strconv.Itoa(n))
	return err
}

func (c *client) quit() error {
	_, err := c.cmd("QUIT")
	c.conn.Close()
	return err
}

// cmd sends a command; the command text is never included in errors because it may contain a password.
func (c *client) cmd(line string) (string, error) {
	c.conn.SetDeadline(time.Now().Add(cmdTimeout))
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		return "", err
	}
	return c.readStatus()
}

func (c *client) readStatus() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "+OK"):
		return strings.TrimSpace(line[3:]), nil
	case strings.HasPrefix(line, "-ERR"):
		msg := strings.TrimSpace(line[4:])
		if msg == "" {
			msg = "-ERR"
		}
		return "", errors.New("server: " + msg)
	default:
		return "", fmt.Errorf("unexpected response %q", line)
	}
}

// readMultiline reads a dot-terminated response, keeping CRLF line endings.
func (c *client) readMultiline() ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 1 && line[0] == '.' {
			return buf.Bytes(), nil
		}
		if len(line) > 0 && line[0] == '.' {
			line = line[1:]
		}
		buf.Write(line)
		buf.WriteString("\r\n")
	}
}

// closeOnCancel closes the connection when ctx ends; call the returned func to stop watching.
func closeOnCancel(ctx context.Context, conn net.Conn) func() bool {
	return context.AfterFunc(ctx, func() { conn.Close() })
}
