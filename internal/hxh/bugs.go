// Bug reports (Andrew, 2026-09-27): any member reports a bug — text and at
// most one picture, the same compose box as BeetleChat — and the page adds
// a snapshot of the reporter's screen (context) to help reproduce it.
// Admins list them, see a pending count (the tray alert), and mark them
// resolved with a note; Claude reads and resolves them through roster.py.
//
// Public URLs (Apache maps /hxh/api/* → /api/hxh/*):
//
//	POST /hxh/api/bugs                 {body, image_id?, context?} → the report (any hxh role)
//	GET  /hxh/api/bugs?status=         pending (default) | resolved | all, newest first (admin)
//	GET  /hxh/api/bugs/count           {pending} (admin)
//	POST /hxh/api/bugs/{id}/status     {status, note?} (admin)
//
// A report's picture is a chat picture (POST /hxh/api/chat/image) and is
// served by GET /hxh/api/chat/image/{id} — to its uploader, and to admins
// once a report carries it (chat.go handleImage).
package hxh

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/slackwing/hobby-server/internal/shared"
)

const (
	BugPending  = "pending"
	BugResolved = "resolved"
	MaxBugBody  = 4000  // characters
	MaxBugCtx   = 16384 // bytes of context JSON
	MaxBugNote  = 2000
)

type BugReport struct {
	ID         int64           `json:"id"`
	Reporter   string          `json:"reporter"`
	Body       string          `json:"body"`
	ImageID    *int64          `json:"image_id"`
	Context    json.RawMessage `json:"context"`
	Status     string          `json:"status"`
	Note       string          `json:"note"`
	CreatedAt  time.Time       `json:"created_at"`
	ResolvedAt *time.Time      `json:"resolved_at"`
	ResolvedBy *string         `json:"resolved_by"`
}

// NewBug is what a member sends.
type NewBug struct {
	Body    string          `json:"body"`
	ImageID int64           `json:"image_id"`
	Context json.RawMessage `json:"context"`
}

// clean checks and trims a report: text or a picture, text within
// MaxBugBody, context a JSON object within MaxBugCtx (else dropped to {}).
func (b NewBug) clean() (NewBug, error) {
	b.Body = strings.TrimSpace(b.Body)
	if b.Body == "" && b.ImageID <= 0 {
		return b, errors.Join(ErrBadInput, errors.New("say what went wrong, or attach a picture"))
	}
	if utf8.RuneCountInString(b.Body) > MaxBugBody {
		return b, errors.Join(ErrBadInput, errors.New("too long"))
	}
	if len(b.Context) == 0 || len(b.Context) > MaxBugCtx || !json.Valid(b.Context) || strings.TrimSpace(string(b.Context))[0] != '{' {
		b.Context = json.RawMessage(`{}`)
	}
	return b, nil
}

// validStatus: pending or resolved.
func validStatus(s string) bool { return s == BugPending || s == BugResolved }

const bugCols = `id, reporter, body, image_id, context, status, note, created_at, resolved_at, resolved_by`

func scanBug(row pgx.Row) (BugReport, error) {
	var b BugReport
	var ctx []byte
	err := row.Scan(&b.ID, &b.Reporter, &b.Body, &b.ImageID, &ctx, &b.Status, &b.Note, &b.CreatedAt, &b.ResolvedAt, &b.ResolvedBy)
	b.Context = json.RawMessage(ctx)
	return b, err
}

// ImageFreeForBug: is picture `id` the reporter's own, on no message and no other report?
func (s *Store) ImageFreeForBug(id int64, user string) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM hxh_chat_image i
		WHERE i.id = $1 AND i.sender = $2
		  AND NOT EXISTS (SELECT 1 FROM hxh_chat_message m WHERE m.image_id = i.id)
		  AND NOT EXISTS (SELECT 1 FROM hxh_bug_report b WHERE b.image_id = i.id)
	`, id, user).Scan(&n)
	return n > 0, err
}

// BugHasImage: does some report carry picture `id`? (Admins may then see it.)
func (s *Store) BugHasImage(id int64) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM hxh_bug_report WHERE image_id = $1`, id).Scan(&n)
	return n > 0, err
}

