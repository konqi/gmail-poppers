package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink/gmailapi"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"golang.org/x/oauth2"
)

type setupPOP struct {
	port        int
	connections atomic.Int32
	commands    chan []string
}

func newSetupPOP(t *testing.T, behavior, user, password string, beforeUIDL ...func()) *setupPOP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &setupPOP{port: listener.Addr().(*net.TCPAddr).Port, commands: make(chan []string, 16)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			server.connections.Add(1)
			connection.SetDeadline(time.Now().Add(3 * time.Second))
			var commands []string
			if behavior == "stall" {
				io.Copy(io.Discard, connection)
			} else {
				fmt.Fprint(connection, "+OK ready\r\n")
				reader := bufio.NewReader(connection)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						break
					}
					command, argument, _ := strings.Cut(strings.TrimSpace(line), " ")
					commands = append(commands, command)
					switch command {
					case "USER":
						if argument != user {
							t.Error("unexpected POP3 username")
						}
						fmt.Fprint(connection, "+OK\r\n")
					case "PASS":
						if argument != password {
							t.Error("unexpected POP3 password")
						}
						if behavior == "reject" {
							fmt.Fprint(connection, "-ERR authentication rejected\r\n")
						} else {
							fmt.Fprint(connection, "+OK\r\n")
						}
					case "UIDL":
						for _, callback := range beforeUIDL {
							callback()
						}
						fmt.Fprint(connection, "+OK\r\n1 baseline-uid\r\n.\r\n")
					case "QUIT":
						fmt.Fprint(connection, "+OK\r\n")
					default:
						t.Errorf("connection check issued forbidden POP3 command %q", command)
						fmt.Fprint(connection, "-ERR forbidden\r\n")
					}
					if command == "QUIT" || command == "PASS" && behavior == "reject" {
						break
					}
				}
			}
			connection.Close()
			server.commands <- commands
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("POP3 server did not stop")
		}
	})
	return server
}

func (server *setupPOP) nextCommands(t *testing.T) string {
	t.Helper()
	select {
	case commands := <-server.commands:
		return strings.Join(commands, ",")
	case <-time.After(5 * time.Second):
		t.Fatal("POP3 session did not finish")
		return ""
	}
}

func setupSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[strings.TrimPrefix(path, dir)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertSetupSnapshot(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := setupSnapshot(t, dir)
	if len(before) != len(after) {
		t.Fatal("setup changed the local file set")
	}
	for name, value := range before {
		if after[name] != value {
			t.Errorf("setup modified %s", name)
		}
	}
}

func TestSetupLocalFlags(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("TOKEN_DIR", "")
	t.Setenv("GOOGLE_CLIENT_SECRET_FILE", "")
	flags := flag.NewFlagSet("local", flag.ContinueOnError)
	options := localFlags(flags)
	if options.configPath != "./config.yaml" || options.envFile != "" || options.tokenDir != "./tokens" || options.clientFile != "./client_secret.json" {
		t.Fatal("unexpected local path defaults")
	}
	t.Setenv("CONFIG_PATH", "custom.yaml")
	t.Setenv("TOKEN_DIR", "custom-tokens")
	t.Setenv("GOOGLE_CLIENT_SECRET_FILE", "custom-client.json")
	flags = flag.NewFlagSet("local", flag.ContinueOnError)
	options = localFlags(flags)
	if err := parseLocal(flags, []string{"source@example.com", "--config", "override.yaml", "target@gmail.com", "--env-file", "explicit.env"}); err != nil {
		t.Fatal(err)
	}
	if options.configPath != "override.yaml" || options.envFile != "explicit.env" || options.tokenDir != "custom-tokens" || options.clientFile != "custom-client.json" || strings.Join(flags.Args(), ",") != "source@example.com,target@gmail.com" {
		t.Fatal("interspersed flags or environment defaults lost")
	}
}

func TestSetupAddSourceThenTest(t *testing.T) {
	for _, test := range []struct{ name, protocolFlag, storage, passwordFlag string }{
		{"type env literal", "--type", "env", "--password"},
		{"mode env variable", "--mode", "env", "--password-env"},
		{"type yaml file", "--type", "yaml", "--password-file"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			password := "00123$word'\""
			secret := password
			if test.passwordFlag == "--password-env" {
				password = "dotenv-password"
				secret = "SETUP_SOURCE_PASSWORD"
				t.Setenv(secret, "")
				if err := os.Unsetenv(secret); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SETUP_SOURCE_PASSWORD=dotenv-password\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if test.passwordFlag == "--password-file" {
				secret = filepath.Join(dir, "password.txt")
				if err := os.WriteFile(secret, []byte(password+"\r\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			server := newSetupPOP(t, "success", "local@example.com", password)
			args := []string{"source", test.protocolFlag, "pop3", "local@example.com", "--config", path, "--server", "127.0.0.1", "--port", fmt.Sprint(server.port), "--tls", "none", test.passwordFlag, secret, "--interval", "45s", "--timeout", "2m", "--check-timeout", "1s", "--non-interactive"}
			if test.passwordFlag != "--password-env" {
				args = append(args, "--secret-storage", test.storage)
			}
			var output bytes.Buffer
			if err := runAdd(args, &output); err != nil {
				t.Fatal(err)
			}
			if server.nextCommands(t) != "USER,PASS,UIDL,QUIT" {
				t.Fatal("add source must only authenticate and list UIDLs")
			}
			if strings.Contains(output.String(), password) {
				t.Fatal("setup exposed password")
			}
			doc, err := config.OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := doc.Config()
			if err != nil {
				t.Fatal(err)
			}
			source := cfg.Accounts.Sources["local@example.com"]
			if source == nil || source.User != "local@example.com" || source.Password != password || source.Port != server.port || source.Interval != 45*time.Second || source.Timeout != 2*time.Minute || source.Account != "" || len(cfg.Routes) != 0 {
				t.Fatal("saved unlinked source differs from requested settings")
			}
			before := setupSnapshot(t, dir)
			t.Setenv("DATA_DIR", filepath.Join(dir, "state"))
			if err := runTest([]string{"local@example.com", "--mode", "source", "--config", path, "--timeout", "1s"}, &output); err != nil {
				t.Fatal(err)
			}
			if server.nextCommands(t) != "USER,PASS,UIDL,QUIT" {
				t.Fatal("test command fetched or modified mail")
			}
			assertSetupSnapshot(t, dir, before)
			for _, name := range []string{path, filepath.Join(dir, ".env")} {
				info, err := os.Stat(name)
				if test.storage == "yaml" && name != path {
					if !os.IsNotExist(err) {
						t.Fatal("YAML secret created an env-file")
					}
					continue
				}
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatal("setup files must be private")
				}
			}
			before = setupSnapshot(t, dir)
			if err := runAdd(args, io.Discard); err == nil || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("duplicate account error = %v", err)
			}
			if server.connections.Load() != 2 {
				t.Fatal("duplicate account contacted server")
			}
			assertSetupSnapshot(t, dir, before)
		})
	}
}

func TestSetupAddRejectsBeforeNetworkOrWrites(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"missing kind", nil, "usage:"},
		{"unknown kind", []string{"unknown"}, "usage:"},
		{"missing email", []string{"source", "--type", "pop3"}, "email"},
		{"invalid email", []string{"source", "invalid"}, "email"},
		{"extra email", []string{"source", "a@example.com", "b@example.com"}, "email"},
		{"headless", []string{"source", "a@example.com", "--headless"}, "headless"},
		{"missing flag value", []string{"source", "a@example.com", "--server"}, "missing value"},
		{"missing type", []string{"source", "a@example.com"}, "--type"},
		{"invalid type", []string{"source", "a@example.com", "--type", "smtp"}, "invalid --type"},
		{"aliases disagree", []string{"source", "a@example.com", "--type", "pop3", "--mode", "imap"}, "disagree"},
		{"missing server", []string{"source", "a@example.com", "--type", "pop3"}, "--server"},
		{"missing password", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1"}, "missing --password"},
		{"missing env password", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password-env", "SETUP_MISSING_PASSWORD"}, "missing or empty"},
		{"missing password file", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password-file", "missing-password-file"}, "could not be read"},
		{"conflicting passwords", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password", "p", "--password-env", "OTHER"}, "only one"},
		{"invalid storage", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password", "p", "--secret-storage", "invalid"}, "invalid --secret-storage"},
		{"invalid TLS", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password", "p", "--tls", "invalid"}, ".tls:"},
		{"invalid port", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password", "p", "--port", "65536"}, "out of range"},
		{"invalid interval", []string{"source", "a@example.com", "--type", "pop3", "--server", "127.0.0.1", "--password", "p", "--interval", "1s"}, "at least 10s"},
		{"negative timeout", []string{"source", "a@example.com", "--timeout", "-1s"}, "durations"},
		{"zero check timeout", []string{"source", "a@example.com", "--check-timeout", "0s"}, "durations"},
		{"invalid duration", []string{"source", "a@example.com", "--interval", "bad"}, "invalid value"},
		{"target missing password", []string{"target", "a@gmail.com", "--mode", "imap"}, "missing --password"},
		{"invalid target mode", []string{"target", "a@gmail.com", "--mode", "unknown"}, "invalid --mode"},
		{"missing explicit env file", []string{"source", "a@example.com", "--env-file", "missing-setup-env-file"}, "env-file does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			t.Setenv("SETUP_MISSING_PASSWORD", "")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte("# unchanged\n{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KEEP=value\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := setupSnapshot(t, dir)
			server := newSetupPOP(t, "success", "a@example.com", "p")
			args := append([]string(nil), test.args...)
			if len(args) != 0 {
				args = append(args, "--config", path, "--non-interactive", "--check-timeout", "100ms")
			}
			if test.name == "zero check timeout" {
				args = append(args, "--check-timeout", "0s")
			}
			if test.name == "missing flag value" {
				args = []string{"source", "a@example.com", "--config", path, "--non-interactive", "--server"}
			}
			if len(args) > 0 && args[0] == "source" && test.name != "invalid port" && test.name != "missing flag value" {
				args = append(args, "--port", fmt.Sprint(server.port))
			}
			err := runAdd(args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v; want %q", err, test.want)
			}
			if server.connections.Load() != 0 {
				t.Fatal("invalid setup contacted a server")
			}
			assertSetupSnapshot(t, dir, before)
		})
	}
}

func TestSetupAddSourceFailedCheckDoesNotSave(t *testing.T) {
	for _, behavior := range []string{"reject", "stall"} {
		t.Run(behavior, func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte("# unchanged\n{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("KEEP=value\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := setupSnapshot(t, dir)
			server := newSetupPOP(t, behavior, "local@example.com", "synthetic")
			err := runAdd([]string{"source", "local@example.com", "--config", path, "--type", "pop3", "--server", "127.0.0.1", "--port", fmt.Sprint(server.port), "--tls", "none", "--password", "synthetic", "--check-timeout", "100ms", "--non-interactive"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "connection check failed") {
				t.Fatalf("failed validation error = %v", err)
			}
			commands := server.nextCommands(t)
			if behavior == "reject" && commands != "USER,PASS" {
				t.Fatalf("failed authentication commands = %s", commands)
			}
			assertSetupSnapshot(t, dir, before)
		})
	}
}

type setupTransport func(*http.Request) (*http.Response, error)

func (transport setupTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestSetupAddAPITargetNonInteractive(t *testing.T) {
	for _, scenario := range []string{"existing token", "dotenv client", "missing token", "failed authentication"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			client := filepath.Join(dir, "client_secret.json")
			if err := os.WriteFile(client, []byte(`{"installed":{"client_id":"synthetic-id","client_secret":"synthetic-secret"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario == "dotenv client" {
				for _, name := range []string{"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"} {
					t.Setenv(name, "")
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("GOOGLE_CLIENT_ID=synthetic-id\nGOOGLE_CLIENT_SECRET=synthetic-secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				client = ""
			}
			tokens := filepath.Join(dir, "tokens")
			if scenario != "missing token" {
				store, err := gmailapi.NewTokenStore(tokens)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Save("me@gmail.com", &oauth2.Token{AccessToken: "synthetic-access", TokenType: "Bearer", RefreshToken: "synthetic-refresh", Expiry: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}
			before := setupSnapshot(t, dir)
			var requests atomic.Int32
			original := http.DefaultTransport
			http.DefaultTransport = setupTransport(func(request *http.Request) (*http.Response, error) {
				requests.Add(1)
				if request.Method != http.MethodGet || request.URL.String() != "https://gmail.googleapis.com/gmail/v1/users/me/labels" || request.Body != nil {
					t.Error("setup attempted OAuth or a Gmail write")
					return nil, errors.New("forbidden request")
				}
				if request.Header.Get("Authorization") != "Bearer synthetic-access" {
					t.Error("setup did not use existing token")
				}
				code, body := http.StatusOK, `{"labels":[]}`
				if scenario == "failed authentication" {
					code, body = http.StatusUnauthorized, `{"error":{"message":"synthetic authentication failure"}}`
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			err := runAdd([]string{"target", "--config", path, "me@gmail.com", "--token-dir", tokens, "--client-secret", client, "--check-timeout", "1s", "--non-interactive"}, io.Discard)
			if scenario == "missing token" || scenario == "failed authentication" {
				want := "connection check failed"
				if scenario == "missing token" {
					want = "existing token"
				}
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v; want %q", err, want)
				}
				assertSetupSnapshot(t, dir, before)
				if scenario == "missing token" && requests.Load() != 0 {
					t.Fatal("missing token started OAuth or network validation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			doc, err := config.OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := doc.Config()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Accounts.Targets["me@gmail.com"].Method != config.MethodAPI || len(cfg.Routes) != 0 || requests.Load() != 1 {
				t.Fatal("API target must default to api and perform one labels GET")
			}
			delete(before, "/config.yaml")
			after := setupSnapshot(t, dir)
			delete(after, "/config.yaml")
			if len(after) != len(before) {
				t.Fatal("API setup created unexpected files")
			}
			for name, value := range before {
				if after[name] != value {
					t.Errorf("API check modified %s", name)
				}
			}
			before = setupSnapshot(t, dir)
			if err := runTest([]string{"me@gmail.com", "--mode", "target", "--config", path, "--token-dir", tokens, "--client-secret", client, "--timeout", "1s"}, io.Discard); err != nil {
				t.Fatal(err)
			}
			assertSetupSnapshot(t, dir, before)
			if requests.Load() != 2 {
				t.Fatal("target check must only list labels")
			}
		})
	}
}

func TestSetupLink(t *testing.T) {
	for _, test := range []struct {
		name    string
		extra   []string
		label   *string
		initial string
		remove  bool
	}{
		{name: "defaults", initial: "all"},
		{name: "explicit empty", extra: []string{"--label=", "--initial-sync", "new_only", "--delete-after-import=false"}, label: func() *string { value := ""; return &value }(), initial: "new_only"},
		{name: "literal dollars", extra: []string{"--label", "Mail ${LABEL_LITERAL}", "--initial-sync", "all", "--delete-after-import=true"}, label: func() *string { value := "Mail ${LABEL_LITERAL}"; return &value }(), initial: "all", remove: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			server := newSetupPOP(t, "success", "u", "p")
			data := fmt.Sprintf("# preserved\ndefaults: {initial_sync: all}\naccounts: {targets: {me@gmail.com: {}}, sources: {local@example.com: {type: pop3, server: 127.0.0.1, port: %d, tls: none, user: u, password: p}}}\n", server.port)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			original := http.DefaultTransport
			http.DefaultTransport = setupTransport(func(*http.Request) (*http.Response, error) {
				t.Error("link contacted HTTP server")
				return nil, errors.New("forbidden")
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			args := []string{"local@example.com", "--config", path, "me@gmail.com", "--non-interactive"}
			if err := runLink(append(args, test.extra...), io.Discard); err != nil {
				t.Fatal(err)
			}
			if server.connections.Load() != 0 {
				t.Fatal("link contacted the source mail server")
			}
			doc, err := config.OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := doc.Config()
			if err != nil {
				t.Fatal(err)
			}
			route := cfg.Routes["local@example.com"]
			if route == nil || route.Target != "me@gmail.com" || route.InitialSync != test.initial || route.DeleteAfterImport != test.remove {
				t.Fatal("link settings differ from requested values")
			}
			if test.label == nil {
				if route.Label != nil {
					t.Fatal("omitted label must retain default")
				}
			} else if route.Label == nil || *route.Label != *test.label {
				t.Fatal("explicit label not preserved")
			}
			sources := cfg.Sources()
			if len(sources) != 1 || sources[0].Name != "local@example.com" || sources[0].Account != "me@gmail.com" || sources[0].InitialSync != test.initial || sources[0].DeleteAfterImport != test.remove {
				t.Fatal("link reversed source/target or lost route options")
			}
			if err := runLink(args, io.Discard); err != nil {
				t.Fatal(err)
			}
			if server.connections.Load() != 0 {
				t.Fatal("repeated link contacted the source mail server")
			}
			reloaded, err := config.OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := reloaded.Config()
			if err != nil {
				t.Fatal(err)
			}
			repeatedRoute := repeated.Routes["local@example.com"]
			if repeatedRoute.Target != route.Target || repeatedRoute.InitialSync != route.InitialSync || repeatedRoute.DeleteAfterImport != route.DeleteAfterImport {
				t.Fatal("omitted link options changed existing route")
			}
			if route.Label == nil {
				if repeatedRoute.Label != nil {
					t.Fatal("repeated link changed default label")
				}
			} else if repeatedRoute.Label == nil || *repeatedRoute.Label != *route.Label {
				t.Fatal("repeated link changed literal label")
			}
			if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
				t.Fatal("link created an env-file")
			}
		})
	}
}

func TestSetupAddSourceConcurrentEdit(t *testing.T) {
	t.Setenv("DRY_RUN", "false")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("KEEP=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newSetupPOP(t, "success", "local@example.com", "synthetic", func() {
		if err := os.WriteFile(path, []byte("# concurrent edit\n{}\n"), 0o600); err != nil {
			t.Error(err)
		}
	})
	err := runAdd([]string{"source", "local@example.com", "--config", path, "--type", "pop3", "--server", "127.0.0.1", "--port", fmt.Sprint(server.port), "--tls", "none", "--password", "synthetic", "--check-timeout", "1s", "--non-interactive"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "files changed") {
		t.Fatalf("concurrent edit error = %v", err)
	}
	if server.nextCommands(t) != "USER,PASS,UIDL,QUIT" {
		t.Fatal("validation must finish before save conflict")
	}
	for name, want := range map[string]string{path: "# concurrent edit\n{}\n", envPath: "KEEP=value\n"} {
		got, err := os.ReadFile(name)
		if err != nil || string(got) != want {
			t.Fatal("save overwrote concurrent edit or saved pending secrets")
		}
	}
}

type setupIMAPSession struct {
	imapserver.Session
	testing  *testing.T
	selected chan string
}

func (session setupIMAPSession) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if options == nil || !options.ReadOnly {
		session.testing.Error("setup selected an IMAP folder writable")
	}
	session.selected <- mailbox
	return session.Session.Select(mailbox, options)
}

func (session setupIMAPSession) Fetch(*imapserver.FetchWriter, imap.NumSet, *imap.FetchOptions) error {
	session.testing.Error("setup fetched IMAP message data")
	return errors.New("forbidden fetch")
}

func (session setupIMAPSession) Store(*imapserver.FetchWriter, imap.NumSet, *imap.StoreFlags, *imap.StoreOptions) error {
	session.testing.Error("setup changed IMAP flags")
	return errors.New("forbidden store")
}

func (session setupIMAPSession) Expunge(*imapserver.ExpungeWriter, *imap.UIDSet) error {
	session.testing.Error("setup expunged IMAP mail")
	return errors.New("forbidden expunge")
}

func (session setupIMAPSession) Append(string, imap.LiteralReader, *imap.AppendOptions) (*imap.AppendData, error) {
	session.testing.Error("setup appended IMAP mail")
	return nil, errors.New("forbidden append")
}

func (session setupIMAPSession) Copy(imap.NumSet, string) (*imap.CopyData, error) {
	session.testing.Error("setup copied IMAP mail")
	return nil, errors.New("forbidden copy")
}

func TestSetupAddIMAPSource(t *testing.T) {
	for _, test := range []struct{ name, password, folders, want string }{
		{"default folder", "synthetic", "", "INBOX"},
		{"multiple folders", "synthetic", " INBOX, Archive, ", "INBOX,Archive"},
		{"invalid authentication", "wrong", "INBOX", ""},
		{"missing folder", "synthetic", "missing", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			memory := imapmemserver.New()
			user := imapmemserver.NewUser("local@example.com", "synthetic")
			for _, mailbox := range []string{"INBOX", "Archive"} {
				if err := user.Create(mailbox, nil); err != nil {
					t.Fatal(err)
				}
			}
			memory.AddUser(user)
			selected := make(chan string, 8)
			server := imapserver.New(&imapserver.Options{
				NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
					return setupIMAPSession{Session: memory.NewSession(), testing: t, selected: selected}, nil, nil
				},
				Caps: imap.CapSet{imap.CapIMAP4rev1: {}}, InsecureAuth: true,
			})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); server.Serve(listener) }()
			t.Cleanup(func() {
				server.Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("IMAP server did not stop")
				}
			})
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			before := setupSnapshot(t, dir)
			args := []string{"source", "local@example.com", "--config", path, "--mode", "imap", "--server", "127.0.0.1", "--port", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port), "--tls", "none", "--password", test.password, "--check-timeout", "1s", "--non-interactive"}
			if test.folders != "" {
				args = append(args, "--folders", test.folders)
			}
			err = runAdd(args, io.Discard)
			if test.want == "" {
				if err == nil || !strings.Contains(err.Error(), "connection check failed") {
					t.Fatalf("invalid IMAP configuration error = %v", err)
				}
				assertSetupSnapshot(t, dir, before)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var mailboxes []string
			for range strings.Split(test.want, ",") {
				select {
				case mailbox := <-selected:
					mailboxes = append(mailboxes, mailbox)
				case <-time.After(time.Second):
					t.Fatal("missing read-only folder selection")
				}
			}
			if strings.Join(mailboxes, ",") != test.want {
				t.Fatal("setup checked wrong folders")
			}
			doc, err := config.OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := doc.Config()
			if err != nil {
				t.Fatal(err)
			}
			source := cfg.Accounts.Sources["local@example.com"]
			if strings.Join(source.Folders, ",") != test.want || source.Interval != 5*time.Minute || source.Timeout != 10*time.Minute {
				t.Fatal("IMAP folders or duration defaults not preserved")
			}
		})
	}
}

func TestSetupLinkRejectedDoesNotSave(t *testing.T) {
	for _, extra := range [][]string{
		{"local@example.com", "missing@gmail.com"},
		{"missing@example.com", "a@gmail.com"},
		{"local@example.com", "b@gmail.com"},
		{"local@example.com", "a@gmail.com", "--initial-sync", "invalid"},
		{"local@example.com", "a@gmail.com", "--headless"},
		{"local@example.com", "a@gmail.com", "--delete-after-import=invalid"},
		{"local@example.com"},
	} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			t.Setenv("DRY_RUN", "false")
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			data := "accounts: {targets: {a@gmail.com: {}, b@gmail.com: {}}, sources: {local@example.com: {type: pop3, server: localhost, user: u, password: p}}}\nroutes: {local@example.com: {target: a@gmail.com}}\n"
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			before := setupSnapshot(t, dir)
			args := append([]string{"--config", path, "--non-interactive"}, extra...)
			if err := runLink(args, io.Discard); err == nil {
				t.Fatal("invalid link accepted")
			}
			assertSetupSnapshot(t, dir, before)
		})
	}
}

func TestSetupDryRunWritesNothing(t *testing.T) {
	for _, value := range []string{"true", "TRUE", "1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DRY_RUN", value)
			dir := t.TempDir()
			path := filepath.Join(dir, "new", "config.yaml")
			before := setupSnapshot(t, dir)
			for _, kind := range []string{"source", "target"} {
				err := runAdd([]string{kind, "local@example.com", "--config", path, "--non-interactive"}, io.Discard)
				if err == nil || !strings.Contains(err.Error(), "DRY_RUN") {
					t.Fatalf("dry-run add error = %v", err)
				}
			}
			if err := runLink([]string{"local@example.com", "me@gmail.com", "--config", path, "--non-interactive"}, io.Discard); err == nil || !strings.Contains(err.Error(), "DRY_RUN") {
				t.Fatalf("dry-run link error = %v", err)
			}
			assertSetupSnapshot(t, dir, before)
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Fatal("dry-run created configuration directory")
			}
		})
	}
}
