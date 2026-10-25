// Package gmailapi writes messages via the Gmail API (users.messages.import).
package gmailapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// RedirectURL is a loopback address nobody listens on; the user copies the
// resulting URL from the browser back into the web UI.
const RedirectURL = "http://127.0.0.1:53682/"

var Scopes = []string{
	"https://www.googleapis.com/auth/gmail.insert",
	"https://www.googleapis.com/auth/gmail.labels",
	"openid",
	"email",
}

var ErrNotAuthorized = errors.New("account not authorized yet, open the web UI and click \"Authorize\"")

// LoadOAuthConfig reads a Google "Desktop app" client secret file, or uses id/secret if file is empty.
func LoadOAuthConfig(file, clientID, clientSecret string) (*oauth2.Config, error) {
	var cfg *oauth2.Config
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("oauth client secret: %w", err)
		}
		cfg, err = google.ConfigFromJSON(b, Scopes...)
		if err != nil {
			return nil, fmt.Errorf("oauth client secret: %w", err)
		}
	} else {
		if clientID == "" || clientSecret == "" {
			return nil, errors.New("oauth: set GOOGLE_CLIENT_SECRET_FILE or GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET")
		}
		cfg = &oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, Endpoint: google.Endpoint, Scopes: Scopes}
	}
	cfg.RedirectURL = RedirectURL
	return cfg, nil
}

// TokenStore keeps one OAuth token file per Gmail account.
type TokenStore struct {
	dir string
	mu  sync.Mutex
}

func NewTokenStore(dir string) (*TokenStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("token store: %w", err)
	}
	return &TokenStore{dir: dir}, nil
}

func (s *TokenStore) path(email string) string {
	return filepath.Join(s.dir, strings.ToLower(filepath.Base(email))+".token.json")
}

func (s *TokenStore) Load(email string) (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path(email))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotAuthorized
	}
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("token file: %w", err)
	}
	return &t, nil
}

func (s *TokenStore) Exists(email string) bool {
	_, err := os.Stat(s.path(email))
	return err == nil
}

func (s *TokenStore) Save(email string, t *oauth2.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := s.path(email) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(email))
}

// Auth runs the one-time authorization flow.
type Auth struct {
	cfg   *oauth2.Config
	store *TokenStore
}

func NewAuth(cfg *oauth2.Config, store *TokenStore) *Auth { return &Auth{cfg: cfg, store: store} }

// AuthURL returns the Google consent URL and the PKCE verifier to keep for Exchange.
func (a *Auth) AuthURL(state, email string) (string, string) {
	verifier := oauth2.GenerateVerifier()
	u := a.cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("login_hint", email),
	)
	return u, verifier
}

// Exchange trades the code for a token and stores it after checking the Google account matches.
func (a *Auth) Exchange(ctx context.Context, code, verifier, email string) error {
	tok, err := a.cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("oauth exchange: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("oauth exchange: Google returned no refresh token; remove the app's access at myaccount.google.com/permissions and try again")
	}
	got, err := emailFromIDToken(tok)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, email) {
		return fmt.Errorf("oauth: signed in as %s, but this authorization is for %s", got, email)
	}
	return a.store.Save(email, tok)
}

// emailFromIDToken reads the email claim; the token came straight from Google's token endpoint over TLS.
func emailFromIDToken(tok *oauth2.Token) (string, error) {
	raw, _ := tok.Extra("id_token").(string)
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("oauth: no id_token in response")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("oauth: id_token: %w", err)
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("oauth: id_token: %w", err)
	}
	if claims.Email == "" || !claims.EmailVerified {
		return "", errors.New("oauth: id_token has no verified email")
	}
	return claims.Email, nil
}

// savingTokenSource persists refreshed tokens.
type savingTokenSource struct {
	mu    sync.Mutex
	base  oauth2.TokenSource
	last  string
	save  func(*oauth2.Token) error
	onErr func(error)
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		if err := s.save(t); err != nil {
			s.onErr(err)
		}
	}
	return t, nil
}
