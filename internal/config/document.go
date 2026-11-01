package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/go-envparse"
	"go.yaml.in/yaml/v3"
)

type Document struct {
	path, envPath         string
	original, originalEnv []byte
	root                  yaml.Node
	values                map[string]string
	secretsChanged        bool
}

func LoadEnvironment(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	values, err := envparse.Parse(file)
	if err != nil {
		return errors.New("env-file: invalid dotenv syntax")
	}
	for name, value := range values {
		if _, exists := os.LookupEnv(name); !exists {
			if err := os.Setenv(name, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func OpenDocument(path, envPath string, explicitEnv bool) (*Document, error) {
	if envPath == "" {
		envPath = filepath.Join(filepath.Dir(path), ".env")
	}
	if filepath.Clean(path) == filepath.Clean(envPath) {
		return nil, errors.New("config and env-file paths must differ")
	}
	doc := &Document{path: path, envPath: envPath, values: map[string]string{}}
	data, err := optionalRead(path)
	if err != nil {
		return nil, err
	}
	doc.original = data
	if len(data) == 0 {
		data = []byte("{}\n")
	}
	if err := yaml.Unmarshal(data, &doc.root); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	doc.originalEnv, err = optionalRead(envPath)
	if err != nil {
		return nil, err
	}
	if explicitEnv && doc.originalEnv == nil {
		return nil, errors.New("env-file does not exist")
	}
	if doc.originalEnv != nil {
		doc.values, err = envparse.Parse(bytes.NewReader(doc.originalEnv))
		if err != nil {
			return nil, errors.New("env-file: invalid dotenv syntax")
		}
	}
	if _, err := doc.Config(); err != nil {
		return nil, err
	}
	return doc, nil
}

func (d *Document) Lookup(name string) (string, bool) {
	if value, ok := os.LookupEnv(name); ok {
		return value, true
	}
	value, ok := d.values[name]
	return value, ok
}

func (d *Document) Config() (*Config, error) {
	data, err := yaml.Marshal(&d.root)
	if err != nil {
		return nil, err
	}
	return Parse(data, d.Lookup)
}

func (d *Document) AddAccount(kind, email string, value any) error {
	if !ValidEmail(email) || (kind != "sources" && kind != "targets") {
		return errors.New("config: invalid account identity")
	}
	parent, err := d.mapping("accounts", kind)
	if err != nil {
		return err
	}
	if nodeValue(parent, email) != nil {
		return fmt.Errorf("config: %s account already exists", kind)
	}
	return setNode(parent, email, value)
}

func (d *Document) Link(source string, route *Route) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	if cfg.Accounts.Sources[source] == nil || cfg.Accounts.Targets[route.Target] == nil {
		return errors.New("link: source and target must exist")
	}
	if existing := cfg.Routes[source]; existing != nil && existing.Target != route.Target {
		return errors.New("link: source is already linked to another target")
	}
	parent, err := d.mapping("routes")
	if err != nil {
		return err
	}
	return setNode(parent, source, route)
}

func (d *Document) Password(kind, email, value, storage string) (string, error) {
	if storage == "yaml" {
		return strings.ReplaceAll(value, "$", "$$"), nil
	}
	if storage != "env" {
		return "", errors.New("secret storage must be env or yaml")
	}
	sum := sha256.Sum256([]byte(kind + "/" + email))
	name := fmt.Sprintf("GMAIL_POPPERS_%s_%X_PASSWORD", strings.ToUpper(kind), sum[:8])
	if existing, ok := d.Lookup(name); ok && existing != value {
		return "", errors.New("password environment variable already has a different value")
	}
	d.values[name] = value
	d.secretsChanged = true
	return "${" + name + "}", nil
}

func (d *Document) Save() error {
	if _, err := d.Config(); err != nil {
		return err
	}
	data, err := yaml.Marshal(&d.root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(d.path+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("config: another writer holds the lock")
	}
	lock.Close()
	defer os.Remove(d.path + ".lock")
	if err := unchanged(d.path, d.original); err != nil {
		return err
	}
	if d.secretsChanged {
		if err := unchanged(d.envPath, d.originalEnv); err != nil {
			return err
		}
		envData, err := marshalEnv(d.values)
		if err != nil {
			return err
		}
		if err := WritePrivate(d.envPath, envData); err != nil {
			return err
		}
	}
	return WritePrivate(d.path, data)
}

func WritePrivate(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("refusing to replace a non-regular file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".gmail-poppers-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func optionalRead(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func unchanged(path string, original []byte) error {
	current, err := optionalRead(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) || (current == nil) != (original == nil) {
		return errors.New("config: files changed during setup; try again")
	}
	return nil
}

func marshalEnv(values map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var text bytes.Buffer
	for _, name := range keys {
		value, err := json.Marshal(values[name])
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&text, "%s=%s\n", name, value)
	}
	decoded, err := envparse.Parse(bytes.NewReader(text.Bytes()))
	if err != nil {
		return nil, errors.New("env-file: values could not be serialized")
	}
	for name, value := range values {
		if decoded[name] != value {
			return nil, errors.New("env-file: values could not be preserved")
		}
	}
	return text.Bytes(), nil
}

func (d *Document) mapping(keys ...string) (*yaml.Node, error) {
	if len(d.root.Content) != 1 {
		return nil, errors.New("config: expected a YAML document")
	}
	parent := d.root.Content[0]
	for _, key := range keys {
		if parent.Kind != yaml.MappingNode {
			return nil, errors.New("config: expected a mapping")
		}
		child := nodeValue(parent, key)
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
		}
		parent = child
	}
	if parent.Kind != yaml.MappingNode {
		return nil, errors.New("config: expected a mapping")
	}
	return parent, nil
}

func nodeValue(parent *yaml.Node, name string) *yaml.Node {
	for index := 0; index+1 < len(parent.Content); index += 2 {
		if parent.Content[index].Value == name {
			return parent.Content[index+1]
		}
	}
	return nil
}

func setNode(parent *yaml.Node, name string, value any) error {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		return err
	}
	if old := nodeValue(parent, name); old != nil {
		node.HeadComment = old.HeadComment
		node.LineComment = old.LineComment
		*old = node
		return nil
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, &node)
	return nil
}
