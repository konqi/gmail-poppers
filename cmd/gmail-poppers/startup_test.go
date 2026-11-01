package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func startupTestEnvironment(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"ENV_FILE", "CONFIG_PATH", "DATA_DIR", "TOKEN_DIR", "GOOGLE_CLIENT_SECRET_FILE", "GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET"} {
		t.Setenv(key, "")
	}
	t.Setenv("CONFIG_PATH", filepath.Join(dir, "config.yaml"))
	t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
	return dir
}

func writeStartupTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStartupIMAPReadyWithoutOAuth(t *testing.T) {
	dir := startupTestEnvironment(t)
	writeStartupTestFile(t, filepath.Join(dir, "config.yaml"), `accounts:
  targets: {me@gmail.com: {method: imap, app_password: synthetic}}
  sources: {source@example.com: {type: imap, server: localhost, user: user, password: synthetic}}
routes: {source@example.com: {target: me@gmail.com}}
`)
	cfg, options, err := startupConfig()
	if err != nil || cfg == nil || options == nil {
		t.Fatalf("IMAP preflight should not require OAuth: config present=%t, options present=%t, error=%v", cfg != nil, options != nil, err)
	}
	if err := cfg.ValidateService(); err != nil {
		t.Fatalf("expected runnable configuration: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("preflight changed local files: %v, %v", entries, err)
	}
}

func TestStartupInstructions(t *testing.T) {
	for _, test := range []struct {
		name, data string
		wants      []string
	}{
		{"missing config", "", []string{"add target", "add source", "link", "auth --account"}},
		{"missing source", "accounts: {targets: {me@gmail.com: {method: imap, app_password: synthetic}}}", []string{"add source", "link", "me@gmail.com"}},
		{"missing target", "accounts: {sources: {source@example.com: {type: pop3, server: localhost, user: user, password: synthetic}}}", []string{"add target", "link", "source@example.com"}},
		{"missing route", "accounts: {targets: {me@gmail.com: {}}, sources: {source@example.com: {type: pop3, server: localhost, user: user, password: synthetic}}}", []string{"link", "source@example.com", "me@gmail.com"}},
		{"missing oauth", "accounts: {targets: {me@gmail.com: {}}, sources: {source@example.com: {type: pop3, server: localhost, user: user, password: synthetic}}}\nroutes: {source@example.com: {target: me@gmail.com}}", []string{"Google OAuth client", "auth --account", "me@gmail.com"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			t.Setenv("CONFIG_PATH", path)
			t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
			t.Setenv("TOKEN_DIR", filepath.Join(dir, "tokens"))
			t.Setenv("GOOGLE_CLIENT_SECRET_FILE", filepath.Join(dir, "client_secret.json"))
			t.Setenv("GOOGLE_CLIENT_ID", "")
			t.Setenv("GOOGLE_CLIENT_SECRET", "")
			if test.data != "" {
				if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := startupConfig()
			var setup *setupRequired
			if !errors.As(err, &setup) {
				t.Fatalf("expected setup guidance: %v", err)
			}
			for _, want := range test.wants {
				if !strings.Contains(setup.message, want) {
					t.Errorf("missing %q: %s", want, setup.message)
				}
			}
			if strings.Contains(setup.message, "synthetic") {
				t.Fatal("credentials leaked in guidance")
			}
			for _, name := range []string{"data", "tokens"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatal("preflight must not create directories")
				}
			}
		})
	}
}

type startupTestTransport struct {
	requests atomic.Int64
}

func (transport *startupTestTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.requests.Add(1)
	return nil, errors.New("startup preflight must not make HTTP requests")
}

func forbidStartupHTTP(t *testing.T) {
	t.Helper()
	previous := http.DefaultTransport
	transport := &startupTestTransport{}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = previous
		if count := transport.requests.Load(); count != 0 {
			t.Errorf("preflight made %d HTTP requests", count)
		}
	})
}

func startupFileSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[name] = "directory"
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[name] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertStartupFilesUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, startupFileSnapshot(t, dir)) {
		t.Error("preflight changed local files or directories")
	}
}

func requireStartupGuidance(t *testing.T, err error, wants, forbidden []string) {
	t.Helper()
	var setup *setupRequired
	if !errors.As(err, &setup) {
		t.Fatalf("expected typed setup guidance, got %v", err)
	}
	for _, want := range wants {
		if !strings.Contains(setup.Error(), want) {
			t.Errorf("setup guidance is missing %q", want)
		}
	}
	for _, unwanted := range forbidden {
		if strings.Contains(setup.Error(), unwanted) {
			t.Errorf("setup guidance contains unwanted text %q", unwanted)
		}
	}
}

