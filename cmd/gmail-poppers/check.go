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
	options := localFlags(flags)
	timeout := flags.Duration("timeout", 30*time.Second, "Connection check timeout")
	if err := parseLocal(flags, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if (*mode != "source" && *mode != "target") || flags.NArg() != 1 || *timeout <= 0 {
		return errors.New("test: use --mode source|target [options] <account-name> with a positive timeout")
	}
	doc, err := options.document()
	if err != nil {
		return err
	}
	cfg, err := doc.Config()
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
		name = selected.Name
		if selected.Type == config.TypePOP3 {
			check = pop3.New(selected, log).Check
		} else {
			check = imapsrc.New(selected, log).Check
		}
	} else {
		account, ok := cfg.Accounts.Targets[name]
		if !ok {
			return fmt.Errorf("test: unknown target account %q (use its Gmail address)", name)
		}
		if account.Method == config.MethodIMAP {
			check = gmailimap.New(account.Email, account.AppPassword, log).Check
		} else {
			oauthCfg, err := options.oauth(doc)
			if err != nil {
				return err
			}
			store, err := gmailapi.OpenTokenStore(options.tokenDir)
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
	selected := cfg.Accounts.Sources[name]
	if selected == nil {
		return nil, fmt.Errorf("test: unknown source %q", name)
	}
	return selected, nil
}
