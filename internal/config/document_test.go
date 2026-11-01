package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDocumentSavesBlockYAML(t *testing.T) {
	for name, original := range map[string]string{
		"new document":           "",
		"existing flow document": "# preserved header\n{accounts: {sources: {old@example.com: {type: pop3, server: localhost, user: u, password: '${PRETTY_PASSWORD}'}}}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PRETTY_PASSWORD", "synthetic")
			path := filepath.Join(t.TempDir(), "config.yaml")
			if original != "" {
				if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			doc, err := OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.AddAccount("targets", "me@gmail.com", map[string]any{"method": "api"}); err != nil {
				t.Fatal(err)
			}
			if err := doc.AddAccount("sources", "new@example.com", map[string]any{
				"type": "imap", "server": "localhost", "user": "u", "password": "${PRETTY_PASSWORD}", "folders": []string{"INBOX", "Archive"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := doc.Link("new@example.com", &Route{Target: "me@gmail.com"}); err != nil {
				t.Fatal(err)
			}
			before, err := doc.Config()
			if err != nil {
				t.Fatal(err)
			}
			if err := doc.Save(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "accounts:\n  targets:\n") && !strings.Contains(string(data), "accounts:\n  sources:\n") {
				t.Fatalf("expected two-space block indentation:\n%s", data)
			}
			if !strings.Contains(string(data), "${PRETTY_PASSWORD}") || original != "" && !strings.Contains(string(data), "# preserved header") {
				t.Fatal("formatting lost a reference or comment")
			}
			var root yaml.Node
			if err := yaml.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			var checkStyle func(*yaml.Node)
			checkStyle = func(node *yaml.Node) {
				if (node.Kind == yaml.MappingNode || node.Kind == yaml.SequenceNode) && node.Style&yaml.FlowStyle != 0 {
					t.Error("flow-style container retained")
				}
				for _, child := range node.Content {
					checkStyle(child)
				}
			}
			checkStyle(&root)
			after, err := Parse(data, os.LookupEnv)
			if err != nil {
				t.Fatal(err)
			}
			if len(before.Accounts.Sources) != len(after.Accounts.Sources) || after.Accounts.Sources["new@example.com"].Password != "synthetic" || len(after.Accounts.Sources["new@example.com"].Folders) != 2 {
				t.Fatal("formatting changed configuration values")
			}
		})
	}
}

func TestLoadEnvironment(t *testing.T) {
	t.Setenv("SETUP_LOAD_SECRET", "existing")
	t.Setenv("SETUP_LOAD_VALUE", "temporary")
	os.Unsetenv("SETUP_LOAD_VALUE")
	path := filepath.Join(t.TempDir(), ".env")
	data, err := marshalEnv(map[string]string{"SETUP_LOAD_SECRET": "ignored", "SETUP_LOAD_VALUE": "00123$word'\"\\\nend"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadEnvironment(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SETUP_LOAD_SECRET") != "existing" || os.Getenv("SETUP_LOAD_VALUE") != "00123$word'\"\\\nend" {
		t.Fatal("environment values changed")
	}
}

func TestDocumentSecretRoundTrip(t *testing.T) {
	for _, password := range []string{"00123", "pa$word'\"\\\nend", "00123$word'\""} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		doc, err := OpenDocument(path, "", false)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := doc.Password("sources", "me@example.com", password, "env")
		if err != nil {
			t.Fatal(err)
		}
		if err := doc.AddAccount("sources", "me@example.com", map[string]any{"type": "pop3", "server": "s", "user": "u", "password": stored}); err != nil {
			t.Fatal(err)
		}
		if err := doc.Save(); err != nil {
			t.Fatal(err)
		}
		reloaded, err := OpenDocument(path, "", false)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := reloaded.Config()
		if err != nil || cfg.Accounts.Sources["me@example.com"].Password != password {
			t.Fatalf("secret not preserved: %v", err)
		}
		for _, name := range []string{path, filepath.Join(filepath.Dir(path), ".env")} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("files must be private")
			}
		}
	}
}

