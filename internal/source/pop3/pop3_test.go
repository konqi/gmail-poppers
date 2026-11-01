package pop3

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
)

type fakeServer struct {
	mu        sync.Mutex
	msgs      map[string]string // uid -> body
	order     []string
	deleted   []string
	retrCount int
	commands  []string
	uidlError bool
	ln        net.Listener
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
		s.mu.Lock()
		s.commands = append(s.commands, f[0])
		s.mu.Unlock()
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
			if s.uidlError {
				w("-ERR UIDL unavailable")
				continue
			}
			w("+OK")
			for i, uid := range snapshot {
				w(strconv.Itoa(i+1) + " " + uid)
			}
			w(".")
		case "RETR", "TOP":
			n, _ := strconv.Atoi(f[1])
			s.mu.Lock()
			body := s.msgs[snapshot[n-1]]
			if f[0] == "RETR" {
				s.retrCount++
			} else {
				body, _, _ = strings.Cut(body, "\r\n\r\n")
				body += "\r\n"
			}
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

func TestCheck(t *testing.T) {
	for _, test := range []struct {
		name      string
		password  string
		uidlError bool
		wantError string
	}{
		{"success", "secret", false, ""},
		{"invalid password", "wrong", false, "pop3 login:"},
		{"listing failed", "secret", true, "pop3 uidl:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := startServer(t)
			server.uidlError = test.uidlError
			server.add("u1", msg("1", "body"))
			cfg := &config.Source{Server: "127.0.0.1", Port: server.port(), TLS: config.TLSNone,
				User: "me", Password: test.password, DeleteAfterImport: true, InitialSync: config.InitialSyncAll}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			err := New(cfg, log).Check(context.Background())
			if test.wantError == "" && err != nil || test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("unexpected error: %v", err)
			}
			server.mu.Lock()
			defer server.mu.Unlock()
			for _, command := range server.commands {
				if command != "USER" && command != "PASS" && command != "UIDL" && command != "QUIT" {
					t.Errorf("unexpected check command: %s", command)
				}
			}
			if test.wantError == "" && strings.Join(server.commands, ",") != "USER,PASS,UIDL,QUIT" {
				t.Errorf("unexpected sequence: %v", server.commands)
			}
		})
	}
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

func TestDryRunDoesNotDeleteOrPersist(t *testing.T) {
	srv := startServer(t)
	srv.add("u1", msg("1", "a"))
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port(), TLS: config.TLSNone,
		User: "me", Password: "secret", InitialSync: config.InitialSyncAll, DeleteAfterImport: true}
	dir := t.TempDir()
	st, _ := state.Open(dir, "t")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for i := 0; i < 2; i++ {
		p := &source.Processor{State: st, Sink: &recSession{}, InitialSync: cfg.InitialSync, DryRun: true, Log: log}
		if err := New(cfg, log).Fetch(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if p.Result.Imported != 1 || p.Result.Deleted != 1 {
			t.Fatalf("run %d: result = %+v", i, p.Result)
		}
	}
	if len(srv.deleted) != 0 || srv.retrCount != 0 {
		t.Fatalf("dry run deleted %v, RETR count %d", srv.deleted, srv.retrCount)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("dry run wrote state")
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
