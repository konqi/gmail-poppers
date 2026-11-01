package gmailimap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"gmail-poppers/internal/mailconn"
)

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

func start(t *testing.T) string {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("me@gmail.com", "app-pw")
	user.Create("INBOX", nil)
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
	return ln.Addr().String()
}

func testSink(addr, pw string) *Sink {
	host, port, _ := net.SplitHostPort(addr)
	s := New("me@gmail.com", pw, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _ := strconv.Atoi(port)
	s.endpoint = mailconn.Endpoint{Host: host, Port: p, Security: mailconn.Plain}
	return s
}

func flagsIn(t *testing.T, addr, folder string) [][]imap.Flag {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login("me@gmail.com", "app-pw").Wait(); err != nil {
		t.Fatal(err)
	}
	d, err := c.Select(folder, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if d.NumMessages == 0 {
		return nil
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]imap.Flag
	for _, m := range msgs {
		out = append(out, m.Flags)
	}
	return out
}

func TestImportUnreadWithLabel(t *testing.T) {
	addr := start(t)
	s := testSink(addr, "app-pw")
	sess, err := s.Open(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("Date: Mon, 02 Jan 2006 15:04:05 +0000\r\nSubject: hi\r\n\r\nbody\r\n")
	if err := sess.Import(context.Background(), raw, "telekom"); err != nil {
		t.Fatal(err)
	}
	sess.Close()

	inbox := flagsIn(t, addr, "INBOX")
	if len(inbox) != 1 {
		t.Fatalf("inbox messages = %d", len(inbox))
	}
	for _, f := range inbox[0] {
		if f == imap.FlagSeen {
			t.Fatal("imported message is marked as read")
		}
	}
	if len(flagsIn(t, addr, "telekom")) != 1 {
		t.Fatal("label folder not populated")
	}
}

func TestCheck(t *testing.T) {
	addr := start(t)
	if err := testSink(addr, "app-pw").Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(flagsIn(t, addr, "INBOX")) != 0 {
		t.Fatal("check appended a message")
	}
	if err := testSink(addr, "wrong").Check(context.Background()); err == nil || !strings.Contains(err.Error(), "gmail imap login") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoginErrorMentionsAppPassword(t *testing.T) {
	addr := start(t)
	_, err := testSink(addr, "wrong").Open(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "app password") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("err = %v", err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	addr := start(t)
	sess, err := testSink(addr, "app-pw").Open(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Import(context.Background(), []byte("Subject: x\r\n\r\n"), "telekom"); err != nil {
		t.Fatal(err)
	}
	sess.Close()
	if n := len(flagsIn(t, addr, "INBOX")); n != 0 {
		t.Fatalf("dry run appended %d messages", n)
	}
	c, _ := imapclient.DialInsecure(addr, nil)
	defer c.Close()
	c.Login("me@gmail.com", "app-pw").Wait()
	if _, err := c.Select("telekom", nil).Wait(); err == nil {
		t.Fatal("dry run created label folder")
	}
}