func TestStartupIncompleteHints(t *testing.T) {
	for _, test := range []struct {
		name, data       string
		wants, forbidden []string
	}{
		{"empty YAML", "", []string{"add target", "add source", "link", "For an API target", "auth --account"}, []string{"No configuration found"}},
		{"empty config", "{}", []string{"add target", "add source", "link", "auth --account"}, []string{"No configuration found"}},
		{"only IMAP target", "accounts: {targets: {me@gmail.com: {method: imap, app_password: synthetic}}}", []string{"add source", "link"}, []string{"add target", "auth --account", "Google Desktop"}},
		{"only source", "accounts: {sources: {source@example.com: {type: pop3, server: localhost, user: user, password: synthetic}}}", []string{"add target", "link"}, []string{"add source"}},
		{"only route missing", "accounts: {targets: {me@gmail.com: {method: imap, app_password: synthetic}}, sources: {source@example.com: {type: pop3, server: localhost, user: user, password: synthetic}}}", []string{"link", "source@example.com", "me@gmail.com"}, []string{"add target", "add source", "auth --account"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := startupTestEnvironment(t)
			writeStartupTestFile(t, os.Getenv("CONFIG_PATH"), test.data)
			before := startupFileSnapshot(t, dir)
			cfg, _, err := startupConfig()
			requireStartupGuidance(t, err, test.wants, append(test.forbidden, "synthetic"))
			if cfg != nil {
				t.Error("incomplete configuration must not be returned as runnable")
			}
			assertStartupFilesUnchanged(t, dir, before)
		})
	}
}

func TestStartupAPIReadiness(t *testing.T) {
	const validToken = `{"access_token":"synthetic-access-secret","refresh_token":"synthetic-refresh-secret","expiry":"2000-01-01T00:00:00Z"}`
	for _, test := range []struct {
		name, targets, routes string
		tokens                map[string]string
		missing               []string
		ready                 []string
	}{
		{"authorized API", "me@gmail.com: {}", "source@example.com: {target: me@gmail.com}", map[string]string{"me@gmail.com": validToken}, nil, nil},
		{"environment OAuth credentials", "me@gmail.com: {}", "source@example.com: {target: me@gmail.com}", map[string]string{"me@gmail.com": validToken}, nil, nil},
		{"unused API does not block IMAP", "me@gmail.com: {method: imap, app_password: synthetic}, unused@gmail.com: {}", "source@example.com: {target: me@gmail.com}", nil, nil, nil},
		{"unused API does not need token", "me@gmail.com: {}, unused@gmail.com: {}", "source@example.com: {target: me@gmail.com}", map[string]string{"me@gmail.com": validToken}, nil, nil},
		{"only relevant missing tokens", "me@gmail.com: {}, missing@gmail.com: {}, unused@gmail.com: {}", "source@example.com: {target: me@gmail.com}, second@example.com: {target: missing@gmail.com}", map[string]string{"me@gmail.com": validToken}, []string{"missing@gmail.com"}, []string{"me@gmail.com", "unused@gmail.com"}},
		{"multiple relevant missing tokens", "me@gmail.com: {}, second@gmail.com: {}, unused@gmail.com: {}", "source@example.com: {target: me@gmail.com}, second@example.com: {target: second@gmail.com}", nil, []string{"me@gmail.com", "second@gmail.com"}, []string{"unused@gmail.com"}},
		{"missing token directory", "me@gmail.com: {}", "source@example.com: {target: me@gmail.com}", nil, []string{"me@gmail.com"}, nil},
		{"corrupt token", "me@gmail.com: {}", "source@example.com: {target: me@gmail.com}", map[string]string{"me@gmail.com": `{"access_token":"synthetic-access-secret","refresh_token":synthetic-refresh-secret}`}, []string{"me@gmail.com"}, nil},
		{"refreshless token", "me@gmail.com: {}", "source@example.com: {target: me@gmail.com}", map[string]string{"me@gmail.com": `{"access_token":"synthetic-access-secret"}`}, []string{"me@gmail.com"}, nil},
		{"multiple authorized API targets", "me@gmail.com: {}, second@gmail.com: {}", "source@example.com: {target: me@gmail.com}, second@example.com: {target: second@gmail.com}", map[string]string{"me@gmail.com": validToken, "second@gmail.com": validToken}, nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := startupTestEnvironment(t)
			forbidStartupHTTP(t)
			writeStartupTestFile(t, os.Getenv("CONFIG_PATH"), fmt.Sprintf(`accounts:
  targets: {%s}
  sources:
    source@example.com: {type: imap, server: localhost, user: user, password: synthetic}
    second@example.com: {type: pop3, server: localhost, user: user, password: synthetic}
routes: {%s}
`, test.targets, test.routes))
			if test.name == "environment OAuth credentials" {
				t.Setenv("GOOGLE_CLIENT_ID", "synthetic-client-id")
				t.Setenv("GOOGLE_CLIENT_SECRET", "synthetic-client-secret")
			} else if test.name != "unused API does not block IMAP" {
				writeStartupTestFile(t, filepath.Join(dir, "client_secret.json"), `{"installed":{"client_id":"synthetic-client-id","client_secret":"synthetic-client-secret"}}`)
			}
			tokenDir := filepath.Join(dir, "data", "tokens")
			if test.tokens != nil {
				if err := os.MkdirAll(tokenDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for email, token := range test.tokens {
					writeStartupTestFile(t, filepath.Join(tokenDir, email+".token.json"), token)
				}
			}
			before := startupFileSnapshot(t, dir)
			cfg, options, err := startupConfig()
			if len(test.missing) == 0 {
				if err != nil || cfg == nil || options == nil {
					t.Fatalf("expected ready configuration, got error=%v", err)
				}
				if err := cfg.ValidateService(); err != nil {
					t.Fatalf("expected runnable configuration: %v", err)
				}
			} else {
				wants := []string{"auth --account", "--config " + strconv.Quote(os.Getenv("CONFIG_PATH")), "--token-dir " + strconv.Quote(tokenDir)}
				for _, email := range test.missing {
					wants = append(wants, "auth --account "+strconv.Quote(email))
				}
				forbidden := []string{"synthetic-access-secret", "synthetic-refresh-secret", "synthetic-client-secret", "Google Cloud setup"}
				forbidden = append(forbidden, test.ready...)
				requireStartupGuidance(t, err, wants, forbidden)
				if cfg != nil {
					t.Error("missing authorization must not return runnable configuration")
				}
			}
			assertStartupFilesUnchanged(t, dir, before)
		})
	}
}

