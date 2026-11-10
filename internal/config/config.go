// Package config validates YAML before typed decoding. Missing files and invalid files are distinct.
package config

import (
	"bytes"
	"fmt"
	"go.yaml.in/yaml/v3"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/onebot"
	"github.com/ZhenForest/xsbot-core/internal/plugins"
)

type Connection struct {
	WS             string `yaml:"ws"`
	Token          string `yaml:"token"`
	APISeconds     int    `yaml:"api_seconds"`
	ConnectSeconds int    `yaml:"connect_seconds"`
	EventBytes     int64  `yaml:"event_bytes"`
	ResponseBytes  int64  `yaml:"response_bytes"`
	Buffer         int    `yaml:"buffer"`
	SelfMessages   bool   `yaml:"self_messages"`
}
type Media struct {
	Enabled         bool  `yaml:"enabled"`
	ResourceBytes   int64 `yaml:"resource_bytes"`
	CapacityBytes   int64 `yaml:"capacity_bytes"`
	Downloads       int   `yaml:"downloads"`
	DownloadSeconds int   `yaml:"download_seconds"`
	TTLSeconds      int   `yaml:"ttl_seconds"`
	ScanSeconds     int   `yaml:"scan_seconds"`
}
type Plugin struct {
	ID       string `yaml:"id"`
	Priority int64  `yaml:"priority"`
	Enabled  bool   `yaml:"enabled"`
}
type Web struct {
	Port            int    `yaml:"port"`
	PublicIP        string `yaml:"public_ip"`
	InitialUsername string `yaml:"initial_username"`
}
type Config struct {
	Schema       int        `yaml:"schema_version"`
	Connection   Connection `yaml:"connection"`
	Media        Media      `yaml:"media"`
	Web          Web        `yaml:"web"`
	CloseSeconds int        `yaml:"close_seconds"`
	LogLevel     string     `yaml:"log_level"`
	Plugins      []Plugin   `yaml:"plugins"`
}

func Default() Config {
	return Config{Schema: 2, Connection: Connection{WS: "ws://127.0.0.1:3001/", APISeconds: 5, ConnectSeconds: 10, EventBytes: 1 << 20, ResponseBytes: 20 << 20, Buffer: 64}, Media: Media{Enabled: true, ResourceBytes: 20 << 20, CapacityBytes: 200 << 20, Downloads: 3, DownloadSeconds: 180, TTLSeconds: 1800, ScanSeconds: 60}, Web: Web{Port: 20107, InitialUsername: "admin"}, CloseSeconds: 30, LogLevel: "INFO", Plugins: []Plugin{}}
}
func (c Config) Transport() onebot.Config {
	return onebot.Config{WS: c.Connection.WS, Token: c.Connection.Token, APITimeout: time.Duration(c.Connection.APISeconds) * time.Second, ConnectTimeout: time.Duration(c.Connection.ConnectSeconds) * time.Second, EventLimit: c.Connection.EventBytes, ResponseLimit: c.Connection.ResponseBytes, Buffer: c.Connection.Buffer}
}
func (c Config) Validate() error {
	if c.Schema != 2 {
		return fmt.Errorf("config schema 2 required: remove connection.http and configure one universal WS plus response_bytes")
	}
	if net.ParseIP(c.Web.PublicIP) == nil || strings.TrimSpace(c.Web.InitialUsername) == "" || strings.HasPrefix(c.Connection.Token, "REPLACE_") {
		return fmt.Errorf("fill required IP, username and actual OneBot token")
	}
	if err := c.Transport().Validate(); err != nil {
		return err
	}
	m := c.Media
	if c.CloseSeconds <= 0 || c.Web.Port < 1 || c.Web.Port > 65535 || m.ResourceBytes <= 0 || m.CapacityBytes < m.ResourceBytes || m.Downloads < 1 || m.DownloadSeconds < 1 || m.TTLSeconds < 1 || m.ScanSeconds < 1 {
		return fmt.Errorf("invalid explicit limits")
	}
	if c.LogLevel != "INFO" && c.LogLevel != "DEBUG" {
		return fmt.Errorf("invalid log level")
	}
	ids := map[string]bool{}
	for _, p := range c.Plugins {
		if !plugins.ValidID(p.ID) || ids[p.ID] || p.Priority < 1 {
			return fmt.Errorf("invalid/duplicate plugin entry")
		}
		ids[p.ID] = true
	}
	return nil
}
func strictYAML(data []byte, out any) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	var tree yaml.Node
	if err := d.Decode(&tree); err != nil {
		return err
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple YAML documents")
		}
		return err
	}
	if err := validateNode(&tree); err != nil {
		return err
	}
	if err := validateTypes(&tree, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}
func validateTypes(n *yaml.Node, t reflect.Type) error {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) != 1 {
			return fmt.Errorf("empty YAML document")
		}
		return validateTypes(n.Content[0], t)
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("expected mapping at line %d", n.Line)
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if tag == "" {
				tag = strings.ToLower(f.Name)
			}
			if tag != "-" {
				fields[tag] = f.Type
			}
		}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			typ, ok := fields[key.Value]
			if !ok {
				return fmt.Errorf("unknown field %q at line %d", key.Value, key.Line)
			}
			if key.Tag != "!!str" {
				return fmt.Errorf("non-string field name")
			}
			if err := validateTypes(n.Content[i+1], typ); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return fmt.Errorf("expected sequence at line %d", n.Line)
		}
		for _, child := range n.Content {
			if err := validateTypes(child, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return fmt.Errorf("expected mapping")
		}
		for i := 0; i < len(n.Content); i += 2 {
			if err := validateTypes(n.Content[i], t.Key()); err != nil {
				return err
			}
			if err := validateTypes(n.Content[i+1], t.Elem()); err != nil {
				return err
			}
		}
	default:
		tag := ""
		switch t.Kind() {
		case reflect.String:
			tag = "!!str"
		case reflect.Bool:
			tag = "!!bool"
		case reflect.Int, reflect.Int64:
			tag = "!!int"
		default:
			return fmt.Errorf("unsupported schema type %v", t)
		}
		if n.Kind != yaml.ScalarNode || n.Tag != tag {
			return fmt.Errorf("wrong scalar type at line %d: expected %s", n.Line, tag)
		}
	}
	return nil
}
func validateNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("YAML aliases/anchors are not supported")
	}
	if n.Tag != "" && !strings.HasPrefix(n.Tag, "!!") {
		return fmt.Errorf("custom YAML tags are not supported")
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return fmt.Errorf("null is not allowed in this config")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || seen[k.Value] {
				return fmt.Errorf("duplicate or non-scalar key at line %d", k.Line)
			}
			seen[k.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := validateNode(child); err != nil {
			return err
		}
	}
	return nil
}
func Read(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c := Default()
	if err = strictYAML(data, &c); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".pending-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("committed; durability uncertain: %w", err)
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return fmt.Errorf("committed; durability uncertain: %w", err)
	}
	return nil
}
func Template(path string) error {
	c := Default()
	c.Connection.Token = "REPLACE_WITH_LOCAL_ONEBOT_TOKEN"
	c.Web.PublicIP = "REPLACE_WITH_SERVER_IP"
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append([]byte("# Fill required values, then restart Core. Never put a token in a URL.\n"), data...)); err != nil {
		return err
	}
	return f.Sync()
}
