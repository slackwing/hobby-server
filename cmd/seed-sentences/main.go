// seed-sentences fills shared_random_sentences with lines from
// PUBLIC-DOMAIN children's books on Project Gutenberg: each book is
// downloaded as plain text, split into sentences, filtered to chat-sized
// lines, and inserted (duplicates skipped) until the target is reached.
//
//	seed-sentences --config ~/.config/hobby-server/config.yaml [--target 10000]
//
// Run it once per database (install.sh's docker image carries it):
//
//	docker run --rm --network host -v "$CONFIG:/config/config.yaml:ro" \
//	  hobby-server:latest seed-sentences --config /config/config.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/slackwing/hobby-server/internal/config"
	"github.com/slackwing/hobby-server/internal/database"
	"github.com/slackwing/hobby-server/internal/shared"
)

// Gutenberg ebook numbers. All public domain in the US; the title line
// of each download is printed so a wrong number is obvious.
var books = []int{
	11,    // Alice's Adventures in Wonderland
	12,    // Through the Looking-Glass
	16,    // Peter Pan
	55,    // The Wonderful Wizard of Oz
	289,   // The Wind in the Willows
	67098, // Winnie-the-Pooh
	2591,  // Grimms' Fairy Tales
	27200, // Andersen's Fairy Tales
	21,    // Aesop's Fables
	14838, // The Tale of Peter Rabbit
	2781,  // Just So Stories
	236,   // The Jungle Book
	500,   // The Adventures of Pinocchio
	501,   // The Story of Doctor Dolittle
	11757, // The Velveteen Rabbit
	18190, // Raggedy Ann Stories
	45,    // Anne of Green Gables
	113,   // The Secret Garden
	1448,  // Heidi
	146,   // A Little Princess
	271,   // Black Beauty
	120,   // Treasure Island
	74,    // The Adventures of Tom Sawyer
	778,   // Five Children and It
	13650, // A Book of Nonsense (Lear)
	1874,  // The Railway Children
	770,   // The Story of the Treasure Seekers
	19033, // Alice (illustrated edition; duplicates are skipped)
	23716, // Mother Goose / nursery rhymes (checked by title at run time)
	17396, // The Reluctant Dragon / Dream Days (Grahame)
}

var (
	titleRe   = regexp.MustCompile(`(?m)^Title:\s*(.+)$`)
	startRe   = regexp.MustCompile(`(?m)^\*\*\* ?START OF (THE|THIS) PROJECT GUTENBERG EBOOK.*$`)
	endRe     = regexp.MustCompile(`(?m)^\*\*\* ?END OF (THE|THIS) PROJECT GUTENBERG EBOOK.*$`)
	spaceRe   = regexp.MustCompile(`\s+`)
	badWordRe = regexp.MustCompile(`(?i)\b(chapter|illustration|gutenberg|copyright|transcriber|contents|preface|footnote|page \d)\b`)
	okCharsRe = regexp.MustCompile(`^[A-Za-z][A-Za-z ,;:'’‘"“”!?.\-—()]*[.!?]["”’']?$`)
)

