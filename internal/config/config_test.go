package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExampleConfiguration(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data, func(string) (string, bool) { return "synthetic", true })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateService(); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const sample = `
defaults: {interval: 2m}
accounts: {
targets: {
me@gmail.com: {},
other@gmail.com: {method: imap, app_password: "${APP_PW}"}
},
sources: {
user@telekom.de: {type: pop3, server: pop.telekom.de, user: user, password: "${TELEKOM_PW}"},
user@gmx.net: {type: imap, server: imap.gmx.net, port: "${GMX_PORT}", user: u, password: "pa$$word", interval: 30s, timeout: 45s},
user@web.de: {type: imap, server: imap.web.de, user: u, password: p, folders: [INBOX, Archive]}
}
}
routes: {
user@telekom.de: {target: me@gmail.com},
user@gmx.net: {target: me@gmail.com, label: ""},
user@web.de: {target: other@gmail.com, label: Web Mail, delete_after_import: true, initial_sync: all}
}
`

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(sample), env(map[string]string{
		"TELEKOM_PW": "123", "GMX_PORT": "993", "APP_PW": "abcd efgh",
	}))
	if err != nil {
		t.Fatal(err)
	}
	me := cfg.Accounts.Targets["me@gmail.com"]
	if me.Method != MethodAPI || me.Email != "me@gmail.com" {
		t.Errorf("target = %+v", me)
	}
	sources := cfg.Sources()
	if len(sources) != 3 {
		t.Fatalf("sources = %d", len(sources))
	}
	gmx, tk, web := sources[0], sources[1], sources[2]
	if tk.Password != "123" || tk.TLS != TLSAuto || tk.Interval != 2*time.Minute || tk.Timeout != 10*time.Minute || tk.LabelName() != "user@telekom.de" || tk.InitialSync != InitialSyncNewOnly || tk.DeleteAfterImport {
		t.Errorf("telekom = %+v", tk)
	}
	if gmx.Port != 993 || gmx.Password != "pa$word" || gmx.LabelName() != "" || gmx.Interval != 30*time.Second || gmx.Timeout != 45*time.Second {
		t.Errorf("gmx = %+v", gmx)
	}
	if len(gmx.Folders) != 1 || gmx.Folders[0] != "INBOX" {
		t.Errorf("gmx folders = %v", gmx.Folders)
	}
	other := cfg.Accounts.Targets["other@gmail.com"]
	if other.AppPassword != "abcd efgh" || !web.DeleteAfterImport || len(web.Folders) != 2 || web.LabelName() != "Web Mail" || web.InitialSync != InitialSyncAll || web.Account != other.Email {
		t.Errorf("web route = %+v", web)
	}
	if id := gmx.ID(); id != "me@gmail.com/user@gmx.net" {
		t.Errorf("first id = %q", id)
	}
	for name, src := range cfg.Accounts.Sources {
		if src.Name != name || src.Account != "" || src.Label != nil || src.InitialSync != "" || src.DeleteAfterImport {
			t.Errorf("stored source changed by route compilation: %+v", src)
		}
	}
	if err := cfg.ValidateService(); err != nil {
		t.Fatal(err)
	}
	accounts := cfg.AccountList()
	if len(accounts) != 2 || accounts[0].Email != me.Email || accounts[1].Email != other.Email {
		t.Errorf("account list = %+v", accounts)
	}
}

func TestParseSetupWithoutRoutes(t *testing.T) {
	for name, data := range map[string]string{
		"empty":       `{}`,
		"empty maps":  `accounts: {targets: {}, sources: {}}`,
		"target only": `accounts: {targets: {me@gmail.com: {}}}`,
		"source only": `accounts: {sources: {user@example.com: {type: imap, server: s, user: u, password: p}}}`,
		"unlinked accounts": `accounts:
  targets: {me@gmail.com: {}}
  sources: {user@example.com: {type: pop3, server: s, user: u, password: p}}`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(data), env(nil))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Sources()) != 0 {
				t.Fatal("unlinked sources must not be polled")
			}
			if err := cfg.ValidateService(); err == nil || !strings.Contains(err.Error(), "at least one route") {
				t.Fatalf("service validation = %v", err)
			}
			if cfg.Defaults.Interval != 5*time.Minute || cfg.Defaults.Timeout != 10*time.Minute || cfg.Defaults.InitialSync != InitialSyncNewOnly {
				t.Errorf("defaults = %+v", cfg.Defaults)
			}
		})
	}
}

func TestParseSourceAndRouteDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
defaults: {interval: 1m, timeout: 2m, initial_sync: all}
accounts:
  targets: {me@gmail.com: {}, unused@gmail.com: {}}
  sources:
    user@example.com: {type: imap, server: s, user: u, password: p}
    unused@example.com: {type: pop3, server: s, user: u, password: p}
routes:
  user@example.com: {target: me@gmail.com}
`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	sources := cfg.Sources()
	if len(sources) != 1 {
		t.Fatalf("routed sources = %d", len(sources))
	}
	src := sources[0]
	if src.Interval != time.Minute || src.Timeout != 2*time.Minute || src.InitialSync != InitialSyncAll || src.LabelName() != "user@example.com" || src.DeleteAfterImport {
		t.Errorf("compiled source = %+v", src)
	}
	if err := cfg.ValidateService(); err != nil {
		t.Fatal(err)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"missing env": {sample, "TELEKOM_PW"},
		"unknown field": {`
accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p, delet_after_import: true}}}
`, `unknown field "accounts.sources.user@example.com.delet_after_import"`},
		"imap without app password": {`
accounts:
  targets:
    a@gmail.com: {method: imap}
`, "app_password is required"},
		"folders on pop3": {`
accounts:
  sources:
    user@example.com: {type: pop3, server: s, user: u, password: p, folders: [INBOX]}
`, "only supported for type imap"},
		"bad tls": {`
accounts:
  sources:
    user@example.com: {type: imap, server: s, user: u, password: p, tls: ssl}
`, "tls: invalid value"},
		"old schema":                {`accounts: {a@gmail.com: {sources: {x: {type: pop3, server: s, user: u, password: p}}}}`, `unknown field "accounts.a@gmail.com"`},
		"unknown root":              {`account: {}`, `unknown field "account"`},
		"unknown target":            {`accounts: {targets: {a@gmail.com: {interval: 1m}}}`, `unknown field "accounts.targets.a@gmail.com.interval"`},
		"unknown route":             {`routes: {user@example.com: {target: a@gmail.com, interval: 1m}}`, `unknown field "routes.user@example.com.interval"`},
		"route field on source":     {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p, label: Mail}}}`, `unknown field "accounts.sources.user@example.com.label"`},
		"runtime account on source": {`accounts: {sources: {user@example.com: {account: a@gmail.com}}}`, `unknown field "accounts.sources.user@example.com.account"`},
		"runtime name on source":    {`accounts: {sources: {user@example.com: {name: other@example.com}}}`, `unknown field "accounts.sources.user@example.com.name"`},
		"source initial sync":       {`accounts: {sources: {user@example.com: {initial_sync: all}}}`, `unknown field "accounts.sources.user@example.com.initial_sync"`},
		"source deletion":           {`accounts: {sources: {user@example.com: {delete_after_import: true}}}`, `unknown field "accounts.sources.user@example.com.delete_after_import"`},
		"missing source reference": {`accounts: {targets: {a@gmail.com: {}}}
routes: {user@example.com: {target: a@gmail.com}}`, "source and target must reference existing accounts"},
		"missing target reference": {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p}}}
routes: {user@example.com: {target: a@gmail.com}}`, "source and target must reference existing accounts"},
		"missing route target": {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p}}}
routes: {user@example.com: {}}`, "source and target must reference existing accounts"},
		"empty route":                  {`routes: {user@example.com: null}`, "empty route"},
		"empty target":                 {`accounts: {targets: {a@gmail.com: null}}`, "empty account"},
		"empty source":                 {`accounts: {sources: {user@example.com: null}}`, "empty source"},
		"invalid source email":         {`accounts: {sources: {legacy: {type: pop3, server: s, user: u, password: p}}}`, "key must be an email address"},
		"qualified source key":         {`accounts: {sources: {a@gmail.com/user@example.com: {type: pop3, server: s, user: u, password: p}}}`, "key must be an email address"},
		"invalid target email":         {`accounts: {targets: {legacy: {}}}`, "key must be a Gmail address"},
		"invalid method":               {`accounts: {targets: {a@gmail.com: {method: smtp}}}`, "method: invalid value"},
		"invalid type":                 {`accounts: {sources: {user@example.com: {type: smtp, server: s, user: u, password: p}}}`, "type: invalid value"},
		"missing source fields":        {`accounts: {sources: {user@example.com: {}}}`, "server: required"},
		"negative default timeout":     {`defaults: {timeout: -1s}`, "defaults.timeout must be positive"},
		"negative source timeout":      {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p, timeout: -1s}}}`, "timeout: must be positive"},
		"short default interval":       {`defaults: {interval: 9s}`, "defaults.interval must be at least 10s"},
		"short source interval":        {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p, interval: 9s}}}`, "interval: must be at least 10s"},
		"negative port":                {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p, port: -1}}}`, "port: out of range"},
		"large port":                   {`accounts: {sources: {user@example.com: {type: pop3, server: s, user: u, password: p, port: 65536}}}`, "port: out of range"},
		"invalid default initial sync": {`defaults: {initial_sync: old}`, "defaults.initial_sync: invalid value"},
		"invalid route initial sync": {`accounts:
  targets: {a@gmail.com: {}}
  sources: {user@example.com: {type: pop3, server: s, user: u, password: p}}
routes: {user@example.com: {target: a@gmail.com, initial_sync: old}}`, "initial_sync: invalid value"},
		"duplicate routes": {`accounts:
  targets: {a@gmail.com: {}, b@gmail.com: {}}
  sources: {user@example.com: {type: pop3, server: s, user: u, password: p}}
routes:
  user@example.com: {target: a@gmail.com}
  user@example.com: {target: b@gmail.com}`, "already defined"},
		"duplicate target keys": {`accounts:
  targets:
    a@gmail.com: {}
    a@gmail.com: {}`, "already defined"},
		"duplicate source keys": {`accounts:
  sources:
    user@example.com: {type: pop3, server: s, user: u, password: p}
    user@example.com: {type: imap, server: s, user: u, password: p}`, "already defined"},
		"duplicate route fields": {`routes: {user@example.com: {target: a@gmail.com, target: b@gmail.com}}`, "already defined"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml), env(nil))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
