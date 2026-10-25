package gmailapi

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func idToken(claims string) string {
	return "x." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".y"
}

func TestEmailFromIDToken(t *testing.T) {
	tok := (&oauth2.Token{}).WithExtra(map[string]any{"id_token": idToken(`{"email":"Me@Gmail.com","email_verified":true}`)})
	got, err := emailFromIDToken(tok)
	if err != nil || got != "Me@Gmail.com" {
		t.Fatalf("got %q, %v", got, err)
	}
	tok = (&oauth2.Token{}).WithExtra(map[string]any{"id_token": idToken(`{"email":"a@b.c","email_verified":false}`)})
	if _, err := emailFromIDToken(tok); err == nil {
		t.Fatal("unverified email accepted")
	}
	if _, err := emailFromIDToken(&oauth2.Token{}); err == nil {
		t.Fatal("missing id_token accepted")
	}
}

func TestTokenStore(t *testing.T) {
	dir := t.TempDir()
	s, err := NewTokenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("me@gmail.com"); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("err = %v", err)
	}
	if err := s.Save("Me@gmail.com", &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.Load("me@gmail.com")
	if err != nil || tok.RefreshToken != "r" || !s.Exists("me@gmail.com") {
		t.Fatalf("tok = %+v, err = %v", tok, err)
	}
	fi, err := os.Stat(s.path("me@gmail.com"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, err = %v", fi.Mode().Perm(), err)
	}
}

func TestDescribeInvalidGrant(t *testing.T) {
	err := describe(&oauth2.RetrieveError{ErrorCode: "invalid_grant"})
	if !strings.Contains(err.Error(), "re-authorize") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOAuthConfig(t *testing.T) {
	if _, err := LoadOAuthConfig("", "", ""); err == nil {
		t.Fatal("expected error without credentials")
	}
	cfg, err := LoadOAuthConfig("", "id", "secret")
	if err != nil || cfg.RedirectURL != RedirectURL || len(cfg.Scopes) != len(Scopes) {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
	a := NewAuth(cfg, nil)
	u, verifier := a.AuthURL("st", "me@gmail.com")
	for _, want := range []string{"state=st", "access_type=offline", "code_challenge=", "login_hint=me%40gmail.com", "gmail.insert"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth url missing %q: %s", want, u)
		}
	}
	if verifier == "" {
		t.Error("empty verifier")
	}
}
