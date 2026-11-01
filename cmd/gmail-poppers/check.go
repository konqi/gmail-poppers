package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink/gmailapi"
	"gmail-poppers/internal/sink/gmailimap"
	imapsrc "gmail-poppers/internal/source/imap"
	"gmail-poppers/internal/source/pop3"
)

func runTest(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(output)
	mode := flags.String("mode", "", "source or target (required)")
	configPath := flags.String("config", env("CONFIG_PATH", "/config/config.yaml"), "Service YAML configuration")
	tokenDir := flags.String("token-dir", env("TOKEN_DIR", filepath.Join(env("DATA_DIR", "/data"), "tokens")), "Existing OAuth token directory")
	clientFile := flags.String("client-secret", os.Getenv("GOOGLE_CLIENT_SECRET_FILE"), "Google Desktop app client JSON file")
	timeout := flags.Duration("timeout", 30*time.Second, "Connection check timeout")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if (*mode != "source" && *mode != "target") || flags.NArg() != 1 || *timeout <= 0 {
		return errors.New("test: use --mode source|target [options] <account-name> with a positive timeout")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	name := flags.Arg(0)
	var check func(context.Context) error
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *mode == "source" {
		selected, err := selectSource(cfg, name)
		if err != nil {
			return err
		}
		name = selected.ID()
		if selected.Type == config.TypePOP3 {
			check = pop3.New(selected, log).Check
		} else {
			check = imapsrc.New(selected, log).Check
		}
	} else {
		account, ok := cfg.Accounts[name]
		if !ok {
			return fmt.Errorf("test: unknown target account %q (use its Gmail address)", name)
		}
		if account.Method == config.MethodIMAP {
			check = gmailimap.New(account.Email, account.AppPassword, log).Check
		} else {
			oauthCfg, err := gmailapi.LoadOAuthConfig(*clientFile, os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"))
			if err != nil {
				return err
			}
			store, err := gmailapi.OpenTokenStore(*tokenDir)
			if err != nil {
				return err
			}
			check = gmailapi.New(account.Email, oauthCfg, store, log).Check
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if err := check(ctx); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("FAIL %s %s: %w", *mode, name, ctx.Err())
		}
		return fmt.Errorf("FAIL %s %s: %w", *mode, name, err)
	}
	fmt.Fprintf(output, "OK %s %s: authentication and read-only operation succeeded\n", *mode, name)
	return nil
}

func selectSource(cfg *config.Config, name string) (*config.Source, error) {
	account, sourceName, qualified := strings.Cut(name, "/")
	if qualified {
		if target := cfg.Accounts[account]; target != nil {
			if selected := target.Sources[sourceName]; selected != nil {
				return selected, nil
			}
		}
		return nil, fmt.Errorf("test: unknown source %q", name)
	}
	var selected *config.Source
	for _, source := range cfg.Sources() {
		if source.Name != name {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("test: source name %q is ambiguous; use <gmail-address>/<source-name>", name)
		}
		selected = source
	}
	if selected == nil {
		return nil, fmt.Errorf("test: unknown source %q", name)
	}
	return selected, nil
}