func TestStartupOAuthClientGuidance(t *testing.T) {
	for _, test := range []struct{ name, client string }{
		{"missing", ""},
		{"malformed", `{"installed":{"client_secret":"synthetic-client-secret",`},
		{"missing fields", `{"installed":{"client_secret":"synthetic-client-secret"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := startupTestEnvironment(t)
			forbidStartupHTTP(t)
			writeStartupTestFile(t, os.Getenv("CONFIG_PATH"), `accounts: {targets: {me@gmail.com: {}}, sources: {source@example.com: {type: pop3, server: localhost, user: user, password: synthetic}}}
routes: {source@example.com: {target: me@gmail.com}}`)
			if test.client != "" {
				writeStartupTestFile(t, filepath.Join(dir, "client_secret.json"), test.client)
			}
			before := startupFileSnapshot(t, dir)
			_, _, err := startupConfig()
			requireStartupGuidance(t, err, []string{"Google OAuth client is missing or invalid", "Desktop app", "enable Gmail API", "https://console.cloud.google.com/auth/clients", filepath.Join(dir, "client_secret.json"), "auth --account \"me@gmail.com\""}, []string{"synthetic-client-secret", "synthetic"})
			assertStartupFilesUnchanged(t, dir, before)
		})
	}
}

func TestServiceOAuthPath(t *testing.T) {
	for _, test := range []struct{ name, file, id, secret, want string }{
		{"default file", "", "", "", "fallback.json"},
		{"environment credentials", "", "synthetic-id", "synthetic-secret", ""},
		{"explicit file wins", "explicit.json", "synthetic-id", "synthetic-secret", "fallback.json"},
		{"missing secret", "", "synthetic-id", "", "fallback.json"},
		{"missing ID", "", "", "synthetic-secret", "fallback.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GOOGLE_CLIENT_SECRET_FILE", test.file)
			t.Setenv("GOOGLE_CLIENT_ID", test.id)
			t.Setenv("GOOGLE_CLIENT_SECRET", test.secret)
			if got := serviceOAuthPath("fallback.json"); got != test.want {
				t.Errorf("serviceOAuthPath = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStartupPaths(t *testing.T) {
	for _, test := range []struct{ name, config, data, tokens, client, wantConfig, wantTokens, wantClient string }{
		{"local defaults", "", "", "", "", "./config.yaml", "./tokens", "client_secret.json"},
		{"explicit data", "", "custom-data", "", "", "./config.yaml", "custom-data/tokens", "client_secret.json"},
		{"client beside config", "settings/config.yaml", "", "", "", "settings/config.yaml", "./tokens", "settings/client_secret.json"},
		{"explicit paths", "settings/custom.yaml", "custom-data", "custom-tokens", "oauth/custom.json", "settings/custom.yaml", "custom-tokens", "oauth/custom.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := startupTestEnvironment(t)
			t.Chdir(dir)
			t.Setenv("CONFIG_PATH", test.config)
			t.Setenv("DATA_DIR", test.data)
			t.Setenv("TOKEN_DIR", test.tokens)
			t.Setenv("GOOGLE_CLIENT_SECRET_FILE", test.client)
			before := startupFileSnapshot(t, dir)
			cfg, options, err := startupConfig()
			requireStartupGuidance(t, err, []string{"No configuration found at " + test.wantConfig, "add target", "add source", "link", "auth --account"}, nil)
			if cfg != nil || options == nil {
				t.Fatalf("unexpected startup results: config present=%t, options present=%t", cfg != nil, options != nil)
			}
			if options.configPath != test.wantConfig || options.tokenDir != test.wantTokens || options.clientFile != test.wantClient {
				t.Errorf("paths = config %q, tokens %q, client %q; want %q, %q, %q", options.configPath, options.tokenDir, options.clientFile, test.wantConfig, test.wantTokens, test.wantClient)
			}
			assertStartupFilesUnchanged(t, dir, before)
		})
	}
}

func TestStartupConfigErrors(t *testing.T) {
	for _, test := range []struct{ name, data string }{
		{"malformed YAML", "accounts: ["},
		{"invalid configuration", "accounts: {targets: {me@gmail.com: {method: unsupported}}}"},
		{"directory instead of config", ""},
		{"permission denied", "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := startupTestEnvironment(t)
			path := os.Getenv("CONFIG_PATH")
			if test.name == "directory instead of config" {
				t.Setenv("CONFIG_PATH", dir)
			} else {
				writeStartupTestFile(t, path, test.data)
			}
			if test.name == "permission denied" {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(path, 0o600) })
				if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
					t.Skip("current user can read mode-000 files; permission check is not feasible")
				}
			}
			cfg, _, err := startupConfig()
			var setup *setupRequired
			if err == nil || errors.As(err, &setup) || cfg != nil {
				t.Fatalf("expected actual config error, not setup guidance: %v", err)
			}
			if (test.name != "invalid configuration" && !strings.HasPrefix(err.Error(), "config:")) || strings.Contains(err.Error(), "No configuration found") {
				t.Errorf("unexpected configuration error: %v", err)
			}
			if test.name == "permission denied" && !errors.Is(err, os.ErrPermission) {
				t.Errorf("permission error was not preserved: %v", err)
			}
		})
	}
}

func TestStartupMissingDotenvVariables(t *testing.T) {
	dir := startupTestEnvironment(t)
	const variable = "GMAIL_POPPERS_STARTUP_TEST_UNDEFINED_PASSWORD"
	t.Setenv(variable, "")
	if err := os.Unsetenv(variable); err != nil {
		t.Fatal(err)
	}
	writeStartupTestFile(t, os.Getenv("CONFIG_PATH"), `accounts: {targets: {me@gmail.com: {method: imap, app_password: "${GMAIL_POPPERS_STARTUP_TEST_UNDEFINED_PASSWORD}"}}}`)
	before := startupFileSnapshot(t, dir)
	_, _, err := startupConfig()
	requireStartupGuidance(t, err, []string{variable, "ENV_FILE=" + strconv.Quote(filepath.Join(dir, ".env")), "Portainer", "service has not started"}, []string{"No configuration found", "add target", "auth --account"})
	assertStartupFilesUnchanged(t, dir, before)
}

func TestRunSetupRequiredBeforeServiceInitialization(t *testing.T) {
	for _, mode := range []string{"invalid", "push"} {
		t.Run(mode, func(t *testing.T) {
			dir := startupTestEnvironment(t)
			forbidStartupHTTP(t)
			t.Setenv("GATUS_MODE", mode)
			t.Setenv("GATUS_URL", "")
			t.Setenv("GATUS_TOKEN", "")
			t.Setenv("HTTP_ADDR", "invalid-address")
			t.Setenv("DRY_RUN", "invalid")
			t.Setenv("TARGET_CONCURRENCY", "invalid")
			before := startupFileSnapshot(t, dir)
			var logs strings.Builder
			err := run(slog.New(slog.NewTextHandler(&logs, nil)))
			requireStartupGuidance(t, err, []string{"No configuration found", "add target", "add source", "link"}, []string{"GATUS_URL", "GATUS_MODE", "DRY_RUN", "web server"})
			if logs.Len() != 0 {
				t.Error("service initialization logged output before setup completed")
			}
			assertStartupFilesUnchanged(t, dir, before)
		})
	}
}
