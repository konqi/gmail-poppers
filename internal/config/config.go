package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const (
	MethodAPI  = "api"
	MethodIMAP = "imap"

	TypePOP3 = "pop3"
	TypeIMAP = "imap"

	TLSAuto     = "auto"
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
	TLSNone     = "none"

	InitialSyncNewOnly = "new_only"
	InitialSyncAll     = "all"
)

type Config struct {
	Defaults Defaults          `yaml:"defaults,omitempty"`
	Accounts Accounts          `yaml:"accounts"`
	Routes   map[string]*Route `yaml:"routes,omitempty"`
}

type Accounts struct {
	Targets map[string]*Account `yaml:"targets"`
	Sources map[string]*Source  `yaml:"sources"`
}

type Route struct {
	Target            string  `yaml:"target"`
	Label             *string `yaml:"label,omitempty"`
	InitialSync       string  `yaml:"initial_sync,omitempty"`
	DeleteAfterImport bool    `yaml:"delete_after_import,omitempty"`
}

type Defaults struct {
	Interval    time.Duration `yaml:"interval"`
	Timeout     time.Duration `yaml:"timeout"`
	InitialSync string        `yaml:"initial_sync"`
}

type Account struct {
	Email       string `yaml:"-"`
	Method      string `yaml:"method"`
	AppPassword string `yaml:"app_password"`
}

type Source struct {
	Name              string        `yaml:"-"`
	Account           string        `yaml:"-"`
	Type              string        `yaml:"type"`
	Server            string        `yaml:"server"`
	Port              int           `yaml:"port"`
	TLS               string        `yaml:"tls"`
	User              string        `yaml:"user"`
	Password          string        `yaml:"password"`
	Folders           []string      `yaml:"folders"`
	DeleteAfterImport bool          `yaml:"-"`
	Label             *string       `yaml:"-"`
	Interval          time.Duration `yaml:"interval"`
	Timeout           time.Duration `yaml:"timeout"`
	InitialSync       string        `yaml:"-"`
}

// ID uniquely identifies a source across all accounts.
func (s *Source) ID() string { return s.Account + "/" + s.Name }

// LabelName returns the Gmail label to apply; empty means no extra label.
func (s *Source) LabelName() string {
	if s.Label == nil {
		return s.Name
	}
	return *s.Label
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(data, os.LookupEnv)
}

func Parse(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var missing []string
	expandNode(&root, lookup, &missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("config: undefined environment variables: %s", strings.Join(dedupe(missing), ", "))
	}
	if err := checkKeys(&root, reflect.TypeOf(Config{}), ""); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var envPattern = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandNode replaces ${VAR} in scalar values (not keys); $$ yields a literal $.
func expandNode(n *yaml.Node, lookup func(string) (string, bool), missing *[]string) {
	switch n.Kind {
	case yaml.ScalarNode:
		if !strings.Contains(n.Value, "$") {
			return
		}
		n.Value = envPattern.ReplaceAllStringFunc(n.Value, func(m string) string {
			if m == "$$" {
				return "$"
			}
			name := m[2 : len(m)-1]
			v, ok := lookup(name)
			if !ok {
				*missing = append(*missing, name)
			}
			return v
		})
		// Re-resolve the type from the expanded value so ${PORT} can feed an int field.
		n.Tag = ""
		n.Style = 0
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			expandNode(n.Content[i], lookup, missing)
		}
	default:
		for _, c := range n.Content {
			expandNode(c, lookup, missing)
		}
	}
}

