package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/gatus"
	"gmail-poppers/internal/runner"
	"gmail-poppers/internal/sink"
	"gmail-poppers/internal/sink/gmailapi"
	"gmail-poppers/internal/sink/gmailimap"
	"gmail-poppers/internal/source"
	imapsrc "gmail-poppers/internal/source/imap"
	"gmail-poppers/internal/source/pop3"
	"gmail-poppers/internal/state"
	"gmail-poppers/internal/status"
	"gmail-poppers/internal/web"

	"golang.org/x/oauth2"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "add" || os.Args[1] == "link") {
		var err error
		if os.Args[1] == "add" {
			err = runAdd(os.Args[2:], os.Stdout)
		} else {
			err = runLink(os.Args[2:], os.Stdout)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "test" {
		if err := runTest(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if len(os.Args) > 1 {
		fmt.Fprintln(os.Stderr, "Usage: gmail-poppers [add target|source <email> | link <source> <target> | test --mode source|target <email> | healthcheck]")
		os.Exit(1)
	}
	log := newLogger(env("LOG_LEVEL", "info"))
	if err := run(log); err != nil {
		var setup *setupRequired
		if errors.As(err, &setup) {
			fmt.Fprintln(os.Stderr, setup.message)
			os.Exit(1)
		}
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if path := os.Getenv("ENV_FILE"); path != "" {
		if err := config.LoadEnvironment(path); err != nil {
			return fmt.Errorf("ENV_FILE: %w", err)
		}
	}
	cfg, paths, err := startupConfig()
	if err != nil {
		return err
	}
	dataDir := env("DATA_DIR", "./data")
	if err := cfg.ValidateService(); err != nil {
		return err
	}
	dryRun, err := strconv.ParseBool(env("DRY_RUN", "false"))
	if err != nil {
		return fmt.Errorf("DRY_RUN: %w", err)
	}
	mode := env("GATUS_MODE", gatus.ModePull)
	if !gatus.ValidMode(mode) {
		return fmt.Errorf("GATUS_MODE: invalid value %q (want pull|push|both)", mode)
	}
	if gatus.PushEnabled(mode) && (os.Getenv("GATUS_URL") == "" || os.Getenv("GATUS_TOKEN") == "") {
		return errors.New("GATUS_URL and GATUS_TOKEN are required for GATUS_MODE push/both")
	}
	if dryRun {
		log.Warn("DRY RUN: sources and Gmail are only read, nothing is imported, labeled, deleted or saved")
	}

	tokens, err := gmailapi.NewTokenStore(paths.tokenDir)
	if err != nil {
		return err
	}
	sinks := buildSinks(cfg, tokens, log)

	reg := status.NewRegistry()
	concurrency, err := strconv.Atoi(env("TARGET_CONCURRENCY", "1"))
	if err != nil || concurrency < 1 {
		return errors.New("TARGET_CONCURRENCY must be a positive integer")
	}
	admissions := map[string]*runner.Admission{}
	for email := range cfg.Accounts.Targets {
		admissions[email] = runner.NewAdmission(concurrency)
	}
	userUnits, err := strconv.Atoi(env("GMAIL_USER_QUOTA_UNITS", "1500"))
	if err != nil {
		return errors.New("GMAIL_USER_QUOTA_UNITS must be an integer")
	}
	projectUnits, err := strconv.Atoi(env("GMAIL_PROJECT_QUOTA_UNITS", "30000"))
	if err != nil {
		return errors.New("GMAIL_PROJECT_QUOTA_UNITS must be an integer")
	}
	budget, err := gmailapi.NewBudget(userUnits, projectUnits)
	if err != nil {
		return err
	}
	for _, target := range sinks {
		if api, ok := target.(*gmailapi.Sink); ok {
			api.Budget = budget
		}
	}
	var reporters []runner.Reporter
	if gatus.PushEnabled(mode) {
		reporters = append(reporters, &gatus.Pusher{
			BaseURL: os.Getenv("GATUS_URL"),
			Token:   os.Getenv("GATUS_TOKEN"),
			Client:  &http.Client{Timeout: 15 * time.Second},
			Log:     log,
		})
		log.Info("add the following to your Gatus configuration:\n" + gatus.RenderExternalEndpoints(cfg))
	}

	var jobs []*runner.Job
	for _, s := range cfg.Sources() {
		slg := log.With("account", s.Account, "source", s.Name)
		st, err := state.Open(filepath.Join(dataDir, "state"), s.Name)
		if err != nil {
			return err
		}
		if err := st.BindTarget(s.Account); err != nil {
			return err
		}
		var src source.Source
		switch s.Type {
		case config.TypePOP3:
			src = pop3.New(s, slg)
		case config.TypeIMAP:
			src = imapsrc.New(s, slg)
		}
		reg.Register(s)
		jobs = append(jobs, &runner.Job{
			Admission: admissions[s.Account],
			Cfg:       s, Source: src, Sink: sinks[s.Account], State: st, DryRun: dryRun,
			Log: slg, Status: reg, Reporters: reporters,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr: env("HTTP_ADDR", ":8080"),
		Handler: (&web.Server{
			Status: reg, GatusMode: mode, DryRun: dryRun,
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		log.Info("web server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j.Run(ctx)
		}()
	}
	log.Info("started", "sources", len(jobs), "gatus_mode", mode, "dry_run", dryRun)

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		stop()
		wg.Wait()
		return fmt.Errorf("web server: %w", err)
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	wg.Wait()
	return nil
}

func buildSinks(cfg *config.Config, tokens *gmailapi.TokenStore, log *slog.Logger) map[string]sink.Sink {
	sinks := map[string]sink.Sink{}
	used := map[string]bool{}
	for _, route := range cfg.Routes {
		used[route.Target] = true
	}

	var oauthCfg *oauth2.Config
	needsOAuth := false
	for _, a := range cfg.Accounts.Targets {
		if !used[a.Email] {
			continue
		}
		needsOAuth = needsOAuth || a.Method == config.MethodAPI
	}
	if needsOAuth {
		oc, err := gmailapi.LoadOAuthConfig(serviceOAuthPath(env("GOOGLE_CLIENT_SECRET_FILE", filepath.Join(filepath.Dir(env("CONFIG_PATH", "./config.yaml")), "client_secret.json"))), os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"))
		if err != nil {
			log.Error("Gmail API accounts cannot work without an OAuth client", "error", err)
		} else {
			oauthCfg = oc
		}
	}

	for _, a := range cfg.Accounts.Targets {
		if !used[a.Email] {
			continue
		}
		alog := log.With("account", a.Email)
		switch {
		case a.Method == config.MethodIMAP:
			sinks[a.Email] = gmailimap.New(a.Email, a.AppPassword, alog)
		case oauthCfg != nil:
			s := gmailapi.New(a.Email, oauthCfg, tokens, alog)
			sinks[a.Email] = s
		default:
			sinks[a.Email] = failingSink{errors.New("gmail api: no OAuth client configured, set GOOGLE_CLIENT_SECRET_FILE or GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET")}
		}
	}
	return sinks
}

type failingSink struct{ err error }

func (f failingSink) Open(context.Context, bool) (sink.Session, error) { return nil, f.err }

func healthcheck() int {
	addr := env("HTTP_ADDR", ":8080")
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
