// add-user: insert (or overwrite) a user in the `user` table.
//
// Usage:
//   add-user <username> <password>
//
// Reads the same ~/.config/rv-server/config.yaml as the server for DB
// credentials. Bcrypt-hashes the password before storing.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/slackwing/rv-server/internal/auth"
	"github.com/slackwing/rv-server/internal/config"
	"github.com/slackwing/rv-server/internal/database"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", defaultConfigPath(), "path to config.yaml")
	flag.Parse()

	args := flag.Args()
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: add-user [--config <path>] <username> <password>")
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
	ctx := context.Background()
	pool, err := database.NewPool(ctx, cfg.PostgresDSN())
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}

	// Upsert: insert or update password_hash if user already exists.
	_, err = pool.Exec(ctx, `
		INSERT INTO "user" (username, password_hash, created_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (username) DO UPDATE SET password_hash = EXCLUDED.password_hash
	`, username, hash)
	if err != nil {
		log.Fatalf("upsert user: %v", err)
	}
	fmt.Printf("user %q upserted\n", username)
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rv-server", "config.yaml")
}
