package imap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/source"
	"gmail-poppers/internal/state"
)

type testServer struct {
	addr string
	port int
}

func startServer(t *testing.T) *testServer {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("me", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if err := user.Create("Archive", nil); err != nil {
		t.Fatal(err)
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}},
		InsecureAuth: true,
		Logger:       nopLogger{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &testServer{addr: ln.Addr().String(), port: ln.Addr().(*net.TCPAddr).Port}
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func (s *testServer) client(t *testing.T) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(s.addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login("me", "secret").Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *testServer) add(t *testing.T, folder, raw string) {
	t.Helper()
	c := s.client(t)
	defer c.Close()
	cmd := c.Append(folder, int64(len(raw)), nil)
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func (s *testServer) count(t *testing.T, folder string) uint32 {
	t.Helper()
	c := s.client(t)
	defer c.Close()
	d, err := c.Select(folder, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return d.NumMessages
}

type recSession struct{ got []string }

func (r *recSession) Import(_ context.Context, raw []byte, _ string) error {
	r.got = append(r.got, string(raw))
	return nil
}
func (r *recSession) Close() error { return nil }

func run(t *testing.T, cfg *config.Source, st *state.Store) (*recSession, *source.Processor) {
	t.Helper()
	sess := &recSession{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &source.Processor{State: st, Sink: sess, InitialSync: cfg.InitialSync, Log: log}
	if err := New(cfg, log).Fetch(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return sess, p
}

func msg(id string) string {
	return "Message-ID: <" + id + "@x>\r\nSubject: " + id + "\r\n\r\nbody\r\n"
}

func TestCheck(t *testing.T) {
	server := startServer(t)
	server.add(t, "INBOX", msg("existing"))
	cfg := &config.Source{Server: "127.0.0.1", Port: server.port, TLS: config.TLSNone,
		User: "me", Password: "secret", Folders: []string{"INBOX", "Archive"}, DeleteAfterImport: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := New(cfg, log).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.count(t, "INBOX") != 1 {
		t.Fatal("check deleted a message")
	}
	connection := server.client(t)
	defer connection.Close()
	connection.Select("INBOX", nil).Wait()
	messages, err := connection.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range messages[0].Flags {
		if flag == imap.FlagSeen || flag == imap.FlagDeleted {
			t.Fatalf("check modified message flags: %v", messages[0].Flags)
		}
	}
	cfg.Password = "wrong"
	if err := New(cfg, log).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "imap login:") {
		t.Fatalf("unexpected login error: %v", err)
	}
	cfg.Password = "secret"
	cfg.Folders = []string{"missing"}
	if err := New(cfg, log).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "select:") {
		t.Fatalf("unexpected folder error: %v", err)
	}
}

func TestFetchNewOnlyThenNewMessages(t *testing.T) {
	srv := startServer(t)
	srv.add(t, "INBOX", msg("old"))
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port, TLS: config.TLSNone,
		User: "me", Password: "secret", Folders: []string{"INBOX", "Archive"}, InitialSync: config.InitialSyncNewOnly}
	st, _ := state.Open(t.TempDir(), "t")

	sess, p := run(t, cfg, st)
	if len(sess.got) != 0 || p.Result.Baseline != 1 {
		t.Fatalf("first run: imported %d baseline %d", len(sess.got), p.Result.Baseline)
	}

	srv.add(t, "INBOX", msg("new1"))
	srv.add(t, "Archive", msg("new2"))
	sess, _ = run(t, cfg, st)
	if len(sess.got) != 2 {
		t.Fatalf("second run imported %d", len(sess.got))
	}
	if !strings.Contains(sess.got[0], "Subject: new1") {
		t.Fatalf("unexpected message %q", sess.got[0])
	}

	sess, _ = run(t, cfg, st)
	if len(sess.got) != 0 {
		t.Fatalf("third run re-imported %d", len(sess.got))
	}
	if srv.count(t, "INBOX") != 2 {
		t.Fatal("messages deleted without delete_after_import")
	}
}

func TestFetchAllWithDelete(t *testing.T) {
	srv := startServer(t)
	srv.add(t, "INBOX", msg("a"))
	srv.add(t, "INBOX", msg("b"))
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port, TLS: config.TLSNone,
		User: "me", Password: "secret", Folders: []string{"INBOX"}, InitialSync: config.InitialSyncAll, DeleteAfterImport: true}
	st, _ := state.Open(t.TempDir(), "t")

	sess, _ := run(t, cfg, st)
	if len(sess.got) != 2 {
		t.Fatalf("imported %d", len(sess.got))
	}
	if n := srv.count(t, "INBOX"); n != 0 {
		t.Fatalf("%d messages left after delete", n)
	}
}

func TestDryRunDoesNotDeleteOrPersist(t *testing.T) {
	srv := startServer(t)
	srv.add(t, "INBOX", msg("a"))
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port, TLS: config.TLSNone,
		User: "me", Password: "secret", Folders: []string{"INBOX"}, InitialSync: config.InitialSyncAll, DeleteAfterImport: true}
	dir := t.TempDir()
	st, _ := state.Open(dir, "t")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess := &recSession{}
	p := &source.Processor{State: st, Sink: sess, InitialSync: cfg.InitialSync, DryRun: true, Log: log}
	if err := New(cfg, log).Fetch(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if p.Result.Imported != 1 || p.Result.Deleted != 1 {
		t.Fatalf("result = %+v", p.Result)
	}
	if strings.Contains(sess.got[0], "body") {
		t.Fatal("dry run fetched the full message")
	}
	if srv.count(t, "INBOX") != 1 {
		t.Fatal("dry run deleted messages")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatal("dry run wrote state")
	}
}

func TestLoginError(t *testing.T) {
	srv := startServer(t)
	cfg := &config.Source{Name: "t", Server: "127.0.0.1", Port: srv.port, TLS: config.TLSNone,
		User: "me", Password: "bad", Folders: []string{"INBOX"}, InitialSync: config.InitialSyncAll}
	st, _ := state.Open(t.TempDir(), "t")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := New(cfg, log).Fetch(context.Background(), &source.Processor{State: st, Sink: &recSession{}, Log: log})
	if err == nil || !strings.HasPrefix(err.Error(), "imap login:") {
		t.Fatalf("err = %v", err)
	}
}
