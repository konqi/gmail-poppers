package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/sink/gmailapi"
	"gmail-poppers/internal/sink/gmailimap"
	imapsrc "gmail-poppers/internal/source/imap"
	"gmail-poppers/internal/source/pop3"

	"golang.org/x/oauth2"
	"golang.org/x/term"
)

type localOptions struct {
	configPath, envFile, tokenDir, clientFile string
}

func localFlags(flags *flag.FlagSet) *localOptions {
	options := &localOptions{}
	flags.StringVar(&options.configPath, "config", env("CONFIG_PATH", "./config.yaml"), "Local YAML configuration")
	flags.StringVar(&options.envFile, "env-file", "", "Dotenv file (default: .env beside config)")
	flags.StringVar(&options.tokenDir, "token-dir", env("TOKEN_DIR", "./tokens"), "OAuth token directory")
	flags.StringVar(&options.clientFile, "client-secret", env("GOOGLE_CLIENT_SECRET_FILE", "./client_secret.json"), "Google Desktop app client JSON")
	return options
}

func parseLocal(flags *flag.FlagSet, args []string) error {
	var options, positional []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		options = append(options, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		entry := flags.Lookup(name)
		if entry == nil || hasValue {
			continue
		}
		boolean, ok := entry.Value.(interface{ IsBoolFlag() bool })
		if ok && boolean.IsBoolFlag() {
			continue
		}
		if index+1 == len(args) {
			return fmt.Errorf("missing value for --%s", name)
		}
		index++
		options = append(options, args[index])
	}
	return flags.Parse(append(options, positional...))
}

func setupWritable() error {
	if strings.EqualFold(os.Getenv("DRY_RUN"), "true") || os.Getenv("DRY_RUN") == "1" {
		return errors.New("setup: DRY_RUN prevents local configuration/token writes")
	}
	return nil
}

func (options *localOptions) document() (*config.Document, error) {
	return config.OpenDocument(options.configPath, options.envFile, options.envFile != "")
}

func (options *localOptions) oauth(doc *config.Document) (*oauth2.Config, error) {
	clientID, _ := doc.Lookup("GOOGLE_CLIENT_ID")
	clientSecret, _ := doc.Lookup("GOOGLE_CLIENT_SECRET")
	path := options.clientFile
	if _, err := os.Stat(path); os.IsNotExist(err) && clientID != "" {
		path = ""
	}
	return gmailapi.LoadOAuthConfig(path, clientID, clientSecret)
}

type setupPrompt struct {
	reader         *bufio.Reader
	output         io.Writer
	nonInteractive bool
}

func (p *setupPrompt) value(name, current, question, fallback string) (string, error) {
	if current != "" {
		return current, nil
	}
	if p.nonInteractive {
		if fallback != "" {
			return fallback, nil
		}
		return "", fmt.Errorf("setup: missing --%s in non-interactive mode", name)
	}
	for {
		fmt.Fprintf(p.output, "%s [%s]: ", question, fallback)
		line, err := p.reader.ReadString('\n')
		if err != nil {
			return "", errors.New("setup: input cancelled")
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = fallback
		}
		if line != "" {
			return line, nil
		}
	}
}

func (p *setupPrompt) choice(name, current, question, fallback string, choices ...string) (string, error) {
	for {
		value, err := p.value(name, current, question, fallback)
		if err != nil {
			return "", err
		}
		for _, choice := range choices {
			if value == choice {
				return value, nil
			}
		}
		if current != "" || p.nonInteractive {
			return "", fmt.Errorf("setup: invalid --%s", name)
		}
		fmt.Fprintf(p.output, "Choose one of: %s\n", strings.Join(choices, ", "))
	}
}

func (p *setupPrompt) secret(password, variable, file string, doc *config.Document) (string, error) {
	count := 0
	for _, value := range []string{password, variable, file} {
		if value != "" {
			count++
		}
	}
	if count > 1 {
		return "", errors.New("setup: use only one of --password, --password-env or --password-file")
	}
	if variable != "" {
		value, ok := doc.Lookup(variable)
		if !ok || value == "" {
			return "", errors.New("setup: password environment variable is missing or empty")
		}
		return value, nil
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", errors.New("setup: password file could not be read")
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	}
	if password != "" {
		return password, nil
	}
	if p.nonInteractive {
		return "", errors.New("setup: missing --password-env or --password-file (or --password) in non-interactive mode")
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("setup: hidden password input needs a terminal; use --password-env or --password-file")
	}
	fmt.Fprint(p.output, "Password (hidden): ")
	data, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(p.output)
	if err != nil || len(data) == 0 {
		return "", errors.New("setup: password is required")
	}
	return string(data), nil
}

