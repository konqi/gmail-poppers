package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const sample = `
defaults:
  interval: 2m
accounts:
  me@gmail.com:
    sources:
      telekom:
        type: pop3
        server: pop.telekom.de
        user: user
        password: ${TELEKOM_PW}
      gmx:
        type: imap
        server: imap.gmx.net
        port: ${GMX_PORT}
        user: u
        password: "pa$$word"
        label: ""
        interval: 30s
  other@gmail.com:
    method: imap
    app_password: ${APP_PW}
    sources:
      web:
        type: imap
        server: imap.web.de
        user: u
        password: p
        folders: [INBOX, Archive]
        delete_after_import: true
        initial_sync: all
`

func TestParse(t *testing.T) {
	cfg, err := Parse([]byte(sample), env(map[string]string{
		"TELEKOM_PW": "123", "GMX_PORT": "993", "APP_PW": "abcd efgh",
	}))
	if err != nil {
		t.Fatal(err)
	}
	me := cfg.Accounts["me@gmail.com"]
	if me.Method != MethodAPI {
		t.Errorf("method default = %q", me.Method)
	}
	tk := me.Sources["telekom"]
	if tk.Password != "123" || tk.TLS != TLSAuto || tk.Interval != 2*time.Minute || tk.LabelName() != "telekom" || tk.InitialSync != InitialSyncNewOnly {
		t.Errorf("telekom = %+v", tk)
	}
	gmx := me.Sources["gmx"]
	if gmx.Port != 993 || gmx.Password != "pa$word" || gmx.LabelName() != "" || gmx.Interval != 30*time.Second {
		t.Errorf("gmx = %+v", gmx)
	}
	if len(gmx.Folders) != 1 || gmx.Folders[0] != "INBOX" {
		t.Errorf("gmx folders = %v", gmx.Folders)
	}
	other := cfg.Accounts["other@gmail.com"]
	if other.AppPassword != "abcd efgh" || !other.Sources["web"].DeleteAfterImport || len(other.Sources["web"].Folders) != 2 {
		t.Errorf("other = %+v", other)
	}
	if got := len(cfg.Sources()); got != 3 {
		t.Errorf("sources = %d", got)
	}
	if id := cfg.Sources()[0].ID(); id != "me@gmail.com/gmx" {
		t.Errorf("first id = %q", id)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"missing env": {sample, "TELEKOM_PW"},
		"unknown field": {`
accounts:
  a@gmail.com:
    sources:
      x: {type: pop3, server: s, user: u, password: p, delet_after_import: true}
`, `unknown field "accounts.a@gmail.com.sources.x.delet_after_import"`},
		"imap without app password": {`
accounts:
  a@gmail.com:
    method: imap
    sources:
      x: {type: pop3, server: s, user: u, password: p}
`, "app_password is required"},
		"folders on pop3": {`
accounts:
  a@gmail.com:
    sources:
      x: {type: pop3, server: s, user: u, password: p, folders: [INBOX]}
`, "only supported for type imap"},
		"bad tls": {`
accounts:
  a@gmail.com:
    sources:
      x: {type: imap, server: s, user: u, password: p, tls: ssl}
`, "tls: invalid value"},
		"no accounts": {`defaults: {interval: 1m}`, "at least one Gmail account"},
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
