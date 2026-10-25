package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gmail-poppers/internal/sink/gmailapi"

	"golang.org/x/oauth2"
)

func TestAuthorizeLocal(t *testing.T) {
	for _, accountMatches := range []bool{true, false} {
		t.Run(map[bool]string{true: "matching account", false: "wrong account"}[accountMatches], func(t *testing.T) {
			var expectedRedirect string
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.FormValue("redirect_uri") != expectedRedirect || request.FormValue("code_verifier") == "" || request.FormValue("code") != "test-code" {
					t.Error("token exchange missing redirect, verifier or code")
				}
				email := "me@gmail.com"
				if !accountMatches {
					email = "other@gmail.com"
				}
				claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `","email_verified":true}`))
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"access_token": "access", "refresh_token": "refresh", "token_type": "Bearer", "id_token": "x." + claims + ".y",
				})
			}))
			defer tokenServer.Close()
			cfg := &oauth2.Config{ClientID: "id", ClientSecret: "secret", Endpoint: oauth2.Endpoint{
				AuthURL: "https://accounts.example/authorize", TokenURL: tokenServer.URL, AuthStyle: oauth2.AuthStyleInParams,
			}}
			store, err := gmailapi.NewTokenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = authorizeLocal(ctx, cfg, store, "me@gmail.com", func(authURL string) {
				parsed, _ := url.Parse(authURL)
				query := parsed.Query()
				expectedRedirect = query.Get("redirect_uri")
				if !strings.HasPrefix(expectedRedirect, "http://127.0.0.1:") || query.Get("code_challenge") == "" {
					t.Fatal("expected loopback callback and PKCE")
				}
				for _, state := range []string{"wrong", query.Get("state")} {
					response, err := http.Get(expectedRedirect + "?" + url.Values{"state": {state}, "code": {"test-code"}}.Encode())
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if state == "wrong" && response.StatusCode != http.StatusBadRequest {
						t.Error("accepted invalid state")
					}
				}
			})
			if accountMatches {
				if err != nil {
					t.Fatal(err)
				}
				token, err := store.Load("me@gmail.com")
				if err != nil || token.RefreshToken != "refresh" {
					t.Fatalf("missing stored refresh token: %v", err)
				}
			} else if err == nil || store.Exists("me@gmail.com") {
				t.Fatal("wrong account must not be stored")
			}
		})
	}
}

func TestAuthorizeLocalCancellationAndDenial(t *testing.T) {
	for _, denied := range []bool{false, true} {
		store, _ := gmailapi.NewTokenStore(t.TempDir())
		ctx, cancel := context.WithCancel(context.Background())
		cfg := &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/authorize"}}
		err := authorizeLocal(ctx, cfg, store, "me@gmail.com", func(authURL string) {
			if !denied {
				cancel()
				return
			}
			parsed, _ := url.Parse(authURL)
			query := parsed.Query()
			response, err := http.Get(query.Get("redirect_uri") + "?" + url.Values{"state": {query.Get("state")}, "error": {"access_denied"}}.Encode())
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
		})
		cancel()
		if err == nil || store.Exists("me@gmail.com") {
			t.Fatal("cancelled or denied authorization must not store a token")
		}
	}
}

func TestAuthArguments(t *testing.T) {
	if err := runAuth([]string{"--help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--account", "../x@gmail.com"}, {"--account", "me@gmail.com", "extra"}} {
		if err := runAuth(args, io.Discard); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}
