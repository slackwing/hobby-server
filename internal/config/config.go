// Package config loads ~/.config/hobby-server/config.yaml.
//
// hobby-server is multi-project: one binary, one process, but each
// configured project has its own database, its own URL prefix, and its
// own cookie scope. Auth tables (`user`, `session`) live INSIDE each
// project's database (per the design — no shared auth surface).
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Database struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

type Project struct {
	// Name is the short id used everywhere (URL fragment, cookie name
	// prefix, liquibase subdir name, log lines). Must match
	// [a-z0-9_]+ and match the directory name under liquibase/.
	Name string `yaml:"name"`

	Database Database `yaml:"database"`

	// URLPrefix is where this project's API is mounted on the server.
	// Combined with the chi router this is the path prefix the server
	// listens on (e.g. "/api/rv"; routes get suffixes like
	// "/api/rv/login"). Apache should reverse-proxy from the public URL
	// to this internal path.
	URLPrefix string `yaml:"url_prefix"`

	// CookiePath is the Path attribute set on the session cookie.
	// Example: "/rv/" — scopes the cookie so /rv/ logins don't bleed
	// into /next/.
	CookiePath string `yaml:"cookie_path"`
}

type Server struct {
	Port int    `yaml:"port"`
	Env  string `yaml:"env"` // "development" or "production"
}

type Config struct {
	Server   Server    `yaml:"server"`
	Projects []Project `yaml:"projects"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Server.Port == 0 {
		c.Server.Port = 5002
	}
	if len(c.Projects) == 0 {
		return nil, fmt.Errorf("config.projects: at least one project required")
	}
	seen := map[string]bool{}
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Name == "" {
			return nil, fmt.Errorf("config.projects[%d].name is required", i)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("config.projects: duplicate name %q", p.Name)
		}
		seen[p.Name] = true
		if p.Database.Host == "" || p.Database.Name == "" || p.Database.User == "" || p.Database.Password == "" {
			return nil, fmt.Errorf("config.projects[%s].database: host/name/user/password all required", p.Name)
		}
		if p.URLPrefix == "" {
			return nil, fmt.Errorf("config.projects[%s].url_prefix is required", p.Name)
		}
		if p.CookiePath == "" {
			return nil, fmt.Errorf("config.projects[%s].cookie_path is required", p.Name)
		}
	}
	return &c, nil
}

// FindProject returns the project with the given name, or nil if absent.
func (c *Config) FindProject(name string) *Project {
	for i := range c.Projects {
		if c.Projects[i].Name == name {
			return &c.Projects[i]
		}
	}
	return nil
}

func (p *Project) PostgresDSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		p.Database.User, p.Database.Password, p.Database.Host, p.Database.Port, p.Database.Name)
}
