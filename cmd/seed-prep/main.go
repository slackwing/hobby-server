// seed-prep: one-shot loader for the rv prep_item table.
//
// Reads a JSON file in the legacy prep.json format and inserts each item
// into prep_item with sort_order = index * 1000. Refuses to run if the
// table is non-empty unless --force is passed.
//
// Usage (typical, inside Docker on the VM):
//
//   docker run --rm --network host \
//     -v "$HOME/.config/hobby-server/config.yaml:/config/config.yaml:ro" \
//     -v "/tmp/prep-seed.json:/tmp/prep-seed.json:ro" \
//     hobby-server:latest \
//     seed-prep --config /config/config.yaml --project rv --file /tmp/prep-seed.json
//
// The JSON file shape:
//
//   {
//     "items": [
//       { "section": "before", "text": "...", "date": "Mon 6/22" },
//       ...
//     ]
//   }
//
// `section` and `text` are required. `date` (e.g. "Mon 6/22") gets
// converted to an inline `@M/D` prefix on `text` — the frontend renders
// `@M/D` as a styled date pill with the auto-computed day-of-week.
// `id` / `depends_on` from the old format are ignored.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slackwing/hobby-server/internal/config"
)

type legacyItem struct {
	Section string `json:"section"`
	Text    string `json:"text"`
	Date    string `json:"date"`
}

type legacyFile struct {
	Items []legacyItem `json:"items"`
}

func main() {
	var (
		configPath  string
		projectName string
		filePath    string
		force       bool
	)
	flag.StringVar(&configPath, "config", defaultConfigPath(), "path to config.yaml")
	flag.StringVar(&projectName, "project", "rv", "project name in config")
	flag.StringVar(&filePath, "file", "", "path to JSON seed file")
	flag.BoolVar(&force, "force", false, "delete existing prep_item rows before seeding")
	flag.Parse()

	if filePath == "" {
		log.Fatalf("--file is required")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	var project *config.Project
	for i := range cfg.Projects {
		if cfg.Projects[i].Name == projectName {
			project = &cfg.Projects[i]
			break
		}
	}
	if project == nil {
		log.Fatalf("project %q not found in config", projectName)
	}

	raw, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("read seed file: %v", err)
	}
	var legacy legacyFile
	if err := json.Unmarshal(raw, &legacy); err != nil {
		log.Fatalf("parse seed file: %v", err)
	}
	if len(legacy.Items) == 0 {
		log.Fatalf("seed file has no items")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, project.PostgresDSN())
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	var existing int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM prep_item`).Scan(&existing); err != nil {
		log.Fatalf("count existing: %v", err)
	}
	if existing > 0 {
		if !force {
			log.Fatalf("prep_item already has %d rows. Re-run with --force to wipe and re-seed.", existing)
		}
		log.Printf("--force: deleting %d existing rows", existing)
		if _, err := pool.Exec(ctx, `DELETE FROM prep_item`); err != nil {
			log.Fatalf("delete existing: %v", err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)

	// "Mon 6/22" → extract "6/22" and prepend "@6/22 " to the item text.
	// The frontend renders @M/D as a styled date pill (day-of-week is
	// auto-computed from the current year, so it stays correct).
	dateExtract := regexp.MustCompile(`(\d{1,2}/\d{1,2})`)

	inserted := 0
	for i, it := range legacy.Items {
		if it.Section == "" || it.Text == "" {
			log.Fatalf("item %d missing section or text", i)
		}
		text := it.Text
		if it.Date != "" {
			if m := dateExtract.FindString(it.Date); m != "" {
				text = "@" + m + " " + text
			}
		}
		sortOrder := float64(i+1) * 1000.0
		_, err := tx.Exec(ctx, `
			INSERT INTO prep_item (section, text, sort_order)
			VALUES ($1, $2, $3)
		`, it.Section, text, sortOrder)
		if err != nil {
			log.Fatalf("insert item %d: %v", i, err)
		}
		inserted++
	}

	if err := tx.Commit(ctx); err != nil {
		log.Fatalf("commit: %v", err)
	}

	fmt.Printf("Inserted %d items into prep_item (project %q)\n", inserted, projectName)
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hobby-server", "config.yaml")
}