func runAdd(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: gmail-poppers add target|source <email> [options]")
	}
	kind := args[0]
	if kind != "target" && kind != "source" {
		return errors.New("usage: gmail-poppers add target|source <email> [options]")
	}
	flags := flag.NewFlagSet("add "+kind, flag.ContinueOnError)
	flags.SetOutput(output)
	options := localFlags(flags)
	mode := flags.String("mode", "", "Target method api|imap or source protocol pop3|imap")
	sourceType := flags.String("type", "", "Source protocol pop3|imap")
	server := flags.String("server", "", "Source server hostname")
	port := flags.Int("port", 0, "Source port (0: auto)")
	tlsMode := flags.String("tls", "auto", "Source TLS auto|implicit|starttls|none")
	user := flags.String("user", "", "Source username (default: account email)")
	password := flags.String("password", "", "Password (prefer --password-env or --password-file)")
	passwordEnv := flags.String("password-env", "", "Environment variable containing the password")
	passwordFile := flags.String("password-file", "", "File containing the password")
	storage := flags.String("secret-storage", "", "Password storage env|yaml (default: env)")
	folders := flags.String("folders", "INBOX", "Comma-separated IMAP source folders")
	interval := flags.Duration("interval", 0, "Source interval (default: config defaults or 5m)")
	timeout := flags.Duration("timeout", 0, "Source run timeout (default: config defaults or 10m)")
	checkTimeout := flags.Duration("check-timeout", 30*time.Second, "Read-only validation timeout")
	nonInteractive := flags.Bool("non-interactive", false, "Never prompt; fail when required information is missing; OAuth token must already exist")
	noBrowser := flags.Bool("no-browser", false, "Print OAuth URL without opening a browser")
	reauthorize := flags.Bool("reauthorize", false, "Renew OAuth for an existing API target without changing configuration")
	if err := parseLocal(flags, args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 1 || !config.ValidEmail(flags.Arg(0)) {
		return errors.New("add: exactly one email address is required")
	}
	if *checkTimeout <= 0 || *interval < 0 || *timeout < 0 {
		return errors.New("add: durations must be positive")
	}
	if err := setupWritable(); err != nil {
		return err
	}
	email := flags.Arg(0)
	doc, err := options.document()
	if err != nil {
		return err
	}
	prompt := &setupPrompt{reader: bufio.NewReader(os.Stdin), output: output, nonInteractive: *nonInteractive}
	current, err := doc.Config()
	if err != nil {
		return err
	}
	if *reauthorize {
		if kind != "target" || current.Accounts.Targets[email] == nil || current.Accounts.Targets[email].Method != config.MethodAPI {
			return errors.New("add: --reauthorize requires an existing API target")
		}
		if *nonInteractive {
			return errors.New("add: --reauthorize requires interactive OAuth")
		}
		if *mode != "" && *mode != config.MethodAPI {
			return errors.New("add: --reauthorize cannot change the target method")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var check func(context.Context) error
	var pendingToken []byte
	var originalToken []byte
	if kind == "source" {
		if *sourceType != "" && *mode != "" && *sourceType != *mode {
			return errors.New("add: --mode and --type disagree")
		}
		protocol := *sourceType
		if protocol == "" {
			protocol = *mode
		}
		protocol, err = prompt.choice("type", protocol, "Source protocol (pop3/imap)", "", "pop3", "imap")
		if err != nil {
			return err
		}
		host, err := prompt.value("server", *server, "Mail server (from your provider's POP3/IMAP settings)", "")
		if err != nil {
			return err
		}
		secret, err := prompt.secret(*password, *passwordEnv, *passwordFile, doc)
		if err != nil {
			return err
		}
		place, err := prompt.choice("secret-storage", *storage, "Store password in env or yaml", "env", "env", "yaml")
		if err != nil {
			return err
		}
		stored, err := doc.Password("sources", email, secret, place)
		if err != nil {
			return err
		}
		username := *user
		if username == "" {
			username = email
		}
		definition := map[string]any{"type": protocol, "server": host, "port": *port, "tls": *tlsMode, "user": username, "password": stored}
		if protocol == config.TypeIMAP {
			definition["folders"] = splitFolders(*folders)
		}
		if *interval != 0 {
			definition["interval"] = interval.String()
		}
		if *timeout != 0 {
			definition["timeout"] = timeout.String()
		}
		if err := doc.AddAccount("sources", email, definition); err != nil {
			return err
		}
		cfg, err := doc.Config()
		if err != nil {
			return err
		}
		source := cfg.Accounts.Sources[email]
		if protocol == config.TypePOP3 {
			check = pop3.New(source, log).Check
		} else {
			check = imapsrc.New(source, log).Check
		}
	} else {
		selectedMode := *mode
		if *reauthorize {
			selectedMode = config.MethodAPI
		}
		method, err := prompt.choice("mode", selectedMode, "Gmail method (api/imap)", "api", "api", "imap")
		if err != nil {
			return err
		}
		definition := map[string]any{"method": method}
		if method == config.MethodIMAP {
			if !*nonInteractive {
				fmt.Fprintln(output, "Gmail IMAP requires 2-step verification and an app password: https://myaccount.google.com/apppasswords")
			}
			secret, err := prompt.secret(*password, *passwordEnv, *passwordFile, doc)
			if err != nil {
				return err
			}
			place, err := prompt.choice("secret-storage", *storage, "Store app password in env or yaml", "env", "env", "yaml")
			if err != nil {
				return err
			}
			stored, err := doc.Password("targets", email, secret, place)
			if err != nil {
				return err
			}
			definition["app_password"] = stored
		}
		if !*reauthorize {
			if err := doc.AddAccount("targets", email, definition); err != nil {
				return err
			}
		}
		cfg, err := doc.Config()
		if err != nil {
			return err
		}
		if method == config.MethodIMAP {
			check = gmailimap.New(email, cfg.Accounts.Targets[email].AppPassword, log).Check
		} else {
			oauthCfg, err := options.oauth(doc)
			if err != nil && !*nonInteractive {
				fmt.Fprintln(output, "Create a Google Cloud project, enable Gmail API, and download a Desktop app OAuth client: https://console.cloud.google.com/auth/clients")
				options.clientFile, err = prompt.value("client-secret", "", "OAuth client JSON path", options.clientFile)
				if err == nil {
					oauthCfg, err = options.oauth(doc)
				}
			}
			if err != nil {
				return err
			}
			store, storeErr := gmailapi.OpenTokenStore(options.tokenDir)
			if *reauthorize || storeErr != nil || !store.Exists(email) {
				if *nonInteractive {
					return errors.New("add: non-interactive OAuth requires an existing token; run add target interactively first")
				}
				if *reauthorize {
					originalToken, err = os.ReadFile(filepath.Join(options.tokenDir, strings.ToLower(email)+".token.json"))
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						return err
					}
				}
				temporary, err := os.MkdirTemp("", "gmail-poppers-auth-*")
				if err != nil {
					return err
				}
				defer os.RemoveAll(temporary)
				store, err = gmailapi.NewTokenStore(temporary)
				if err != nil {
					return err
				}
				authCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				err = authorizeLocal(authCtx, oauthCfg, store, email, func(authURL string) {
					fmt.Fprintf(output, "Authorize %s on this computer:\n%s\n", email, authURL)
					if !*noBrowser {
						if err := openBrowser(authURL); err != nil {
							fmt.Fprintln(output, "Open the URL manually in your browser.")
						}
					}
				})
				cancel()
				if err != nil {
					return err
				}
				pendingToken, err = os.ReadFile(filepath.Join(temporary, strings.ToLower(email)+".token.json"))
				if err != nil {
					return err
				}
			}
			check = gmailapi.New(email, oauthCfg, store, log).Check
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, *checkTimeout)
	defer cancel()
	if err := check(checkCtx); err != nil {
		return fmt.Errorf("add: connection check failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(pendingToken) > 0 {
		path := filepath.Join(options.tokenDir, strings.ToLower(email)+".token.json")
		if *reauthorize {
			if err := doc.Unchanged(); err != nil {
				return err
			}
			currentToken, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if !bytes.Equal(currentToken, originalToken) || (currentToken == nil) != (originalToken == nil) {
				return errors.New("add: token file changed during authorization; refusing to overwrite")
			}
		} else if _, err := os.Stat(path); err == nil {
			return errors.New("add: token file appeared during setup; refusing to overwrite")
		}
		if err := config.WritePrivate(path, pendingToken); err != nil {
			return err
		}
	}
	if *reauthorize {
		fmt.Fprintf(output, "Reauthorized target %s. Configuration and routes are unchanged; restart the service after installing the token.\n", email)
		return nil
	}
	if err := doc.Save(); err != nil {
		return fmt.Errorf("add: local save failed (a token or env-file may already have been saved): %w", err)
	}
	fmt.Fprintf(output, "Added %s %s to %s after a read-only connection check.\n", kind, email, options.configPath)
	return nil
}

func runLink(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("link", flag.ContinueOnError)
	flags.SetOutput(output)
	options := localFlags(flags)
	label := flags.String("label", "", "Gmail label; an explicit empty value disables it")
	initial := flags.String("initial-sync", "", "new_only or all (default: config defaults)")
	remove := flags.Bool("delete-after-import", false, "Delete source messages after successful import")
	nonInteractive := flags.Bool("non-interactive", false, "Never prompt; use defaults for omitted route options")
	if err := parseLocal(flags, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: gmail-poppers link <source-email> <target-email> [options]")
	}
	if err := setupWritable(); err != nil {
		return err
	}
	doc, err := options.document()
	if err != nil {
		return err
	}
	cfg, err := doc.Config()
	if err != nil {
		return err
	}
	source, target := flags.Arg(0), flags.Arg(1)
	if cfg.Accounts.Sources[source] == nil || cfg.Accounts.Targets[target] == nil {
		return errors.New("link: source and target must exist")
	}
	route := &config.Route{Target: target}
	if old := cfg.Routes[source]; old != nil {
		copy := *old
		route = &copy
		route.Target = target
		if copy.Label != nil {
			literal := strings.ReplaceAll(*copy.Label, "$", "$$")
			route.Label = &literal
		}
	}
	prompt := &setupPrompt{reader: bufio.NewReader(os.Stdin), output: output}
	provided := map[string]bool{}
	flags.Visit(func(entry *flag.Flag) { provided[entry.Name] = true })
	if provided["label"] {
		literal := strings.ReplaceAll(*label, "$", "$$")
		route.Label = &literal
	} else if !*nonInteractive {
		value, err := prompt.value("label", "", "Label (- disables it)", source)
		if err != nil {
			return err
		}
		if value == "-" {
			value = ""
		}
		value = strings.ReplaceAll(value, "$", "$$")
		route.Label = &value
	}
	if provided["initial-sync"] {
		route.InitialSync = *initial
	}
	if provided["delete-after-import"] {
		route.DeleteAfterImport = *remove
	}
	if !*nonInteractive {
		if !provided["initial-sync"] {
			fallback := route.InitialSync
			if fallback == "" {
				fallback = cfg.Defaults.InitialSync
			}
			route.InitialSync, err = prompt.choice("initial-sync", "", "First import policy", fallback, "new_only", "all")
			if err != nil {
				return err
			}
		}
		if !provided["delete-after-import"] {
			answer, err := prompt.choice("delete-after-import", "", "Delete source mail after successful import", strconv.FormatBool(route.DeleteAfterImport), "true", "false")
			if err != nil {
				return err
			}
			route.DeleteAfterImport, _ = strconv.ParseBool(answer)
		}
	}
	if err := doc.Link(source, route); err != nil {
		return err
	}
	if err := doc.Save(); err != nil {
		return err
	}
	fmt.Fprintf(output, "Linked %s to %s in %s. No mail server was contacted.\n", source, target, options.configPath)
	return nil
}

func splitFolders(value string) []string {
	var folders []string
	for _, folder := range strings.Split(value, ",") {
		if folder = strings.TrimSpace(folder); folder != "" {
			folders = append(folders, folder)
		}
	}
	return folders
}
