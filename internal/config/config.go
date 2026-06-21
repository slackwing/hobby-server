// Package config loads ~/.config/rv-server/config.yaml.
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

type Server struct {
	Port int    `yaml:"port"`
	Env  string `yaml:"env"` // "development" or "production"
}

type Config struct {
	Database Database `yaml:"database"`
	Server   Server   `yaml:"server"`
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
	if c.Database.Host == "" {
		return nil, fmt.Errorf("config.database.host is required")
	}
	if c.Database.Name == "" {
		return nil, fmt.Errorf("config.database.name is required")
	}
	if c.Database.User == "" {
		return nil, fmt.Errorf("config.database.user is required")
	}
	if c.Database.Password == "" {
		return nil, fmt.Errorf("config.database.password is required")
	}
	if c.Server.Port == 0 {
		c.Server.Port = 5002
	}
	return &c, nil
}

func (c *Config) PostgresDSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		c.Database.User, c.Database.Password, c.Database.Host, c.Database.Port, c.Database.Name)
}
