package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink/gmailapi"
)

type setupRequired struct{ message string }

func (e *setupRequired) Error() string { return e.message }

func startupConfig() (*config.Config, *localOptions, error) {
	dataDir := env("DATA_DIR", "./data")
	tokens := "./tokens"
	if os.Getenv("DATA_DIR") != "" {
		tokens = filepath.Join(dataDir, "tokens")
	}
	path := env("CONFIG_PATH", "./config.yaml")
	options := &localOptions{configPath: path, tokenDir: env("TOKEN_DIR", tokens),
		clientFile: env("GOOGLE_CLIENT_SECRET_FILE", filepath.Join(filepath.Dir(path), "client_secret.json"))}
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, options, setupInstructions(&config.Config{}, options, "No configuration found at "+path+".")
		}
		if strings.HasPrefix(err.Error(), "config: undefined environment variables:") {
			return nil, options, &setupRequired{message: err.Error() + "\n\nLoad the generated dotenv file or supply the variables through Portainer:\n  ENV_FILE=" +
				strconv.Quote(filepath.Join(filepath.Dir(path), ".env")) + " gmail-poppers\nThe service has not started."}
		}
		return nil, options, err
	}
	if len(cfg.Accounts.Targets) == 0 || len(cfg.Accounts.Sources) == 0 || len(cfg.Routes) == 0 {
		return nil, options, setupInstructions(cfg, options, "Setup is incomplete; the service has not started.")
	}
	used := map[string]bool{}
	for _, route := range cfg.Routes {
		used[route.Target] = true
	}
	var missing []string
	var apiTargets []string
	for email := range used {
		if cfg.Accounts.Targets[email].Method == config.MethodAPI {
			apiTargets = append(apiTargets, email)
		}
	}
	sort.Strings(apiTargets)
	if len(apiTargets) > 0 {
		if _, err := gmailapi.LoadOAuthConfig(serviceOAuthPath(options.clientFile), os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET")); err != nil {
			missing = append(missing, "Google OAuth client is missing or invalid. Create a Desktop app client, enable Gmail API, and download its JSON to "+options.clientFile+".",
				"Google Cloud setup: https://console.cloud.google.com/auth/clients")
		}
		store, err := gmailapi.OpenTokenStore(options.tokenDir)
		for _, email := range apiTargets {
			needsAuth := err != nil
			if !needsAuth {
				token, tokenErr := store.Load(email)
				needsAuth = tokenErr != nil || token.RefreshToken == ""
			}
			if needsAuth {
				missing = append(missing, "Authorize Gmail target "+email+" on your browser computer:", authInstruction(email, options))
			}
		}
	}
	if len(missing) > 0 {
		return nil, options, &setupRequired{message: "Setup is incomplete; the service has not started.\n\n" + strings.Join(missing, "\n") + "\n\nFor a container, transfer the resulting tokens and restart it. OAuth does not run inside the service."}
	}
	return cfg, options, nil
}

func authInstruction(email string, options *localOptions) string {
	return "  gmail-poppers add target " + strconv.Quote(email) + " --reauthorize --config " + strconv.Quote(options.configPath) +
		" --client-secret " + strconv.Quote(options.clientFile) + " --token-dir " + strconv.Quote(options.tokenDir)
}

func serviceOAuthPath(fallback string) string {
	if os.Getenv("GOOGLE_CLIENT_SECRET_FILE") == "" && os.Getenv("GOOGLE_CLIENT_ID") != "" && os.Getenv("GOOGLE_CLIENT_SECRET") != "" {
		return ""
	}
	return fallback
}

func setupInstructions(cfg *config.Config, options *localOptions, reason string) error {
	var lines []string
	lines = append(lines, reason, "", "Run these setup commands on your browser computer:")
	target := "you@gmail.com"
	source := "you@example.com"
	for _, account := range cfg.AccountList() {
		target = account.Email
		break
	}
	var sources []string
	for email := range cfg.Accounts.Sources {
		sources = append(sources, email)
	}
	sort.Strings(sources)
	if len(sources) > 0 {
		source = sources[0]
	}
	if len(cfg.Accounts.Targets) == 0 {
		lines = append(lines, "Add a Gmail target (the assistant guides you through OAuth or an IMAP app password):",
			"  gmail-poppers add target "+strconv.Quote(target)+" --config "+strconv.Quote(options.configPath)+
				" --client-secret "+strconv.Quote(options.clientFile)+" --token-dir "+strconv.Quote(options.tokenDir))
	}
	if len(cfg.Accounts.Sources) == 0 {
		lines = append(lines, "Add an external mail source (the assistant asks for protocol, server and credentials):",
			"  gmail-poppers add source "+strconv.Quote(source)+" --config "+strconv.Quote(options.configPath))
	}
	if len(cfg.Routes) == 0 {
		lines = append(lines, "Link the source to its Gmail target:",
			"  gmail-poppers link "+strconv.Quote(source)+" "+strconv.Quote(target)+" --config "+strconv.Quote(options.configPath))
	}
	lines = append(lines, "", "Replace example addresses with your own.")
	if len(cfg.Accounts.Targets) == 0 || cfg.Accounts.Targets[target].Method == config.MethodAPI {
		lines = append(lines, "For an API target, download a Google Desktop app OAuth client to "+options.clientFile+".",
			"The option is --client-secret; the default input filename is client_secret.json (with an underscore).",
			"If the target already exists but still needs OAuth, use:", authInstruction(target, options))
	}
	lines = append(lines, "", "Setup changes only local files; connection checks do not modify mailboxes.",
		"Start again after setup; if using the generated dotenv file:",
		"  ENV_FILE="+strconv.Quote(filepath.Join(filepath.Dir(options.configPath), ".env"))+" gmail-poppers")
	return &setupRequired{message: strings.Join(lines, "\n")}
}
