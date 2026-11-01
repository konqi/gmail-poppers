package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink/gmailapi"

	"golang.org/x/oauth2"
)

func runAuth(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("auth", flag.ContinueOnError)
	flags.SetOutput(output)
	account := flags.String("account", "", "Gmail address to authorize (required)")
	options := localFlags(flags)
	noBrowser := flags.Bool("no-browser", false, "Print the URL without opening a browser")
	if err := parseLocal(flags, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || !config.ValidEmail(*account) {
		return errors.New("auth: provide --account with a Gmail address and no positional arguments")
	}
	if err := setupWritable(); err != nil {
		return err
	}
	doc, err := options.document()
	if err != nil {
		return err
	}
	cfg, err := options.oauth(doc)
	if err != nil {
		return err
	}
	store, err := gmailapi.NewTokenStore(options.tokenDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	launch := func(authURL string) {
		fmt.Fprintf(output, "Open this URL in a browser on this computer:\n%s\n\nWaiting for Google authorization (5 minutes)...\n", authURL)
		if !*noBrowser {
			if err := openBrowser(authURL); err != nil {
				fmt.Fprintln(output, "Could not open a browser automatically; open the URL above manually.")
			}
		}
	}
	if err := authorizeLocal(ctx, cfg, store, *account, launch); err != nil {
		return err
	}
	fmt.Fprintf(output, "Authorized %s. Token saved in %s. Keep token files private.\n", *account, options.tokenDir)
	return nil
}

func authorizeLocal(ctx context.Context, cfg *oauth2.Config, store *gmailapi.TokenStore, account string, launch func(string)) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("auth callback: %w", err)
	}
	localConfig := *cfg
	localConfig.RedirectURL = "http://" + listener.Addr().String() + "/"
	auth := gmailapi.NewAuth(&localConfig, store)
	state := oauth2.GenerateVerifier()
	authURL, verifier := auth.AuthURL(state, account)
	type callback struct {
		code string
		err  error
	}
	result := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		query := request.URL.Query()
		if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid authorization state.", http.StatusBadRequest)
			return
		}
		response := callback{code: query.Get("code")}
		if query.Get("error") != "" {
			response.err = errors.New("auth: Google authorization was denied; try again")
		} else if response.code == "" {
			http.Error(w, "Missing authorization code.", http.StatusBadRequest)
			return
		}
		select {
		case result <- response:
			fmt.Fprintln(w, "Authorization response received. Return to the terminal for the result.")
		default:
			http.Error(w, "Authorization response already received.", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	defer server.Close()
	launch(authURL)
	select {
	case <-ctx.Done():
		return fmt.Errorf("auth: %w", ctx.Err())
	case err := <-serverErrors:
		return fmt.Errorf("auth callback: %w", err)
	case response := <-result:
		if response.err != nil {
			return response.err
		}
		return auth.Exchange(ctx, response.code, verifier, account)
	}
}

func openBrowser(authURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", authURL)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", authURL)
	default:
		command = exec.Command("xdg-open", authURL)
	}
	if err := command.Start(); err != nil {
		return err
	}
	go command.Wait()
	return nil
}
