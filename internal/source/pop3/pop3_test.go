package pop3

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
)

type fakeServer struct {
	mu      sync.Mutex
	msgs    map[string]string // uid -> body
	order   []string
	deleted []string
	ln      net.Listener
}

func startServer(t *testing.T) *fakeServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{msgs: map[string]string{}, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeServer) add(uid, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs[uid] = body
	s.order = append(s.order, uid)
}

func (s *fakeServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeServer) serve(conn net.Conn) {
	defer conn.Close()
	s.mu.Lock()
	snapshot := append([]string(nil), s.order...)
	s.mu.Unlock()
	var pending []string
	r := bufio.NewReader(conn)
	w := func(l string) { io.WriteString(conn, l+"\r\n") }
	w("+OK ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "USER":
			w("+OK")
		case "PASS":
			if f[1] != "secret" {
				w("-ERR [AUTH] invalid credentials")
				continue
			}
			w("+OK")
		case "UIDL":
			w("+OK")
			for i, uid := range snapshot {
				w(strconv.Itoa(i+1) + " " + uid)
			}
			w(".")
		case "RETR":
			n, _ := strconv.Atoi(f[1])
			s.mu.Lock()
			body := s.msgs[snapshot[n-1]]
			s.mu.Unlock()
			w("+OK")
			for _, l := range strings.Split(body, "\r\n") {
				if strings.HasPrefix(l, ".") {
					l = "." + l
				}
				w(l)
			}
			w(".")
		case "DELE":
			n, _ := strconv.Atoi(f[1])
			pending = append(pending, snapshot[n-1])
			w("+OK")
		case "QUIT":
			s.mu.Lock()
			for _, uid := range pending {
				delete(s.msgs, uid)
				for i, o := range s.order {
					if o == uid {
						s.order = append(s.order[:i], s.order[i+1:]...)
						break
					}
				}
				s.deleted = append(s.deleted, uid)
			}
			s.mu.Unlock()
			w("+OK bye")
			return
		default:
			w("-ERR unknown")
		}
	}
}

type recSession struct{ got []string }

func (r *recSession) Import(_ context.Context, raw []byte, _ string) error {
	r.got = append(r.got, string(raw))
	return nil
}
func (r *recSession) Close() error { return nil }

func run(t *testing.T, cfg *config.Source, st *state.Store) (*recSession, *source.Processor, error) {
	t.Helper()
	sess := &recSession{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &source.Processor{State: st, Sink: sess, InitialSync: cfg.InitialSync, Log: log}
	err := New(cfg, log).Fetch(context.Background(), p)
	return sess, p, err
}

func msg(id, body string) string {
	return "Message-ID: <" + id + "@x>\r\nSubject: t\r\n\r\n" + body + "\r\n"
}

func TestFetchNewOnlyThenImport(t *testing.T) {
	srv := startServer(t)
	srv.add("u1", msg("1", "old"))
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port(), TLS: config.TLSNone,
		User: "me", Password: "secret", InitialSync: config.InitialSyncNewOnly}
	st, _ := state.Open(t.TempDir(), "t")

	sess, p, err := run(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.got) != 0 || p.Result.Baseline != 1 {
		t.Fatalf("first run imported %d, baseline %d", len(sess.got), p.Result.Baseline)
	}

	srv.add("u2", msg("2", ".dotted line"))
	sess, p, err = run(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.got) != 1 || p.Result.Imported != 1 {
		t.Fatalf("second run imported %d", len(sess.got))
	}
	if !strings.Contains(sess.got[0], "\r\n.dotted line\r\n") {
		t.Fatalf("dot-unstuffing/CRLF broken: %q", sess.got[0])
	}
	if len(srv.deleted) != 0 {
		t.Fatalf("deleted without delete_after_import: %v", srv.deleted)
	}
}

func TestFetchAllWithDelete(t *testing.T) {
	srv := startServer(t)
	srv.add("u1", msg("1", "a"))
	srv.add("u2", msg("2", "b"))
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port(), TLS: config.TLSNone,
		User: "me", Password: "secret", InitialSync: config.InitialSyncAll, DeleteAfterImport: true}
	st, _ := state.Open(t.TempDir(), "t")

	sess, _, err := run(t, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.got) != 2 || len(srv.deleted) != 2 {
		t.Fatalf("imported %d, deleted %v", len(sess.got), srv.deleted)
	}
}

func TestLoginErrorHasContextAndNoPassword(t *testing.T) {
	srv := startServer(t)
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port(), TLS: config.TLSNone,
		User: "me", Password: "wrong-pw", InitialSync: config.InitialSyncAll}
	st, _ := state.Open(t.TempDir(), "t")
	_, _, err := run(t, cfg, st)
	if err == nil || !strings.HasPrefix(err.Error(), "pop3 login: server: [AUTH] invalid credentials") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong-pw") {
		t.Fatal("password leaked into error")
	}
}

func TestAutoTLSNeverFallsBackToPlaintext(t *testing.T) {
	srv := startServer(t)
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port(), TLS: config.TLSAuto,
		User: "me", Password: "secret", InitialSync: config.InitialSyncAll}
	st, _ := state.Open(t.TempDir(), "t")
	_, _, err := run(t, cfg, st)
	if err == nil || !strings.Contains(err.Error(), "tls auto-detection failed") {
		t.Fatalf("err = %v", err)
	}
}