func TestDocumentEnvSavedBeforeYAML(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original.yaml")
	if err := os.WriteFile(original, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(original, path); err != nil {
		t.Fatal(err)
	}
	doc, err := OpenDocument(path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := doc.Password("sources", "local@example.com", "synthetic", "env")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.AddAccount("sources", "local@example.com", map[string]any{"type": "pop3", "server": "localhost", "user": "u", "password": stored}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Save(); err == nil || !strings.Contains(err.Error(), "non-regular") {
		t.Fatalf("symlink save error = %v", err)
	}
	envData, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil || !bytes.Contains(envData, []byte("synthetic")) {
		t.Fatal("environment must be saved before YAML")
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "{}\n" {
		t.Fatal("save modified symlink target")
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("failed write retained lock")
	}
}

func TestDocumentPrivateWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "config.yaml")
	if err := WritePrivate(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	for name, permissions := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(name)
		if err != nil || info.Mode().Perm() != permissions {
			t.Fatal("private write has unsafe permissions")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatal("atomic replacement did not save contents")
	}
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, nonRegular := range []string{link, filepath.Dir(path)} {
		if err := WritePrivate(nonRegular, []byte("unsafe")); err == nil {
			t.Fatal("private write replaced a non-regular path")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatal("private write leaked temporary files")
	}
}

func TestDocumentGeneratedPasswordConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	doc, err := OpenDocument(path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := doc.Password("sources", "local@example.com", "first", "env")
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(stored, "${"), "}")
	if !strings.HasPrefix(name, "GMAIL_POPPERS_SOURCES_") {
		t.Fatal("password must use generated environment reference")
	}
	if _, err := doc.Password("sources", "local@example.com", "second", "env"); err == nil {
		t.Fatal("different password overwrote existing generated variable")
	}
	t.Setenv(name, "process-value")
	if _, err := doc.Password("sources", "local@example.com", "first", "env"); err == nil {
		t.Fatal("conflicting process environment password accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("password preparation wrote config")
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Fatal("password preparation wrote dotenv")
	}
}

func TestDocumentOptionalFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	doc, err := OpenDocument(path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := doc.Config()
	if err != nil || len(cfg.Accounts.Sources) != 0 || len(cfg.Accounts.Targets) != 0 {
		t.Fatalf("empty configuration: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("opening optional files must not create directories")
	}
	if _, err := OpenDocument(path, filepath.Join(filepath.Dir(path), ".env"), true); err == nil {
		t.Fatal("explicit missing env-file accepted")
	}
	if _, err := OpenDocument(path, path, true); err == nil {
		t.Fatal("identical config and env paths accepted")
	}
}

func TestDocumentProcessEnvironmentPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := "# preserved header\naccounts:\n  sources:\n    local@example.com: {type: pop3, server: localhost, user: u, password: '${SETUP_TEST_PASSWORD}'} # preserved source\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SETUP_TEST_PASSWORD=file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SETUP_TEST_PASSWORD", "process-value")
	doc, err := OpenDocument(path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := doc.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts.Sources["local@example.com"].Password != "process-value" {
		t.Fatal("dotenv overrode process environment")
	}
	if err := doc.AddAccount("targets", "me@gmail.com", map[string]any{"method": "api"}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# preserved header", "# preserved source", "${SETUP_TEST_PASSWORD}"} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("lost %q", want)
		}
	}
	t.Setenv("SETUP_TEST_PASSWORD", "")
	if value, ok := doc.Lookup("SETUP_TEST_PASSWORD"); !ok || value != "" {
		t.Fatal("explicit empty process value must override dotenv")
	}
}

func TestDocumentRejectedChangesDoNotWrite(t *testing.T) {
	for _, scenario := range []string{"invalid account", "duplicate account", "unknown source", "unknown target", "another target", "invalid route", "yaml conflict", "env conflict", "lock"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			envPath := filepath.Join(dir, ".env")
			original := []byte("accounts: {targets: {a@gmail.com: {}, b@gmail.com: {}}, sources: {local@example.com: {type: pop3, server: localhost, user: u, password: p}}}\nroutes: {local@example.com: {target: a@gmail.com}}\n")
			envData := []byte("KEEP=original\n")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(envPath, envData, 0o600); err != nil {
				t.Fatal(err)
			}
			doc, err := OpenDocument(path, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := doc.Password("sources", "new@example.com", "synthetic", "env"); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "invalid account":
				err = doc.AddAccount("sources", "new@example.com", map[string]any{"type": "invalid"})
				if err == nil {
					err = doc.Save()
				}
			case "duplicate account":
				err = doc.AddAccount("sources", "local@example.com", map[string]any{})
			case "unknown source":
				err = doc.Link("missing@example.com", &Route{Target: "a@gmail.com"})
			case "unknown target":
				err = doc.Link("local@example.com", &Route{Target: "missing@gmail.com"})
			case "another target":
				err = doc.Link("local@example.com", &Route{Target: "b@gmail.com"})
			case "invalid route":
				err = doc.Link("local@example.com", &Route{Target: "a@gmail.com", InitialSync: "invalid"})
				if err == nil {
					err = doc.Save()
				}
			case "yaml conflict":
				original = append(original, []byte("# concurrent edit\n")...)
				if err := os.WriteFile(path, original, 0o600); err != nil {
					t.Fatal(err)
				}
				err = doc.Save()
			case "env conflict":
				envData = []byte("KEEP=concurrent\n")
				if err := os.WriteFile(envPath, envData, 0o600); err != nil {
					t.Fatal(err)
				}
				err = doc.Save()
			case "lock":
				if err := os.WriteFile(path+".lock", []byte("existing lock"), 0o600); err != nil {
					t.Fatal(err)
				}
				err = doc.Save()
			}
			if err == nil {
				t.Fatal("invalid or conflicting change accepted")
			}
			for name, want := range map[string][]byte{path: original, envPath: envData} {
				got, err := os.ReadFile(name)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("rejected change modified %s: %v", filepath.Base(name), err)
				}
			}
			if scenario != "lock" {
				if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
					t.Fatal("save left a lock file")
				}
			}
		})
	}
}

func TestDocumentYAMLPasswordRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	doc, err := OpenDocument(path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	password := "00123$word${UNSET_TEST_VALUE}'\"\\\nend"
	stored, err := doc.Password("sources", "local@example.com", password, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "$$") {
		t.Fatal("YAML password dollars must be escaped")
	}
	if _, err := doc.Password("sources", "local@example.com", password, "invalid"); err == nil {
		t.Fatal("invalid storage accepted")
	}
	if err := doc.AddAccount("sources", "local@example.com", map[string]any{"type": "pop3", "server": "localhost", "user": "u", "password": stored}); err != nil {
		t.Fatal(err)
	}
	if err := doc.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenDocument(path, "", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := reloaded.Config()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Accounts.Sources["local@example.com"].Password != password {
		t.Fatal("literal YAML password changed")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".env")); !os.IsNotExist(err) {
		t.Fatal("YAML storage created dotenv")
	}
}
