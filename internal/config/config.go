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

// Telegram holds optional bot-notification settings for a project
// (used by bap: a message is sent when a user's cup shatters). Both
// fields empty = notifications disabled.
type Telegram struct {
	BotToken string `yaml:"bot_token"`
	ChatID   string `yaml:"chat_id"`
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

	// Telegram is optional; see the Telegram type.
	Telegram Telegram `yaml:"telegram"`
}

type Server struct {
	Port int    `yaml:"port"`
	Env  string `yaml:"env"` // "development" or "production"
}

// Email is the server-wide SMTP sender used by the shared auth system
// (internal/shared) for templated emails — invites, welcomes. Leave
// the whole block out to disable sending; the admin console then
// reports "email not configured". Any STARTTLS submission endpoint
// works (Gmail app password on smtp.gmail.com:587, etc.).
type Email struct {
	SMTPHost string `yaml:"smtp_host"`
	SMTPPort int    `yaml:"smtp_port"` // default 587
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	From     string `yaml:"from"` // "Name <addr>" or bare address

	// SiteBaseURL overrides where a website's _email/ templates are
	// fetched from and what links in emails point at. Empty (the
	// production setting) derives it from each request's
	// X-Forwarded-Proto + Host, i.e. https://andrewcheong.com.
	// Set it only in dev, to point at a local static server.
	SiteBaseURL string `yaml:"site_base_url"`
}

// Bots configures the bot service (internal/bots): fake users that use
// the PUBLIC site API like real players. Omit the block to run no bots.
type Bots struct {
	Enabled bool `yaml:"enabled"`
	// BaseURL is where the bots reach the site — the public origin in
	// production (through Apache, TLS and all), the Go server itself in
	// development (then set Direct).
	BaseURL string `yaml:"base_url"`
	// Direct: BaseURL is the Go server (paths /api/<site>/…) rather than
	// the public site (paths /<site>/api/…).
	Direct bool `yaml:"direct"`
	// Password is provisioned onto every bot account (never logged).
	Password string `yaml:"password"`
	// Tick is the scheduler interval ("5m" default; "30s" in dev).
	Tick string `yaml:"tick"`
}

type Config struct {
	Server   Server    `yaml:"server"`
	Email    Email     `yaml:"email"`
	Bots     Bots      `yaml:"bots"`
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
	if c.Email.SMTPHost != "" {
		if c.Email.From == "" {
			return nil, fmt.Errorf("config.email.from is required when smtp_host is set")
		}
		if c.Email.SMTPPort == 0 {
			c.Email.SMTPPort = 587
		}
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
