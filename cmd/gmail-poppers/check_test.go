package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gmail-poppers/internal/config"
)

func TestSourceSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `accounts: {
targets: {a@gmail.com: {}, b@gmail.com: {}},
sources: {
shared@example.com: {type: pop3, server: localhost, user: u, password: p},
shared@other.example: {type: imap, server: localhost, user: u, password: p}
}
}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--mode", "source", "--config", path, "shared"}, "unknown source"},
		{[]string{"--mode", "source", "--config", path, "a@gmail.com/missing"}, "unknown source"},
		{[]string{"--mode", "source", "--config", path, "a@gmail.com/shared@example.com"}, "unknown source"},
		{[]string{"--mode", "target", "--config", path, "missing@gmail.com"}, "unknown target"},
	} {
		if err := runTest(test.args, io.Discard); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%v: unexpected error %v", test.args, err)
		}
	}
	if err := runTest([]string{"--help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--mode", "unknown", "name"}, {"--mode", "source", "--timeout", "0s", "name"}} {
		if err := runTest(args, io.Discard); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}

func TestSelectSourceUnlinked(t *testing.T) {
	cfg, err := config.Parse([]byte(`accounts: {sources: {
shared@example.com: {type: pop3, server: localhost, user: u, password: p},
shared@other.example: {type: imap, server: localhost, user: u, password: p}
}}`), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range cfg.Accounts.Sources {
		got, err := selectSource(cfg, name)
		if err != nil || got != want {
			t.Fatalf("selectSource(%q) = %v, %v; want stored unlinked source", name, got, err)
		}
		if got.Name != name || got.Account != "" {
			t.Errorf("unlinked source identity = %+v", got)
		}
	}
	for _, name := range []string{"shared", "missing@example.com", "a@gmail.com/shared@example.com"} {
		if _, err := selectSource(cfg, name); err == nil || !strings.Contains(err.Error(), "unknown source") {
			t.Errorf("selectSource(%q) error = %v; want unknown source", name, err)
		}
	}
}

func TestSourceCommand(t *testing.T) {
	for _, test := range []struct {
		name    string
		stalled bool
		routed  bool
	}{
		{name: "routed read-only", routed: true},
		{name: "unlinked read-only"},
		{name: "routed timeout", stalled: true, routed: true},
		{name: "unlinked timeout", stalled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			done := make(chan []string, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					done <- nil
					return
				}
				defer connection.Close()
				if test.stalled {
					io.Copy(io.Discard, connection)
					done <- nil
					return
				}
				fmt.Fprint(connection, "+OK ready\r\n")
				reader := bufio.NewReader(connection)
				var commands []string
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						break
					}
					command := strings.Fields(line)[0]
					commands = append(commands, command)
					if command == "UIDL" {
						fmt.Fprint(connection, "+OK\r\n1 uid-one\r\n.\r\n")
					} else {
						fmt.Fprint(connection, "+OK\r\n")
					}
					if command == "QUIT" {
						break
					}
				}
				done <- commands
			}()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			data := fmt.Sprintf(`accounts: {
sources: {local@example.com: {type: pop3, server: 127.0.0.1, port: %d, tls: none, user: u, password: p}}
}
`, listener.Addr().(*net.TCPAddr).Port)
			if test.routed {
				data = fmt.Sprintf(`accounts: {
targets: {a@gmail.com: {}},
sources: {local@example.com: {type: pop3, server: 127.0.0.1, port: %d, tls: none, user: u, password: p}}
}
routes: {local@example.com: {target: a@gmail.com, delete_after_import: true, initial_sync: all}}
`, listener.Addr().(*net.TCPAddr).Port)
			}
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
			var output bytes.Buffer
			err = runTest([]string{"--mode", "source", "--config", path, "--timeout", "1s", "local@example.com"}, &output)
			listener.Close()
			commands := <-done
			if test.stalled {
				if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
					t.Fatalf("unexpected timeout result: %v", err)
				}
			} else {
				if err != nil || !strings.Contains(output.String(), "OK source local@example.com:") {
					t.Fatalf("unexpected result: %v %s", err, output.String())
				}
				if strings.Join(commands, ",") != "USER,PASS,UIDL,QUIT" {
					t.Fatalf("unexpected commands: %v", commands)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "data")); !os.IsNotExist(err) {
				t.Fatal("connection test must not create data or state directories")
			}
		})
	}
}

func TestTargetCommandMissingTokenDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`accounts: {targets: {me@gmail.com: {}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_CLIENT_ID", "id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "secret")
	tokens := filepath.Join(dir, "tokens")
	err := runTest([]string{"--mode", "target", "--config", path, "--client-secret", "", "--token-dir", tokens, "me@gmail.com"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "token store:") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(tokens); !os.IsNotExist(err) {
		t.Fatal("check must not create a token directory")
	}
}