func (c *Config) normalize() error {
	if c.Defaults.Interval == 0 {
		c.Defaults.Interval = 5 * time.Minute
	}
	if c.Defaults.Timeout == 0 {
		c.Defaults.Timeout = 10 * time.Minute
	}
	if c.Defaults.InitialSync == "" {
		c.Defaults.InitialSync = InitialSyncNewOnly
	}

	var errs []error
	if c.Defaults.Interval < 10*time.Second {
		errs = append(errs, errors.New("defaults.interval must be at least 10s"))
	}
	if !oneOf(c.Defaults.InitialSync, InitialSyncNewOnly, InitialSyncAll) {
		errs = append(errs, fmt.Errorf("defaults.initial_sync: invalid value %q (want new_only|all)", c.Defaults.InitialSync))
	}
	if c.Defaults.Timeout <= 0 {
		errs = append(errs, errors.New("defaults.timeout must be positive"))
	}

	for email, acc := range c.Accounts.Targets {
		p := "accounts.targets." + email
		if acc == nil {
			errs = append(errs, fmt.Errorf("%s: empty account", p))
			continue
		}
		acc.Email = email
		if !ValidEmail(email) {
			errs = append(errs, fmt.Errorf("%s: key must be a Gmail address", p))
		}
		if acc.Method == "" {
			acc.Method = MethodAPI
		}
		switch acc.Method {
		case MethodAPI:
		case MethodIMAP:
			if acc.AppPassword == "" {
				errs = append(errs, fmt.Errorf("%s: app_password is required for method imap", p))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.method: invalid value %q (want api|imap)", p, acc.Method))
		}
	}
	for name, src := range c.Accounts.Sources {
		sp := "accounts.sources." + name
		if src == nil {
			errs = append(errs, fmt.Errorf("%s: empty source", sp))
			continue
		}
		if !ValidEmail(name) {
			errs = append(errs, fmt.Errorf("%s: key must be an email address", sp))
		}
		src.Name = name
		errs = append(errs, c.normalizeSource(sp, src)...)
	}
	for name, route := range c.Routes {
		if route == nil {
			errs = append(errs, fmt.Errorf("routes.%s: empty route", name))
			continue
		}
		if c.Accounts.Sources[name] == nil || c.Accounts.Targets[route.Target] == nil {
			errs = append(errs, fmt.Errorf("routes.%s: source and target must reference existing accounts", name))
		}
		if route.InitialSync == "" {
			route.InitialSync = c.Defaults.InitialSync
		}
		if !oneOf(route.InitialSync, InitialSyncNewOnly, InitialSyncAll) {
			errs = append(errs, fmt.Errorf("routes.%s.initial_sync: invalid value", name))
		}
	}
	return errors.Join(errs...)
}

func ValidEmail(email string) bool {
	return strings.Contains(email, "@") && !strings.ContainsAny(email, "/\\\r\n\x00 ")
}

func (c *Config) ValidateService() error {
	if len(c.Routes) == 0 {
		return errors.New("config: at least one route is required to run the service")
	}
	return nil
}

func (c *Config) normalizeSource(p string, s *Source) []error {
	var errs []error
	if strings.TrimSpace(s.Name) == "" {
		errs = append(errs, fmt.Errorf("%s: source name must not be empty", p))
	}
	switch s.Type {
	case TypePOP3:
		if len(s.Folders) > 0 {
			errs = append(errs, fmt.Errorf("%s.folders: only supported for type imap", p))
		}
	case TypeIMAP:
		if len(s.Folders) == 0 {
			s.Folders = []string{"INBOX"}
		}
	case "":
		errs = append(errs, fmt.Errorf("%s.type: required (pop3|imap)", p))
	default:
		errs = append(errs, fmt.Errorf("%s.type: invalid value %q (want pop3|imap)", p, s.Type))
	}
	if s.Server == "" {
		errs = append(errs, fmt.Errorf("%s.server: required", p))
	}
	if s.User == "" {
		errs = append(errs, fmt.Errorf("%s.user: required", p))
	}
	if s.Password == "" {
		errs = append(errs, fmt.Errorf("%s.password: required", p))
	}
	if s.Port < 0 || s.Port > 65535 {
		errs = append(errs, fmt.Errorf("%s.port: out of range", p))
	}
	if s.TLS == "" {
		s.TLS = TLSAuto
	}
	if !oneOf(s.TLS, TLSAuto, TLSImplicit, TLSStartTLS, TLSNone) {
		errs = append(errs, fmt.Errorf("%s.tls: invalid value %q (want auto|implicit|starttls|none)", p, s.TLS))
	}
	if s.Interval == 0 {
		s.Interval = c.Defaults.Interval
	}
	if s.Interval < 10*time.Second {
		errs = append(errs, fmt.Errorf("%s.interval: must be at least 10s", p))
	}
	if s.Timeout == 0 {
		s.Timeout = c.Defaults.Timeout
	}
	if s.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("%s.timeout: must be positive", p))
	}
	return errs
}

// Sources returns all sources sorted by account and name.
func (c *Config) Sources() []*Source {
	var out []*Source
	for name, route := range c.Routes {
		src := *c.Accounts.Sources[name]
		src.Account = route.Target
		src.Label = route.Label
		src.InitialSync = route.InitialSync
		src.DeleteAfterImport = route.DeleteAfterImport
		out = append(out, &src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// AccountList returns all accounts sorted by email.
func (c *Config) AccountList() []*Account {
	out := make([]*Account, 0, len(c.Accounts.Targets))
	for _, a := range c.Accounts.Targets {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// checkKeys rejects unknown mapping keys (typos) based on the yaml struct tags.
func checkKeys(n *yaml.Node, t reflect.Type, path string) error {
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			if err := checkKeys(c, t, path); err != nil {
				return err
			}
		}
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	switch t.Kind() {
	case reflect.Map:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := checkKeys(n.Content[i+1], t.Elem(), path+n.Content[i].Value+"."); err != nil {
				return err
			}
		}
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			if name != "" && name != "-" {
				fields[name] = t.Field(i).Type
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			ft, ok := fields[key.Value]
			if !ok {
				return fmt.Errorf("line %d: unknown field %q", key.Line, path+key.Value)
			}
			if err := checkKeys(n.Content[i+1], ft, path+key.Value+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
