package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultUserAgent is what every recorder request pretends to be. Services do
// branch on it (Nextcloud serves different CSS to browsers and to non-browsers),
// so recording with Go's default UA produced snapshots no browser would receive.
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// PostSpec describes one recorded non-GET request.
type PostSpec struct {
	Path        string `yaml:"path"`
	ContentType string `yaml:"content_type"`
	Body        string `yaml:"body"`
}

// Rewrite is a literal find/replace applied to replayed bodies and Location headers.
type Rewrite struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// Service is one imitated service served on its own port.
type Service struct {
	Name       string `yaml:"name"`
	Port       int    `yaml:"port"`
	FixtureDir string `yaml:"fixture_dir"`

	// Catalog metadata (shown by `srvimit list`, stored in the manifest).
	Title       string `yaml:"title"`
	Category    string `yaml:"category"`
	LoginPath   string `yaml:"login_path"`
	App         string `yaml:"app"`
	DefaultPort int    `yaml:"default_port"`

	// Upstream is only used in record mode: a live instance of the real service.
	Upstream string `yaml:"upstream"`
	// PublicURL is the URL clients will see (https://host). Recorded pages are
	// generated with this Host so absolute URLs already match production.
	PublicURL string `yaml:"public_url"`

	Pages []string `yaml:"pages"`
	API   []string `yaml:"api"`
	// Posts are login attempts recorded with deliberately wrong credentials, so a
	// client that POSTs the login form gets the service's real failure response
	// (status, headers, body) instead of the login page again.
	Posts []PostSpec `yaml:"posts"`
	// ExtraURLsFile lists additional paths to record, one per line. Use it to
	// complete a snapshot with URLs a real browser requested (lazy JS chunks).
	ExtraURLsFile  string    `yaml:"extra_urls_file"`
	RecordNotFound bool      `yaml:"record_not_found"`
	KeepSetCookie  bool      `yaml:"keep_set_cookie"`
	Rewrite        []Rewrite `yaml:"rewrite"`
	RewriteBody    bool      `yaml:"rewrite_body"`
	MaxAssets      int       `yaml:"max_assets"`
	UserAgent      string    `yaml:"user_agent"`
	NotFound       string    `yaml:"not_found"` // "fixture" (default) or "plain"
}

// Config is the whole services.yaml.
type Config struct {
	Listen   string    `yaml:"listen"`
	Services []Service `yaml:"services"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "0.0.0.0"
	}
	for i := range c.Services {
		s := &c.Services[i]
		if s.UserAgent == "" {
			s.UserAgent = DefaultUserAgent
		}
		if s.MaxAssets <= 0 {
			s.MaxAssets = 400
		}
		if len(s.Pages) == 0 {
			s.Pages = []string{"/"}
		}
		if s.NotFound == "" {
			s.NotFound = "fixture"
		}
		if s.FixtureDir == "" {
			s.FixtureDir = "fixtures/" + s.Name
		}
	}
}

func (c *Config) validate() error {
	if len(c.Services) == 0 {
		return fmt.Errorf("config: no services defined")
	}
	seenName := map[string]bool{}
	seenPort := map[int]bool{}
	for i, s := range c.Services {
		if s.Name == "" {
			return fmt.Errorf("config: services[%d]: name is required", i)
		}
		if !validName(s.Name) {
			return fmt.Errorf("config: service %q: name must be [a-z0-9_-]+", s.Name)
		}
		if seenName[s.Name] {
			return fmt.Errorf("config: duplicate service name %q", s.Name)
		}
		seenName[s.Name] = true
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("config: service %q: invalid port %d", s.Name, s.Port)
		}
		if seenPort[s.Port] {
			return fmt.Errorf("config: duplicate port %d", s.Port)
		}
		seenPort[s.Port] = true
		if s.Upstream != "" {
			u, err := url.Parse(s.Upstream)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("config: service %q: upstream must be an absolute http(s) URL", s.Name)
			}
		}
		if s.PublicURL != "" {
			u, err := url.Parse(s.PublicURL)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("config: service %q: public_url must be absolute", s.Name)
			}
		}
		switch s.NotFound {
		case "fixture", "plain":
		default:
			return fmt.Errorf("config: service %q: not_found must be fixture|plain", s.Name)
		}
	}
	return nil
}

func validName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return s != ""
}

// Select filters services by comma separated names ("" = all).
func (c *Config) Select(only string) []Service {
	if strings.TrimSpace(only) == "" {
		return c.Services
	}
	want := map[string]bool{}
	for _, p := range strings.Split(only, ",") {
		want[strings.TrimSpace(p)] = true
	}
	var out []Service
	for _, s := range c.Services {
		if want[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// Defaults fills the fields a hand-built Service may leave empty (the YAML path
// does this in Load, but programmatic callers need the same behaviour).
func (s Service) Defaults() Service {
	if s.UserAgent == "" {
		s.UserAgent = DefaultUserAgent
	}
	if s.MaxAssets <= 0 {
		s.MaxAssets = 400
	}
	if s.NotFound == "" {
		s.NotFound = "fixture"
	}
	if len(s.Pages) == 0 {
		s.Pages = []string{"/"}
	}
	if s.FixtureDir == "" {
		s.FixtureDir = "fixtures/" + s.Name
	}
	return s
}

// PublicHost returns the host[:port] recorded pages should be generated for.
func (s Service) PublicHost() string {
	if s.PublicURL == "" {
		return ""
	}
	u, err := url.Parse(s.PublicURL)
	if err != nil {
		return ""
	}
	return u.Host
}