func main() {
	var configPath string
	var target, minLen, maxLen int
	flag.StringVar(&configPath, "config", "", "path to config.yaml (the admin project's database is used)")
	flag.IntVar(&target, "target", 10000, "how many sentences the table should hold")
	flag.IntVar(&minLen, "min", 24, "shortest sentence, characters")
	flag.IntVar(&maxLen, "max", 120, "longest sentence, characters")
	flag.Parse()
	if configPath == "" {
		log.Fatal("--config required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	var admin *config.Project
	for i := range cfg.Projects {
		if cfg.Projects[i].Name == "admin" {
			admin = &cfg.Projects[i]
		}
	}
	if admin == nil {
		log.Fatal("no admin project in config")
	}
	ctx := context.Background()
	pool, err := database.NewPool(ctx, admin.PostgresDSN())
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	store := shared.NewStore(pool)
	have, _ := store.CountSentences()
	log.Printf("corpus has %d sentences; target %d", have, target)
	if have >= target {
		return
	}

	// gather everything first, then take a fair share of every book
	type src struct {
		title string
		lines []string
	}
	var sources []src
	seen := map[string]bool{}
	client := &http.Client{Timeout: 60 * time.Second}
	for _, id := range books {
		title, text, err := fetch(client, id)
		if err != nil {
			log.Printf("  #%d: %v (skipped)", id, err)
			continue
		}
		lines := sentences(text, minLen, maxLen)
		uniq := lines[:0]
		for _, l := range lines {
			k := strings.ToLower(l)
			if !seen[k] {
				seen[k] = true
				uniq = append(uniq, l)
			}
		}
		log.Printf("  #%d %q: %d usable sentences", id, title, len(uniq))
		sources = append(sources, src{title: title, lines: uniq})
	}
	rnd := rand.New(rand.NewSource(20261031))
	for i := range sources {
		rnd.Shuffle(len(sources[i].lines), func(a, b int) { sources[i].lines[a], sources[i].lines[b] = sources[i].lines[b], sources[i].lines[a] })
	}
	// round-robin across books so no single book dominates
	need := target - have
	added := 0
	for round := 0; added < need; round++ {
		progress := false
		for i := range sources {
			if round < len(sources[i].lines) && added < need {
				n, err := store.InsertSentences(sources[i].title, []string{sources[i].lines[round]})
				if err != nil {
					log.Fatalf("insert: %v", err)
				}
				added += n
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	total, _ := store.CountSentences()
	log.Printf("added %d; corpus now %d", added, total)
}

func fetch(client *http.Client, id int) (title, text string, err error) {
	for _, u := range []string{
		fmt.Sprintf("https://www.gutenberg.org/cache/epub/%d/pg%d.txt", id, id),
		fmt.Sprintf("https://www.gutenberg.org/files/%d/%d-0.txt", id, id),
	} {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("User-Agent", "hobby-server seed-sentences (public-domain corpus; contact acheong87@gmail.com)")
		resp, e := client.Do(req)
		if e != nil {
			err = e
			continue
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		s := string(b)
		if m := titleRe.FindStringSubmatch(s); m != nil {
			title = strings.TrimSpace(m[1])
		} else {
			title = fmt.Sprintf("Gutenberg #%d", id)
		}
		if m := startRe.FindStringIndex(s); m != nil {
			s = s[m[1]:]
		}
		if m := endRe.FindStringIndex(s); m != nil {
			s = s[:m[0]]
		}
		return title, s, nil
	}
	return "", "", err
}

// sentences splits prose into chat-sized lines and keeps the clean ones.
func sentences(text string, minLen, maxLen int) []string {
	text = strings.ReplaceAll(text, "\r", "")
	// paragraph breaks end sentences too; then collapse whitespace
	text = strings.ReplaceAll(text, "\n\n", "   ")
	text = spaceRe.ReplaceAllString(text, " ")
	var out []string
	var cur strings.Builder
	runes := []rune(text)
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s != "" {
			out = append(out, s)
		}
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == ' ' {
			flush()
			continue
		}
		cur.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			// swallow closing quotes, then cut if the next non-space is uppercase or a quote
			j := i + 1
			for j < len(runes) && (runes[j] == '"' || runes[j] == '”' || runes[j] == '\'' || runes[j] == '’') {
				cur.WriteRune(runes[j])
				j++
			}
			k := j
			for k < len(runes) && runes[k] == ' ' {
				k++
			}
			if k >= len(runes) || unicode.IsUpper(runes[k]) || runes[k] == '"' || runes[k] == '“' {
				flush()
			}
			i = j - 1
		}
	}
	flush()
	keep := out[:0]
	for _, s := range out {
		if ok(s, minLen, maxLen) {
			keep = append(keep, s)
		}
	}
	sort.Strings(keep)
	return keep
}

func ok(s string, minLen, maxLen int) bool {
	n := len([]rune(s))
	if n < minLen || n > maxLen {
		return false
	}
	if !okCharsRe.MatchString(s) || badWordRe.MatchString(s) {
		return false
	}
	words := strings.Fields(s)
	if len(words) < 5 {
		return false
	}
	upper, letters := 0, 0
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters == 0 || float64(upper)/float64(letters) > 0.3 {
		return false // headings, shouting
	}
	// unbalanced quotes read as fragments
	if strings.Count(s, "\"")%2 == 1 || strings.Count(s, "“") != strings.Count(s, "”") {
		return false
	}
	return true
}

var _ = os.Exit