func (s *Store) InsertBug(reporter string, b NewBug) (BugReport, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var img *int64
	if b.ImageID > 0 {
		img = &b.ImageID
	}
	return scanBug(s.pool.QueryRow(ctx, `INSERT INTO hxh_bug_report (reporter, body, image_id, context) VALUES ($1, $2, $3, $4) RETURNING `+bugCols,
		reporter, b.Body, img, []byte(b.Context)))
}

// ListBugs: status pending, resolved or "" (all); newest first.
func (s *Store) ListBugs(status string) ([]BugReport, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT `+bugCols+` FROM hxh_bug_report WHERE ($1::text = '' OR status = $1::text) ORDER BY created_at DESC, id DESC LIMIT 500`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BugReport{}
	for rows.Next() {
		b, err := scanBug(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) PendingBugs() (int, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM hxh_bug_report WHERE status = 'pending'`).Scan(&n)
	return n, err
}

// SetBugStatus: resolved stamps who and when; back to pending clears them. The note is kept either way.
func (s *Store) SetBugStatus(id int64, status, by, note string) (BugReport, error) {
	ctx, cancel := withCtx()
	defer cancel()
	b, err := scanBug(s.pool.QueryRow(ctx, `
		UPDATE hxh_bug_report SET status = $2::text, note = $4::text,
		  resolved_at = CASE WHEN $2::text = 'resolved' THEN NOW() ELSE NULL END,
		  resolved_by = CASE WHEN $2::text = 'resolved' THEN $3::text ELSE NULL END
		WHERE id = $1 RETURNING `+bugCols, id, status, by, note))
	if errors.Is(err, pgx.ErrNoRows) {
		return b, ErrNotFound
	}
	return b, err
}

// MountBugs wires /bugs: reporting for any member, the rest for admins.
func MountBugs(r chi.Router, store *Store, auth *shared.Store) {
	r.Route("/bugs", func(g chi.Router) {
		g.With(requireHxh(auth, false), refuseAnonymous(auth)).Post("/", handleNewBug(store))
		g.Group(func(a chi.Router) {
			a.Use(requireHxh(auth, true))
			a.Get("/", handleListBugs(store))
			a.Get("/count", handleBugCount(store))
			a.Post("/{id}/status", handleBugStatus(store))
		})
	})
}

func handleNewBug(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in NewBug
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBugCtx+MaxBugBody*4+1024)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
			return
		}
		b, err := in.clean()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), ErrBadInput.Error()+"\n")})
			return
		}
		user := userOf(r)
		if b.ImageID > 0 {
			ok, err := store.ImageFreeForBug(b.ImageID, user)
			if err != nil {
				fail(w, err, "bug image")
				return
			}
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that picture cannot be attached"})
				return
			}
		}
		out, err := store.InsertBug(user, b)
		if err != nil {
			fail(w, err, "new bug")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleListBugs(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := r.URL.Query().Get("status")
		switch status {
		case "":
			status = BugPending
		case "all":
			status = ""
		default:
			if !validStatus(status) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status is pending, resolved or all"})
				return
			}
		}
		out, err := store.ListBugs(status)
		if err != nil {
			fail(w, err, "bugs")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleBugCount(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := store.PendingBugs()
		if err != nil {
			fail(w, err, "bug count")
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"pending": n})
	}
}

func handleBugStatus(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Status string  `json:"status"`
			Note   *string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBugNote*4+256)).Decode(&in); err != nil || !validStatus(in.Status) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status is pending or resolved"})
			return
		}
		note := ""
		if in.Note != nil {
			note = strings.TrimSpace(*in.Note)
			if utf8.RuneCountInString(note) > MaxBugNote {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "note too long"})
				return
			}
		}
		out, err := store.SetBugStatus(id, in.Status, userOf(r), note)
		if err != nil {
			fail(w, err, "bug status")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}
