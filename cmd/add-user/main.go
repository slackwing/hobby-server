// add-user: insert (or overwrite) a user in a project's `user` table.
//
// Usage:
//   add-user --project <name> [--config <path>] <username> <password>
//
// Reads the same ~/.config/hobby-server/config.yaml as the server.
// The --project flag selects which configured project's database to
// write to. Bcrypt-hashes the password before storing.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/slackwing/hobby-server/internal/auth"
	"github.com/slackwing/hobby-server/internal/config"
	"github.com/slackwing/hobby-server/internal/database"
)

func main() {
	var configPath, projectName string
	flag.StringVar(&configPath, "config", defaultConfigPath(), "path to config.yaml")
	flag.StringVar(&projectName, "project", "", "project name (must match a configured project)")
	flag.Parse()

	args := flag.Args()
	if projectName == "" || len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: add-user --project <name> [--config <path>] <username> <password>")
		os.Exit(2)
	}
	username, password := args[0], args[1]
	if err := auth.ValidatePassword(password); err != nil {
		log.Fatalf("invalid password: %v", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	project := cfg.FindProject(projectName)
	if project == nil {
		log.Fatalf("project %q not found in config (configured: %v)",
			projectName, projectNames(cfg))
	}

	ctx := context.Background()
	pool, err := database.NewPool(ctx, project.PostgresDSN())
	if err != nil {
		log.Fatalf("connect db (project %s): %v", projectName, err)
	}
	defer pool.Close()

	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO "user" (username, password_hash, created_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash
	`, username, hash)
	if err != nil {
		log.Fatalf("upsert user: %v", err)
	}
	fmt.Printf("user %q upserted in project %q (db=%s)\n", username, projectName, project.Database.Name)
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hobby-server", "config.yaml")
}

func projectNames(cfg *config.Config) []string {
	out := make([]string, len(cfg.Projects))
	for i, p := range cfg.Projects {
		out[i] = p.Name
	}
	return out
}
