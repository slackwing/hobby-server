// Roster DB — the curated character base (schema: liquibase/hxh/
// changelog/006-roster-db.xml). Characters are inserted as "pending" one
// at a time by the hxh-character skill (feathers .claude/skills/) and
// reviewed by hand in the admin page /hxh/roster/. Every picture is a
// row in hxh_char_image with a GLOBAL id, a type (raw, cropped,
// pixelated, upscaled, transparent) and the image it was made from.
//
// Public URLs (Apache maps /hxh/api/* → /api/hxh/*):
//
//	GET    /hxh/api/db/chars?status=&name=      list, no blobs (name: case-insensitive exact)
//	POST   /hxh/api/db/chars                    create (pending, version 1)
//	GET    /hxh/api/db/chars/{id}               profile + image metadata + review log
//	PATCH  /hxh/api/db/chars/{id}               partial update (bumps version)
//	POST   /hxh/api/db/chars/{id}/review        {status, reason} — the verdict on the current version
//	DELETE /hxh/api/db/chars/{id}
//	POST   /hxh/api/db/chars/{id}/images        upload (multipart "file" or raw body)
//	GET    /hxh/api/db/binder                   accepted characters, card fields only (any hxh role) — the Binder's source
//	GET    /hxh/api/db/images/{id}              the picture (any hxh role)
//	GET    /hxh/api/db/images/{id}/thumb        its preview (any hxh role)
//	GET    /hxh/api/db/images/{id}/meta         its row (what the crop page shows)
//	PATCH  /hxh/api/db/images/{id}              status / caption
//	DELETE /hxh/api/db/images/{id}
//	POST   /hxh/api/db/images/{id}/crop         {x,y,w,h} → a new "cropped" image
//
// Writes need role admin on hxh; reads of pictures need any hxh role
// (the guest-facing Binder will show avatars one day).
//
// Versioning (Andrew, 2026-09-19): every change to a character or its
// pictures bumps hxh_char.version; a review (accept / reject with a
// reason / back to pending) is a verdict on the version it was passed
// on and is logged in hxh_char_review, never bumping the version.
package hxh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/slackwing/hobby-server/internal/shared"
)

// Vocabularies. Lists are stored comma-joined (see splitSlugs/joinSlugs).
var (
	ImageTypes = []string{"raw", "uploaded", "cropped", "pixelated", "upscaled", "transparent"}
	NenTypes   = []string{"enhancement", "transmutation", "conjuration", "emission", "manipulation", "specialization"}
	ArcSlugs   = []string{"hunter-exam", "zoldyck-family", "heavens-arena", "yorknew-city", "greed-island", "chimera-ant", "chairman-election"}
	Ranks      = []string{"S", "A", "B", "C"}
	CharStatus = []string{"pending", "accepted", "rejected", "skipped"}
	Verdicts   = []string{"pending", "accepted", "rejected"} // what a reviewer sets; requests are a count, not a state (Andrew, 2026-09-21)
	// "skipped" (Andrew, 2026-09-21): a stub the bot filed for a character
	// deliberately left out — name, arc and one line why, no pictures. It is
	// frozen: no verdict, no edit, no picture, no request, and never a
	// number, until the bot resurrects it (sets it pending) on Andrew's word.
	ImgStatus = []string{"kept", "rejected"}
)

const (
	thumbMaxSide  = 320
	maxUploadSize = 40 << 20
)

var (
	ErrConflict = errors.New("conflict")
	ErrNotFound = errors.New("not found")
	ErrBadInput = errors.New("bad input")
)

type Char struct {
	ID              int64          `json:"id"`
	CardNumber      int            `json:"card_number"` // the binder position, every card has one; not unique on purpose (Andrew, 2026-09-21/22)
	Name            string         `json:"name"`
	NameJA          string         `json:"name_ja"`
	First           string         `json:"first"`
	Rank            string         `json:"rank"`
	NenTypes        []string       `json:"nen_types"`
	Affiliation     string         `json:"affiliation"`
	Arcs            []string       `json:"arcs"`
	Arms            []string       `json:"arms"`
	Description     string         `json:"description"`
	CardDesc        string         `json:"card_description"`
	Notes           string         `json:"notes"`
	Version         int            `json:"version"`
	AcceptedVersion *int           `json:"accepted_version"` // the version the Binder shows; nil = never accepted
	OpenRequests    int            `json:"open_requests"`
	ReviewStatus    string         `json:"review_status"`
	ReviewReason    string         `json:"review_reason"`
	AvatarImageID   *int64         `json:"avatar_image_id"`
	CardImageID     *int64         `json:"card_image_id"`
	Owner           string         `json:"owner"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	ImageCount      int            `json:"image_count"`
	ImageCounts     map[string]int `json:"image_counts"` // per type, for the list's Pics column
	Images          []Image        `json:"images,omitempty"`
	Reviews         []Review       `json:"reviews,omitempty"`
	Requests        []Request      `json:"requests,omitempty"`
	Baseline        *Baseline      `json:"baseline"` // the last human verdict, or null
	Changes         []Change       `json:"changes"`  // what changed above the baseline (always present)
}

// Baseline is the last human verdict: the version the reviewer judged.
type Baseline struct {
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
}

// Change is one line of the change log: what a version changed — a
// field (old and new value) or a picture added, removed or edited —
// by whom, and whether that was the bot. The Roster DB marks a bot's
// changes above the baseline "New"; a human's are self-approved
// (Andrew, 2026-09-21).
type Change struct {
	ID        int64     `json:"id"`
	CharID    int64     `json:"char_id"`
	Version   int       `json:"version"`
	Kind      string    `json:"kind"` // field | image
	Field     string    `json:"field,omitempty"`
	ImageID   *int64    `json:"image_id,omitempty"`
	Action    string    `json:"action"` // set | added | removed | edited
	Old       string    `json:"old,omitempty"`
	New       string    `json:"new,omitempty"`
	Owner     string    `json:"owner"`
	Bot       bool      `json:"bot"`
	CreatedAt time.Time `json:"created_at"`
}

// Review is one verdict from the log.
type Review struct {
	ID        int64     `json:"id"`
	CharID    int64     `json:"char_id"`
	Version   int       `json:"version"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason"`
	Owner     string    `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
}

type Image struct {
	ID            int64     `json:"id"`
	CharID        int64     `json:"char_id"`
	Type          string    `json:"type"`
	SourceImageID *int64    `json:"source_image_id"`
	Mime          string    `json:"mime"`
	Width         int       `json:"width"`
	Height        int       `json:"height"`
	Bytes         int       `json:"bytes"`
	SHA256        string    `json:"sha256"`
	SourceURL     string    `json:"source_url"`
	Caption       string    `json:"caption"`
	Status        string    `json:"status"`
	Owner         string    `json:"owner"`
	CreatedAt     time.Time `json:"created_at"`
}

func in(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func validSlugList(list []string, vocab []string, max int) error {
	if len(list) > max {
		return fmt.Errorf("%w: at most %d values", ErrBadInput, max)
	}
	seen := map[string]bool{}
	for _, v := range list {
		if !in(vocab, v) {
			return fmt.Errorf("%w: unknown value %q", ErrBadInput, v)
		}
		if seen[v] {
			return fmt.Errorf("%w: duplicate %q", ErrBadInput, v)
		}
		seen[v] = true
	}
	return nil
}

func validSlug(s string) bool {
	if s == "" || len(s) > 50 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return !strings.Contains(s, "--")
}

// ---- image processing (pure; tested in rosterdb_test.go) ----

// decoded is what the server derives from uploaded bytes.
type decoded struct {
	img       image.Image
	mime      string
	width     int
	height    int
	sha       string
	thumb     []byte
	thumbMime string
}

func mimeFor(format string) string {
	switch format {
	case "png":
		return "image/png"
	case "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

func isOpaque(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return false
}

// makeThumb scales the picture down to fit thumbMaxSide (never up) and
// encodes it — JPEG when opaque, PNG when it has transparency.
func makeThumb(img image.Image, maxSide int) ([]byte, string, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	tw, th := w, h
	if w > maxSide || h > maxSide {
		if w >= h {
			tw, th = maxSide, int(float64(h)*float64(maxSide)/float64(w)+0.5)
		} else {
			tw, th = int(float64(w)*float64(maxSide)/float64(h)+0.5), maxSide
		}
		if tw < 1 {
			tw = 1
		}
		if th < 1 {
			th = 1
		}
	}
	var buf bytes.Buffer
	if isOpaque(img) {
		dst := image.NewRGBA(image.Rect(0, 0, tw, th))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 84}); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), "image/jpeg", nil
	}
	dst := image.NewNRGBA(image.Rect(0, 0, tw, th))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	if err := png.Encode(&buf, dst); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/png", nil
}

// decodeImage is image.Decode plus the one correction the standard
// decoders need: x/image/webp hands back a lossy WebP as image.YCbCr,
// and Go's YCbCr→RGB math assumes JPEG's full-range luma, while VP8
// stores BT.601 limited range (16–235). Left alone, every thumb and crop
// made from a WebP came out flattened (dark +10, bright −14 — Gon's
// first card, 2026-09-20), so the studio-swing formula libwebp and the
// browsers use is applied here. Alpha WebPs (NYCbCrA) get the same.
func decodeImage(data []byte) (image.Image, string, error) {
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if format == "webp" {
		switch v := img.(type) {
		case *image.YCbCr:
			img = studioToFull(v, nil)
		case *image.NYCbCrA:
			img = studioToFull(&v.YCbCr, v)
		}
	}
	return img, format, nil
}

func studioToFull(src *image.YCbCr, alpha *image.NYCbCrA) *image.NRGBA {
	clamp := func(v float64) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v + 0.5)
	}
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := src.YCbCrAt(x, y)
			yy := 1.164 * (float64(c.Y) - 16)
			cb, cr := float64(c.Cb)-128, float64(c.Cr)-128
			a := uint8(255)
			if alpha != nil {
				a = alpha.A[alpha.AOffset(x, y)]
			}
			dst.SetNRGBA(x, y, color.NRGBA{clamp(yy + 1.596*cr), clamp(yy - 0.392*cb - 0.813*cr), clamp(yy + 2.017*cb), a})
		}
	}
	return dst
}

// decodeUpload validates bytes as a picture and derives everything the
// row needs. GIFs keep only their first frame.
func decodeUpload(data []byte) (*decoded, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrBadInput)
	}
	img, format, err := decodeImage(data)
	if err != nil {
		return nil, fmt.Errorf("%w: not a png/jpeg/gif/webp image", ErrBadInput)
	}
	if format == "gif" {
		if g, err := gif.Decode(bytes.NewReader(data)); err == nil {
			img = g
		}
	}
	b := img.Bounds()
	if b.Dx() < 1 || b.Dy() < 1 {
		return nil, fmt.Errorf("%w: empty image", ErrBadInput)
	}
	sum := sha256.Sum256(data)
	thumb, tmime, err := makeThumb(img, thumbMaxSide)
	if err != nil {
		return nil, err
	}
	return &decoded{img: img, mime: mimeFor(format), width: b.Dx(), height: b.Dy(),
		sha: hex.EncodeToString(sum[:]), thumb: thumb, thumbMime: tmime}, nil
}

type CropRect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func (c CropRect) within(w, h int) error {
	if c.W < 1 || c.H < 1 || c.X < 0 || c.Y < 0 || c.X+c.W > w || c.Y+c.H > h {
		return fmt.Errorf("%w: crop %dx%d+%d+%d outside %dx%d", ErrBadInput, c.W, c.H, c.X, c.Y, w, h)
	}
	return nil
}

// cropPNG cuts the exact source pixels and encodes them losslessly.
func cropPNG(img image.Image, c CropRect) ([]byte, error) {
	b := img.Bounds()
	if err := c.within(b.Dx(), b.Dy()); err != nil {
		return nil, err
	}
	r := image.Rect(b.Min.X+c.X, b.Min.Y+c.Y, b.Min.X+c.X+c.W, b.Min.Y+c.Y+c.H)
	var out image.Image
	if s, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		out = s.SubImage(r)
	} else {
		dst := image.NewNRGBA(image.Rect(0, 0, c.W, c.H))
		draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
		out = dst
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- store ----

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

const charCols = `c.id, c.card_number, c.name, c.name_ja, c.first, c.rank, c.nen_types, c.affiliation, c.arcs, c.arms,
	c.description, c.card_description, c.notes, c.version, c.review_status, c.review_reason, c.avatar_image_id, c.card_image_id, c.owner, c.created_at, c.updated_at,
	c.accepted_version, (SELECT count(*) FROM hxh_char_request q WHERE q.char_id = c.id AND q.status = 'open'),
	(SELECT count(*) FROM hxh_char_image i WHERE i.char_id = c.id),
	COALESCE((SELECT json_object_agg(t.type, t.n) FROM (SELECT type, count(*) AS n FROM hxh_char_image i WHERE i.char_id = c.id GROUP BY type) t), '{}'::json)`

func scanChar(row pgx.Row) (*Char, error) {
	var c Char
	var nen, arcs, arms string
	var counts []byte
	if err := row.Scan(&c.ID, &c.CardNumber, &c.Name, &c.NameJA, &c.First, &c.Rank, &nen, &c.Affiliation, &arcs, &arms,
		&c.Description, &c.CardDesc, &c.Notes, &c.Version, &c.ReviewStatus, &c.ReviewReason, &c.AvatarImageID, &c.CardImageID, &c.Owner, &c.CreatedAt, &c.UpdatedAt,
		&c.AcceptedVersion, &c.OpenRequests, &c.ImageCount, &counts); err != nil {
		return nil, err
	}
	c.NenTypes, c.Arcs, c.Arms = splitSlugs(nen), splitSlugs(arcs), splitSlugs(arms)
	c.ImageCounts = map[string]int{}
	if len(counts) > 0 {
		if err := json.Unmarshal(counts, &c.ImageCounts); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

func (s *Store) ListChars(status, name string) ([]Char, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT `+charCols+` FROM hxh_char c
		WHERE ($1 = '' OR c.review_status = $1) AND ($2 = '' OR lower(c.name) = lower($2)) ORDER BY c.card_number, c.id`, status, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Char{}
	for rows.Next() {
		c, err := scanChar(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) GetChar(id int64) (*Char, error) {
	ctx, cancel := withCtx()
	defer cancel()
	c, err := scanChar(s.pool.QueryRow(ctx, `SELECT `+charCols+` FROM hxh_char c WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+imageCols+` FROM hxh_char_image i WHERE i.char_id = $1
		ORDER BY i.created_at DESC, i.id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	c.Images = []Image{}
	for rows.Next() {
		im, err := scanImage(rows)
		if err != nil {
			return nil, err
		}
		c.Images = append(c.Images, *im)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rv, err := s.pool.Query(ctx, `SELECT id, char_id, version, status, reason, owner, created_at FROM hxh_char_review
		WHERE char_id = $1 ORDER BY created_at DESC, id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rv.Close()
	c.Reviews = []Review{}
	for rv.Next() {
		var r Review
		if err := rv.Scan(&r.ID, &r.CharID, &r.Version, &r.Status, &r.Reason, &r.Owner, &r.CreatedAt); err != nil {
			return nil, err
		}
		c.Reviews = append(c.Reviews, r)
	}
	if err := rv.Err(); err != nil {
		return nil, err
	}
	rq, err := s.pool.Query(ctx, `SELECT `+requestCols+requestFrom+`WHERE q.char_id = $1 ORDER BY q.created_at DESC, q.id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rq.Close()
	c.Requests = []Request{}
	for rq.Next() {
		var q Request
		if err := scanRequest(rq, &q); err != nil {
			return nil, err
		}
		c.Requests = append(c.Requests, q)
	}
	if err := rq.Err(); err != nil {
		return nil, err
	}
	var b Baseline
	err = s.pool.QueryRow(ctx, `SELECT version, status, owner, created_at FROM hxh_char_review
		WHERE char_id = $1 AND status IN ('accepted', 'rejected') ORDER BY created_at DESC, id DESC LIMIT 1`, id).Scan(&b.Version, &b.Status, &b.Owner, &b.CreatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	c.Changes = []Change{}
	if err == nil {
		c.Baseline = &b
		ch, err := s.pool.Query(ctx, `SELECT id, char_id, version, kind, field, image_id, action, old, new, owner, bot, created_at
			FROM hxh_char_change WHERE char_id = $1 AND version > $2 ORDER BY version, id`, id, b.Version)
		if err != nil {
			return nil, err
		}
		defer ch.Close()
		for ch.Next() {
			var x Change
			if err := ch.Scan(&x.ID, &x.CharID, &x.Version, &x.Kind, &x.Field, &x.ImageID, &x.Action, &x.Old, &x.New, &x.Owner, &x.Bot, &x.CreatedAt); err != nil {
				return nil, err
			}
			c.Changes = append(c.Changes, x)
		}
		if err := ch.Err(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// changed records what a write did: one version bump, the rows logged
// under it, and — when the BOT touched an accepted or rejected
// character — the character back to pending with a review-log line
// saying why, since the verdict was on a version that no longer
// exists. A human's change bumps the version but is self-approved and
// leaves the verdict alone (Andrew, 2026-09-21). No rows, no bump.
func (s *Store) changed(ctx context.Context, charID int64, by string, bot bool, rows []Change) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int
	var status string
	if err := tx.QueryRow(ctx, `UPDATE hxh_char SET version = version + 1, updated_at = NOW() WHERE id = $1 RETURNING version, review_status`, charID).Scan(&version, &status); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_change (char_id, version, kind, field, image_id, action, old, new, owner, bot)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, charID, version, r.Kind, r.Field, r.ImageID, r.Action, r.Old, r.New, by, bot); err != nil {
			return err
		}
	}
	switch {
	case bot && (status == "accepted" || status == "rejected"):
		if _, err := tx.Exec(ctx, `UPDATE hxh_char SET review_status = 'pending', review_reason = '' WHERE id = $1`, charID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_review (char_id, version, status, reason, owner) VALUES ($1, $2, 'pending', $3, $4)`,
			charID, version, summarize(rows), by); err != nil {
			return err
		}
	case !bot && status == "accepted":
		// self-approved: the accepted card follows a person's edit at once
		if _, err := tx.Exec(ctx, `UPDATE hxh_char SET accepted_version = version, accepted_snapshot = `+snapshotExpr+` WHERE id = $1`, charID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// summarize is the review log's line for a bot change: "changed
// description, notes", "added 3 pictures", "removed a picture".
func summarize(rows []Change) string {
	var fields []string
	counts := map[string]int{}
	for _, r := range rows {
		if r.Kind == "field" {
			fields = append(fields, r.Field)
		} else {
			counts[r.Action]++
		}
	}
	var parts []string
	if len(fields) > 0 {
		parts = append(parts, "changed "+strings.Join(fields, ", "))
	}
	for _, act := range []string{"added", "removed", "edited"} {
		switch n := counts[act]; {
		case n == 1:
			parts = append(parts, act+" a picture")
		case n > 1:
			parts = append(parts, fmt.Sprintf("%s %d pictures", act, n))
		}
	}
	return strings.Join(parts, "; ")
}

// diffChar lists the fields whose stored value would change, with old
// and new as the strings the API shows (lists joined by ", ").
func diffChar(before, after *Char) []Change {
	var out []Change
	set := func(field, o, n string) {
		if o != n {
			out = append(out, Change{Kind: "field", Field: field, Action: "set", Old: o, New: n})
		}
	}
	ref := func(p *int64) string {
		if p == nil {
			return ""
		}
		return strconv.FormatInt(*p, 10)
	}
	set("name", before.Name, after.Name)
	set("name_ja", before.NameJA, after.NameJA)
	set("first", before.First, after.First)
	set("rank", before.Rank, after.Rank)
	set("nen_types", strings.Join(before.NenTypes, ", "), strings.Join(after.NenTypes, ", "))
	set("affiliation", before.Affiliation, after.Affiliation)
	set("arcs", strings.Join(before.Arcs, ", "), strings.Join(after.Arcs, ", "))
	set("arms", strings.Join(before.Arms, ", "), strings.Join(after.Arms, ", "))
	set("description", before.Description, after.Description)
	set("card_description", before.CardDesc, after.CardDesc)
	set("notes", before.Notes, after.Notes)
	set("avatar_image_id", ref(before.AvatarImageID), ref(after.AvatarImageID))
	set("card_image_id", ref(before.CardImageID), ref(after.CardImageID))
	set("card_number", strconv.Itoa(before.CardNumber), strconv.Itoa(after.CardNumber))
	return out
}

// errFrozen is the answer to any verdict, edit, picture or request on a
// skipped stub.
var errFrozen = fmt.Errorf("%w: a skipped character is frozen until the bot resurrects it", ErrBadInput)

// frozen says whether the character is a skipped stub (ErrNotFound if
// there is no such character).
func (s *Store) frozen(ctx context.Context, id int64) error {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT review_status FROM hxh_char WHERE id = $1`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if st == "skipped" {
		return errFrozen
	}
	return nil
}

// Resurrect puts a skipped stub back to pending — the bot's act, on
// Andrew's word — so the reviewers see it and the bot researches it.
func (s *Store) Resurrect(id int64, by string) (*Char, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int
	err = tx.QueryRow(ctx, `UPDATE hxh_char SET review_status = 'pending', review_reason = '' WHERE id = $1 AND review_status = 'skipped' RETURNING version`, id).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.frozen(ctx, id); errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: character %d is not skipped", ErrBadInput, id)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_review (char_id, version, status, reason, owner) VALUES ($1, $2, 'pending', 'resurrected', $3)`, id, version, by); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetChar(id)
}

// snapshotExpr is the accepted card as JSON, built from the row itself
// so Accept and a person's edit write the same shape (and the 014
// changeset's backfill matches it). Keys are BinderCard's.
const snapshotExpr = `jsonb_build_object(
	'name', name, 'first', first, 'rank', rank,
	'nen_types', COALESCE(to_jsonb(string_to_array(NULLIF(nen_types, ''), ',')), '[]'::jsonb),
	'affiliation', affiliation,
	'arcs', COALESCE(to_jsonb(string_to_array(NULLIF(arcs, ''), ',')), '[]'::jsonb),
	'arms', COALESCE(to_jsonb(string_to_array(NULLIF(arms, ''), ',')), '[]'::jsonb),
	'card_description', card_description, 'description', description,
	'avatar_image_id', avatar_image_id, 'card_image_id', card_image_id)`

// Review records a verdict on the character's current version. A
// rejection may carry a reason (Andrew, 2026-09-19: optional). Accept
// moves accepted_version to this version and snapshots the card the
// Binder will print; requests are untouched by any verdict. A bot
// never passes a verdict (Andrew, 2026-09-21).
func (s *Store) Review(id int64, status, reason, owner string, bot bool) (*Char, error) {
	if !in(Verdicts, status) {
		return nil, fmt.Errorf("%w: status must be pending, accepted or rejected", ErrBadInput)
	}
	if bot {
		return nil, fmt.Errorf("%w: a bot cannot pass a verdict", ErrBadInput)
	}
	if err := s.frozen(context.Background(), id); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if status != "rejected" {
		reason = ""
	}
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int
	q := `UPDATE hxh_char SET review_status = $2, review_reason = $3 WHERE id = $1 RETURNING version`
	if status == "accepted" {
		q = `UPDATE hxh_char SET review_status = $2, review_reason = $3, accepted_version = version, accepted_snapshot = ` + snapshotExpr + ` WHERE id = $1 RETURNING version`
	}
	err = tx.QueryRow(ctx, q, id, status, reason).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_review (char_id, version, status, reason, owner) VALUES ($1, $2, $3, $4, $5)`,
		id, version, status, reason, owner); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetChar(id)
}

/* ---------- requests: a reviewer asks the bot for work ---------- */

// RequestKind is a category of work a reviewer can ask for (Andrew,
// 2026-09-21: "Extend picture", "Card description", more later). Slugs
// live in hxh_request_kind so a new kind is a changeset, not a release.
type RequestKind struct {
	Slug      string `json:"slug"`
	Label     string `json:"label"`
	Sort      int    `json:"sort"`
	Scope     string `json:"scope"`      // character | image | any
	NeedsText bool   `json:"needs_text"` // "Other…": the details are the request
}

// Request is one ask: a kind, free text, and its state — open until the
// bot resolves it (done) or the reviewer's own verdict overrides it
// (withdrawn). While a character has an open request its review_status
// is "requested"; resolving the last one puts it back to "pending".
type Request struct {
	ID         int64      `json:"id"`
	CharID     int64      `json:"char_id"`
	CharName   string     `json:"char_name,omitempty"`
	Kind       string     `json:"kind"`
	Label      string     `json:"label"`
	Text       string     `json:"text"`
	Status     string     `json:"status"`
	Version    int        `json:"version"`
	Owner      string     `json:"owner"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedBy string     `json:"resolved_by"`
	ResolvedAt *time.Time `json:"resolved_at"`
	Resolution string     `json:"resolution"` // how it ended, in the resolver's words
	ImageID    *int64     `json:"image_id"`   // set for a request on one picture
}

// RequestStatus: open until the bot does it (done) or someone lets it go (dropped).
var RequestStatus = []string{"open", "done", "dropped"}

const requestCols = `q.id, q.char_id, q.kind, k.label, q.text, q.status, q.version, q.owner, q.created_at, q.resolved_by, q.resolved_at, q.resolution, q.image_id`
const requestFrom = ` FROM hxh_char_request q JOIN hxh_request_kind k ON k.slug = q.kind `

func scanRequest(row pgx.Row, q *Request) error {
	return row.Scan(&q.ID, &q.CharID, &q.Kind, &q.Label, &q.Text, &q.Status, &q.Version, &q.Owner, &q.CreatedAt, &q.ResolvedBy, &q.ResolvedAt, &q.Resolution, &q.ImageID)
}

func (s *Store) RequestKinds() ([]RequestKind, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT slug, label, sort, scope, needs_text FROM hxh_request_kind ORDER BY sort, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestKind{}
	for rows.Next() {
		var k RequestKind
		if err := rows.Scan(&k.Slug, &k.Label, &k.Sort, &k.Scope, &k.NeedsText); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Request files an ask against the character's current version and
// marks the character "requested" until the bot resolves it.
func (s *Store) Request(charID int64, kind, text string, imageID *int64, owner string) (*Char, error) {
	kind, text = strings.TrimSpace(kind), strings.TrimSpace(text)
	ctx, cancel := withCtx()
	defer cancel()
	var label, scope string
	var needsText bool
	err := s.pool.QueryRow(ctx, `SELECT label, scope, needs_text FROM hxh_request_kind WHERE slug = $1`, kind).Scan(&label, &scope, &needsText)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: unknown request kind %q", ErrBadInput, kind)
	}
	if err != nil {
		return nil, err
	}
	if err := s.frozen(ctx, charID); err != nil {
		return nil, err
	}
	want := "character"
	if imageID != nil {
		want = "image"
	}
	if scope != "any" && scope != want {
		return nil, fmt.Errorf("%w: %s is not a %s request", ErrBadInput, label, want)
	}
	if needsText && text == "" {
		return nil, fmt.Errorf("%w: %s needs details", ErrBadInput, label)
	}
	if imageID != nil {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM hxh_char_image WHERE id = $1 AND char_id = $2`, *imageID, charID).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: picture %d is not this character's", ErrBadInput, *imageID)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int
	err = tx.QueryRow(ctx, `SELECT version FROM hxh_char WHERE id = $1`, charID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_request (char_id, kind, text, version, owner, image_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		charID, kind, text, version, owner, imageID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetChar(charID)
}

// ListRequests is the queue: open ones first, oldest first within.
func (s *Store) ListRequests(status string) ([]Request, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT `+requestCols+`, c.name`+requestFrom+`JOIN hxh_char c ON c.id = q.char_id
		WHERE ($1 = '' OR q.status = $1) ORDER BY (q.status = 'open') DESC, q.created_at, q.id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var q Request
		if err := rows.Scan(&q.ID, &q.CharID, &q.Kind, &q.Label, &q.Text, &q.Status, &q.Version, &q.Owner, &q.CreatedAt, &q.ResolvedBy, &q.ResolvedAt, &q.Resolution, &q.ImageID, &q.CharName); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ResolveRequest closes an open request: done (the work happened) or
// dropped (nobody will do it — the bot could not, or the reviewer let it
// go), with a note in the resolver's words. Verdicts are untouched.
func (s *Store) ResolveRequest(id int64, by, status, note string) (*Request, error) {
	if status == "" {
		status = "done"
	}
	if status == "open" || !in(RequestStatus, status) {
		return nil, fmt.Errorf("%w: a request is resolved as done or dropped", ErrBadInput)
	}
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var charID int64
	err = tx.QueryRow(ctx, `UPDATE hxh_char_request SET status = $3, resolved_by = $2, resolved_at = NOW(), resolution = $4
		WHERE id = $1 AND status = 'open' RETURNING char_id`, id, by, status, strings.TrimSpace(note)).Scan(&charID)
	if errors.Is(err, pgx.ErrNoRows) {
		var st string
		if err := tx.QueryRow(ctx, `SELECT status FROM hxh_char_request WHERE id = $1`, id).Scan(&st); err != nil {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: request #%d is already %s", ErrBadInput, id, st)
	}
	if err != nil {
		return nil, err
	}
	var q Request
	if err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestCols+requestFrom+`WHERE q.id = $1`, id), &q); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &q, nil
}

func (s *Store) CreateChar(c Char) (*Char, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return nil, fmt.Errorf("%w: name required", ErrBadInput)
	}
	if c.Rank == "" {
		c.Rank = "C"
	}
	if c.ReviewStatus != "skipped" {
		c.ReviewStatus, c.ReviewReason = "pending", ""
	}
	c.ReviewReason = strings.TrimSpace(c.ReviewReason)
	if err := validateCharValues(&c); err != nil {
		return nil, err
	}
	ctx, cancel := withCtx()
	defer cancel()
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO hxh_char
		(name, name_ja, first, rank, nen_types, affiliation, arcs, arms, description, card_description, notes, owner, review_status, review_reason, card_number)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, (SELECT COALESCE(MAX(card_number), 0) + 1 FROM hxh_char)) RETURNING id`,
		c.Name, c.NameJA, c.First, c.Rank, joinSlugs(c.NenTypes), c.Affiliation,
		joinSlugs(c.Arcs), joinSlugs(c.Arms), c.Description, c.CardDesc, c.Notes, c.Owner, c.ReviewStatus, c.ReviewReason).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetChar(id)
}

func validateCharValues(c *Char) error {
	if !in(Ranks, c.Rank) {
		return fmt.Errorf("%w: rank must be S, A, B or C", ErrBadInput)
	}
	if !in(CharStatus, c.ReviewStatus) {
		return fmt.Errorf("%w: review_status must be pending, accepted, rejected or skipped", ErrBadInput)
	}
	if err := validSlugList(c.NenTypes, NenTypes, 2); err != nil {
		return fmt.Errorf("nen_types %w", err)
	}
	if err := validSlugList(c.Arcs, ArcSlugs, len(ArcSlugs)); err != nil {
		return fmt.Errorf("arcs %w", err)
	}
	for _, a := range c.Arms {
		if !validSlug(a) {
			return fmt.Errorf("%w: arms must be kebab slugs, got %q", ErrBadInput, a)
		}
	}
	if len(c.First) > 60 || len(c.NameJA) > 100 || len(c.Affiliation) > 60 || len(c.Name) > 100 { // first: no short cap — the card plaque shrinks a long name (Andrew, 2026-09-22)
		return fmt.Errorf("%w: field too long", ErrBadInput)
	}
	return nil
}

// charPatch is the JSON body of PATCH /chars/{id}: any subset of the
// editable fields. Unknown keys are an error so typos never pass silently.
// The review verdict is not a field — see Review.
var patchKeys = []string{"name", "name_ja", "first", "rank", "nen_types", "affiliation", "arcs", "arms",
	"description", "card_description", "notes", "avatar_image_id", "card_image_id", "card_number"}

// applyPatch merges a decoded patch into c, validating as it goes.
func applyPatch(c *Char, patch map[string]json.RawMessage) error {
	for k := range patch {
		if !in(patchKeys, k) {
			return fmt.Errorf("%w: unknown field %q", ErrBadInput, k)
		}
	}
	str := func(k string, dst *string) error {
		raw, ok := patch[k]
		if !ok {
			return nil
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%w: %s must be a string", ErrBadInput, k)
		}
		*dst = v
		return nil
	}
	list := func(k string, dst *[]string) error {
		raw, ok := patch[k]
		if !ok {
			return nil
		}
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%w: %s must be a list of strings", ErrBadInput, k)
		}
		if v == nil {
			v = []string{}
		}
		*dst = v
		return nil
	}
	ref := func(k string, dst **int64) error {
		raw, ok := patch[k]
		if !ok {
			return nil
		}
		var v *int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%w: %s must be an image id or null", ErrBadInput, k)
		}
		*dst = v
		return nil
	}
	for k, dst := range map[string]*string{"name": &c.Name, "name_ja": &c.NameJA, "first": &c.First,
		"rank": &c.Rank, "affiliation": &c.Affiliation, "description": &c.Description, "card_description": &c.CardDesc, "notes": &c.Notes} {
		if err := str(k, dst); err != nil {
			return err
		}
	}
	for k, dst := range map[string]*[]string{"nen_types": &c.NenTypes, "arcs": &c.Arcs, "arms": &c.Arms} {
		if err := list(k, dst); err != nil {
			return err
		}
	}
	for k, dst := range map[string]**int64{"avatar_image_id": &c.AvatarImageID, "card_image_id": &c.CardImageID} {
		if err := ref(k, dst); err != nil {
			return err
		}
	}
	if raw, ok := patch["card_number"]; ok {
		var v int
		if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
			return fmt.Errorf("%w: card_number must be a whole number", ErrBadInput)
		}
		c.CardNumber = v
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return fmt.Errorf("%w: name required", ErrBadInput)
	}
	return validateCharValues(c)
}

func (s *Store) UpdateChar(id int64, patch map[string]json.RawMessage, by string, bot bool) (*Char, error) {
	before, err := s.GetChar(id)
	if err != nil {
		return nil, err
	}
	if before.ReviewStatus == "skipped" {
		return nil, errFrozen
	}
	next := *before
	c := &next
	if err := applyPatch(c, patch); err != nil {
		return nil, err
	}
	rows := diffChar(before, c)
	if len(rows) == 0 {
		return before, nil
	}
	ctx, cancel := withCtx()
	defer cancel()
	for _, ref := range []*int64{c.AvatarImageID, c.CardImageID} {
		if ref == nil {
			continue
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM hxh_char_image WHERE id = $1 AND char_id = $2`, *ref, id).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: image %d is not this character's", ErrBadInput, *ref)
		}
	}
	_, err = s.pool.Exec(ctx, `UPDATE hxh_char SET name=$2, name_ja=$3, first=$4, rank=$5, nen_types=$6,
		affiliation=$7, arcs=$8, arms=$9, description=$10, card_description=$11, notes=$12, avatar_image_id=$13, card_image_id=$14, card_number=$15,
		updated_at=NOW() WHERE id=$1`,
		id, c.Name, c.NameJA, c.First, c.Rank, joinSlugs(c.NenTypes), c.Affiliation, joinSlugs(c.Arcs),
		joinSlugs(c.Arms), c.Description, c.CardDesc, c.Notes, c.AvatarImageID, c.CardImageID, c.CardNumber)
	if err != nil {
		return nil, err
	}
	if err := s.changed(ctx, id, by, bot, rows); err != nil {
		return nil, err
	}
	return s.GetChar(id)
}

func (s *Store) DeleteChar(id int64) error {
	ctx, cancel := withCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `DELETE FROM hxh_char WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const imageCols = `i.id, i.char_id, i.type, i.source_image_id, i.mime, i.width, i.height, length(i.data), i.sha256,
	i.source_url, i.caption, i.status, i.owner, i.created_at`

func scanImage(row pgx.Row) (*Image, error) {
	var im Image
	if err := row.Scan(&im.ID, &im.CharID, &im.Type, &im.SourceImageID, &im.Mime, &im.Width, &im.Height, &im.Bytes,
		&im.SHA256, &im.SourceURL, &im.Caption, &im.Status, &im.Owner, &im.CreatedAt); err != nil {
		return nil, err
	}
	im.SHA256 = strings.TrimSpace(im.SHA256)
	return &im, nil
}

func (s *Store) GetImage(id int64) (*Image, error) {
	ctx, cancel := withCtx()
	defer cancel()
	im, err := scanImage(s.pool.QueryRow(ctx, `SELECT `+imageCols+` FROM hxh_char_image i WHERE i.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return im, err
}

// ImageData returns the picture (thumb=false) or its preview (thumb=true).
func (s *Store) ImageData(id int64, thumb bool) (mimeType string, data []byte, err error) {
	ctx, cancel := withCtx()
	defer cancel()
	col := "mime, data"
	if thumb {
		col = "thumb_mime, thumb"
	}
	err = s.pool.QueryRow(ctx, `SELECT `+col+` FROM hxh_char_image WHERE id = $1`, id).Scan(&mimeType, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return mimeType, data, err
}

// AddImage stores a picture for a character. When the same bytes already
// exist for that character the existing row comes back with created=false
// (its status tells the caller whether it was rejected before).
func (s *Store) AddImage(charID int64, typ string, sourceID *int64, sourceURL, caption, by string, bot bool, data []byte) (im *Image, created bool, err error) {
	if !in(ImageTypes, typ) {
		return nil, false, fmt.Errorf("%w: type must be one of %s", ErrBadInput, strings.Join(ImageTypes, ", "))
	}
	if len(caption) > 200 {
		caption = caption[:200]
	}
	d, err := decodeUpload(data)
	if err != nil {
		return nil, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.frozen(ctx, charID); err != nil {
		return nil, false, err
	}
	if sourceID != nil {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM hxh_char_image WHERE id = $1 AND char_id = $2`, *sourceID, charID).Scan(&n); err != nil {
			return nil, false, err
		}
		if n == 0 {
			return nil, false, fmt.Errorf("%w: source image %d is not this character's", ErrBadInput, *sourceID)
		}
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO hxh_char_image
		(char_id, type, source_image_id, mime, width, height, data, thumb, thumb_mime, sha256, source_url, caption, owner)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		charID, typ, sourceID, d.mime, d.width, d.height, data, d.thumb, d.thumbMime, d.sha, sourceURL, caption, by).Scan(&id)
	if isUnique(err) {
		err = s.pool.QueryRow(ctx, `SELECT id FROM hxh_char_image WHERE char_id = $1 AND sha256 = $2`, charID, d.sha).Scan(&id)
		if err != nil {
			return nil, false, err
		}
		im, err := s.GetImage(id)
		return im, false, err
	}
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23503" {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}
	if err := s.changed(ctx, charID, by, bot, []Change{{Kind: "image", Action: "added", ImageID: &id}}); err != nil {
		return nil, false, err
	}
	im, err = s.GetImage(id)
	return im, true, err
}

func (s *Store) UpdateImage(id int64, status, caption *string, by string, bot bool) (*Image, error) {
	if status != nil && !in(ImgStatus, *status) {
		return nil, fmt.Errorf("%w: status must be kept or rejected", ErrBadInput)
	}
	if caption != nil && len(*caption) > 200 {
		return nil, fmt.Errorf("%w: caption too long", ErrBadInput)
	}
	ctx, cancel := withCtx()
	defer cancel()
	var charID int64
	err := s.pool.QueryRow(ctx, `UPDATE hxh_char_image SET status = COALESCE($2, status), caption = COALESCE($3, caption) WHERE id = $1 RETURNING char_id`, id, status, caption).Scan(&charID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.changed(ctx, charID, by, bot, []Change{{Kind: "image", Action: "edited", ImageID: &id}}); err != nil {
		return nil, err
	}
	return s.GetImage(id)
}

func (s *Store) DeleteImage(id int64, by string, bot bool) error {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var used int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM hxh_char WHERE (accepted_snapshot->>'avatar_image_id')::bigint = $1 OR (accepted_snapshot->>'card_image_id')::bigint = $1`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return fmt.Errorf("%w: picture %d is on the accepted card; accept another picture first", ErrConflict, id)
	}
	var charID int64
	err = tx.QueryRow(ctx, `DELETE FROM hxh_char_image WHERE id = $1 RETURNING char_id`, id).Scan(&charID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// a request on a picture that is gone is moot: withdrawn, in the deleter's name
	if _, err := tx.Exec(ctx, `UPDATE hxh_char_request SET status = 'dropped', resolved_by = $2, resolved_at = NOW(), resolution = 'picture deleted'
		WHERE image_id = $1 AND status = 'open'`, id, by); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return s.changed(ctx, charID, by, bot, []Change{{Kind: "image", Action: "removed", ImageID: &id}})
}

// CropImage cuts rect out of image id at native resolution and stores the
// result as a "cropped" image of the same character.
func (s *Store) CropImage(id int64, rect CropRect, by string, bot bool) (*Image, bool, error) {
	src, err := s.GetImage(id)
	if err != nil {
		return nil, false, err
	}
	_, data, err := s.ImageData(id, false)
	if err != nil {
		return nil, false, err
	}
	img, _, err := decodeImage(data)
	if err != nil {
		return nil, false, fmt.Errorf("decode stored image %d: %w", id, err)
	}
	out, err := cropPNG(img, rect)
	if err != nil {
		return nil, false, err
	}
	return s.AddImage(src.CharID, "cropped", &id, "", src.Caption, by, bot, out)
}

// ---- handlers ----

// requireHxh resolves the shared session and, with admin=true, insists on
// role admin for hxh; otherwise any hxh role will do. A site-wide admin
// (role admin on the "admin" website — e.g. the skill's bot user
// "claude") passes either check. The username lands in the request
// context (ctxUser / userOf, shared with chat.go).
func requireHxh(auth *shared.Store, admin bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(shared.CookieName)
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			username, ok := auth.GetSession(cookie.Value)
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			allowed, err := auth.HasRole(username, "admin", "admin")
			if err == nil && !allowed {
				if admin {
					allowed, err = auth.HasRole(username, "hxh", "admin")
				} else {
					allowed, err = auth.IsMember(username, "hxh")
				}
			}
			if err != nil {
				log.Printf("[hxh db] role check error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			bot, err := auth.IsBot(username)
			if err != nil {
				log.Printf("[hxh db] bot check error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			ctx := context.WithValue(context.WithValue(r.Context(), ctxUser, username), ctxBot, bot)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

const ctxBot ctxKey = 2

// botOf says whether the request's user is a bot (the change log's
// litmus: a bot's change needs review, a person's is self-approved).
func botOf(r *http.Request) bool {
	b, _ := r.Context().Value(ctxBot).(bool)
	return b
}

func idParam(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return id, err == nil && id > 0
}

func fail(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": strings.TrimPrefix(err.Error(), ErrConflict.Error()+": ")})
	case errors.Is(err, ErrBadInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), ErrBadInput.Error()+": ")})
	default:
		log.Printf("[hxh db] %s: %v", what, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

// MountRosterDB wires the Roster DB under <prefix>/db.
func MountRosterDB(r chi.Router, store *Store, auth *shared.Store) {
	r.Route("/db", func(g chi.Router) {
		g.Group(func(m chi.Router) {
			m.Use(requireHxh(auth, false))
			m.Get("/binder", handleBinder(store))
			m.Get("/stamps", handleStamps(store))
			m.Post("/chars/{id}/stamp", handleStamp(store))
			m.Get("/images/{id}", handleImageData(store, false))
			m.Get("/images/{id}/thumb", handleImageData(store, true))
		})
		g.Group(func(a chi.Router) {
			a.Use(requireHxh(auth, true))
			a.Get("/chars", handleListChars(store))
			a.Post("/chars", handleCreateChar(store))
			a.Get("/chars/{id}", handleGetChar(store))
			a.Patch("/chars/{id}", handlePatchChar(store))
			a.Post("/chars/{id}/review", handleReview(store))
			a.Post("/chars/{id}/request", handleRequest(store))
			a.Post("/chars/{id}/move", handleMove(store))
			a.Post("/chars/{id}/resurrect", handleResurrect(store))
			a.Get("/request-kinds", handleRequestKinds(store))
			a.Get("/requests", handleListRequests(store))
			a.Post("/requests/{id}/resolve", handleResolveRequest(store))
			a.Delete("/chars/{id}", handleDeleteChar(store))
			a.Post("/chars/{id}/images", handleUpload(store))
			a.Get("/images/{id}/meta", handleImageMeta(store))
			a.Patch("/images/{id}", handlePatchImage(store))
			a.Delete("/images/{id}", handleDeleteImage(store))
			a.Post("/images/{id}/crop", handleCrop(store))
		})
	})
}

// BinderCard is what a guest's Binder needs to print one card — accepted
// characters only, no notes, no review.
type BinderCard struct {
	ID            int64    `json:"id"`
	CardNumber    int      `json:"card_number"`
	Name          string   `json:"name"`
	First         string   `json:"first"`
	Rank          string   `json:"rank"`
	NenTypes      []string `json:"nen_types"`
	Affiliation   string   `json:"affiliation"`
	Arcs          []string `json:"arcs"`
	Arms          []string `json:"arms"`
	CardDesc      string   `json:"card_description"`
	Description   string   `json:"description"`
	AvatarImageID *int64   `json:"avatar_image_id"`
	CardImageID   *int64   `json:"card_image_id"`
	Version       int      `json:"version"`
}

// Binder is what the party sees: every accepted snapshot that has both
// pictures, in card-number order. The live row does not matter here —
// a bot's edit shows nowhere until a reviewer accepts it.
func (s *Store) Binder() ([]BinderCard, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT id, card_number, accepted_version, accepted_snapshot FROM hxh_char
		WHERE accepted_snapshot IS NOT NULL ORDER BY card_number, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BinderCard{}
	for rows.Next() {
		var card BinderCard
		var snap []byte
		var accepted int
		if err := rows.Scan(&card.ID, &card.CardNumber, &accepted, &snap); err != nil {
			return nil, err
		}
		id, no := card.ID, card.CardNumber
		if err := json.Unmarshal(snap, &card); err != nil {
			return nil, fmt.Errorf("snapshot of character %d: %w", id, err)
		}
		card.ID, card.CardNumber, card.Version = id, no, accepted
		if card.AvatarImageID == nil || card.CardImageID == nil { // a card needs both pictures (Andrew, 2026-09-21)
			continue
		}
		for _, p := range []*[]string{&card.NenTypes, &card.Arcs, &card.Arms} {
			if *p == nil {
				*p = []string{}
			}
		}
		out = append(out, card)
	}
	return out, rows.Err()
}

/* ---------- stamps: hearts (public, anonymous) and bookmarks (private) ---------- */

var StampKinds = []string{"heart", "bookmark"}

// Heart is one member's heart on a card, without the member: where it
// sits on the description box (% of the box) and how it leans.
type Heart struct {
	CharID   int64   `json:"char_id"`
	X        float32 `json:"x"`
	Y        float32 `json:"y"`
	Rotation float32 `json:"rotation"`
}

// Stamps is what one member sees: everyone's hearts, and which cards
// they themselves hearted or bookmarked.
type Stamps struct {
	Hearts    []Heart `json:"hearts"`
	Mine      []int64 `json:"hearts_mine"`
	Bookmarks []int64 `json:"bookmarks"`
}

func (s *Store) Stamps(username string) (*Stamps, error) {
	ctx, cancel := withCtx()
	defer cancel()
	out := &Stamps{Hearts: []Heart{}, Mine: []int64{}, Bookmarks: []int64{}}
	rows, err := s.pool.Query(ctx, `SELECT char_id, pos_x, pos_y, rotation, username = $1, kind FROM hxh_stamp ORDER BY created_at, id`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h Heart
		var mine bool
		var kind string
		if err := rows.Scan(&h.CharID, &h.X, &h.Y, &h.Rotation, &mine, &kind); err != nil {
			return nil, err
		}
		switch kind {
		case "heart":
			out.Hearts = append(out.Hearts, h)
			if mine {
				out.Mine = append(out.Mine, h.CharID)
			}
		case "bookmark":
			if mine {
				out.Bookmarks = append(out.Bookmarks, h.CharID)
			}
		}
	}
	return out, rows.Err()
}

// ToggleStamp adds the member's stamp of that kind on the card at the
// spot given, or removes it when it is already there. Answers whether
// it is on now. Only accepted (binder) cards take stamps.
func (s *Store) ToggleStamp(username string, charID int64, kind string, x, y, rot float32) (bool, error) {
	if !in(StampKinds, kind) {
		return false, fmt.Errorf("%w: kind must be heart or bookmark", ErrBadInput)
	}
	ctx, cancel := withCtx()
	defer cancel()
	var accepted bool
	err := s.pool.QueryRow(ctx, `SELECT accepted_snapshot IS NOT NULL FROM hxh_char WHERE id = $1`, charID).Scan(&accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if !accepted {
		return false, fmt.Errorf("%w: only a card in the binder takes a stamp", ErrBadInput)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM hxh_stamp WHERE username = $1 AND char_id = $2 AND kind = $3`, username, charID, kind)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return false, nil
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO hxh_stamp (username, char_id, kind, pos_x, pos_y, rotation) VALUES ($1, $2, $3, $4, $5, $6)`,
		username, charID, kind, x, y, rot)
	if isUnique(err) { // two presses raced: the first won, the stamp is on
		return true, nil
	}
	return err == nil, err
}

func handleStamps(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := store.Stamps(userOf(r))
		if err != nil {
			fail(w, err, "stamps")
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

// handleStamp: {kind, x, y, rotation} toggles the member's stamp on the card.
func handleStamp(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var body struct {
			Kind     string  `json:"kind"`
			X        float32 `json:"x"`
			Y        float32 `json:"y"`
			Rotation float32 `json:"rotation"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "stamp")
			return
		}
		on, err := store.ToggleStamp(userOf(r), id, body.Kind, body.X, body.Y, body.Rotation)
		if err != nil {
			fail(w, err, "stamp")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"on": on, "kind": body.Kind, "char_id": id, "x": body.X, "y": body.Y, "rotation": body.Rotation})
	}
}

func handleBinder(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := store.Binder()
		if err != nil {
			fail(w, err, "binder")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

/* ---------- card numbers: the binder order ---------- */

// numbered is a character's place in the binder order.
type numbered struct {
	ID int64
	N  int
}

// moveOrder places id right after after (0 = the front) in seq — every
// character ordered by (card_number, id) — and returns the new number
// of each character whose number changes. The numbers held between the
// old and the new position are handed out again in order, so the
// multiset of numbers is untouched (a duplicate stays a duplicate) and
// nothing outside that stretch moves. Andrew (2026-09-21): numbers are
// not unique on purpose; a renumbering is one atomic operation.
func moveOrder(seq []numbered, id, after int64) (map[int64]int, error) {
	p := -1
	for i, x := range seq {
		if x.ID == id {
			p = i
		}
	}
	if p < 0 {
		return nil, ErrNotFound
	}
	if after == id {
		return nil, nil
	}
	rest := make([]numbered, 0, len(seq))
	rest = append(rest, seq[:p]...)
	rest = append(rest, seq[p+1:]...)
	q := 0
	if after != 0 {
		q = -1
		for i, x := range rest {
			if x.ID == after {
				q = i + 1
			}
		}
		if q < 0 {
			return nil, fmt.Errorf("%w: no character %d to put it after", ErrBadInput, after)
		}
	}
	if q == p {
		return nil, nil
	}
	next := make([]numbered, 0, len(seq))
	next = append(next, rest[:q]...)
	next = append(next, seq[p])
	next = append(next, rest[q:]...)
	lo, hi := p, q
	if lo > hi {
		lo, hi = hi, lo
	}
	changes := map[int64]int{}
	for i := lo; i <= hi; i++ {
		if next[i].N != seq[i].N {
			changes[next[i].ID] = seq[i].N
		}
	}
	return changes, nil
}

// Move renumbers so that id sits right after after (0 = the front), in
// one transaction over the whole order, and answers with the whole
// list. Versions do not bump: a card's number is where it sits, not
// what it says.
func (s *Store) Move(id, after int64) ([]Char, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id, card_number FROM hxh_char ORDER BY card_number, id FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	var seq []numbered
	for rows.Next() {
		var x numbered
		if err := rows.Scan(&x.ID, &x.N); err != nil {
			rows.Close()
			return nil, err
		}
		seq = append(seq, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	changes, err := moveOrder(seq, id, after)
	if err != nil {
		return nil, err
	}
	for cid, n := range changes {
		if _, err := tx.Exec(ctx, `UPDATE hxh_char SET card_number = $2 WHERE id = $1`, cid, n); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.ListChars("", "")
}

// handleMove: {after: id} — put this character right after that one (0 = the front).
func handleMove(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var body struct {
			After int64 `json:"after"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "move")
			return
		}
		list, err := store.Move(id, body.After)
		if err != nil {
			fail(w, err, "move")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// handleResurrect: a skipped stub back to pending (the bot, on Andrew's word).
func handleResurrect(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		c, err := store.Resurrect(id, userOf(r))
		if err != nil {
			fail(w, err, "resurrect")
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func handleRequestKinds(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kinds, err := store.RequestKinds()
		if err != nil {
			fail(w, err, "request kinds")
			return
		}
		writeJSON(w, http.StatusOK, kinds)
	}
}

// handleRequest files a reviewer's ask: {kind, text}.
func handleRequest(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var body struct {
			Kind    string `json:"kind"`
			Text    string `json:"text"`
			ImageID *int64 `json:"image_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "request")
			return
		}
		c, err := store.Request(id, body.Kind, body.Text, body.ImageID, userOf(r))
		if err != nil {
			fail(w, err, "request")
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func handleListRequests(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqs, err := store.ListRequests(r.URL.Query().Get("status"))
		if err != nil {
			fail(w, err, "list requests")
			return
		}
		writeJSON(w, http.StatusOK, reqs)
	}
}

func handleResolveRequest(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var body struct {
			Status string `json:"status"`
			Note   string `json:"note"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
				fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "resolve request")
				return
			}
		}
		q, err := store.ResolveRequest(id, userOf(r), body.Status, body.Note)
		if err != nil {
			fail(w, err, "resolve request")
			return
		}
		writeJSON(w, http.StatusOK, q)
	}
}

func handleListChars(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chars, err := store.ListChars(r.URL.Query().Get("status"), r.URL.Query().Get("name"))
		if err != nil {
			fail(w, err, "list chars")
			return
		}
		writeJSON(w, http.StatusOK, chars)
	}
}

func handleCreateChar(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var c Char
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "create")
			return
		}
		c.Owner = userOf(r)
		out, err := store.CreateChar(c)
		if err != nil {
			fail(w, err, "create char")
			return
		}
		writeJSON(w, http.StatusCreated, out)
	}
}

func handleGetChar(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		c, err := store.GetChar(id)
		if err != nil {
			fail(w, err, "get char")
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func handlePatchChar(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var patch map[string]json.RawMessage
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&patch); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "patch")
			return
		}
		c, err := store.UpdateChar(id, patch, userOf(r), botOf(r))
		if err != nil {
			fail(w, err, "patch char")
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func handleReview(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var body struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "review")
			return
		}
		c, err := store.Review(id, body.Status, body.Reason, userOf(r), botOf(r))
		if err != nil {
			fail(w, err, "review")
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func handleDeleteChar(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		if err := store.DeleteChar(id); err != nil {
			fail(w, err, "delete char")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleUpload accepts either multipart/form-data (field "file" plus
// type, source_image_id, source_url, caption) — what the page's
// drag-and-drop sends — or the raw picture as the body with the same
// options as query parameters — what the skill's curl sends.
func handleUpload(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		charID, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
		var data []byte
		get := r.URL.Query().Get
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct == "multipart/form-data" {
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				fail(w, fmt.Errorf("%w: bad multipart body", ErrBadInput), "upload")
				return
			}
			f, _, err := r.FormFile("file")
			if err != nil {
				fail(w, fmt.Errorf("%w: missing file", ErrBadInput), "upload")
				return
			}
			defer f.Close()
			if data, err = io.ReadAll(f); err != nil {
				fail(w, fmt.Errorf("%w: body too large", ErrBadInput), "upload")
				return
			}
			get = r.FormValue
		} else {
			var err error
			if data, err = io.ReadAll(r.Body); err != nil {
				fail(w, fmt.Errorf("%w: body too large", ErrBadInput), "upload")
				return
			}
		}
		typ := get("type")
		if typ == "" {
			typ = "raw"
		}
		var sourceID *int64
		if s := get("source_image_id"); s != "" {
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil || n <= 0 {
				fail(w, fmt.Errorf("%w: bad source_image_id", ErrBadInput), "upload")
				return
			}
			sourceID = &n
		}
		im, created, err := store.AddImage(charID, typ, sourceID, get("source_url"), get("caption"), userOf(r), botOf(r), data)
		if err != nil {
			fail(w, err, "upload")
			return
		}
		status := http.StatusCreated
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"image": im, "created": created})
	}
}

func handleImageData(store *Store, thumb bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			http.NotFound(w, r)
			return
		}
		mimeType, data, err := store.ImageData(id, thumb)
		if err != nil {
			fail(w, err, "image data")
			return
		}
		// Image ids are immutable, so the global no-store is wrong here.
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		w.Header().Set("Content-Type", mimeType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

func handleImageMeta(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		im, err := store.GetImage(id)
		if err != nil {
			fail(w, err, "image meta")
			return
		}
		c, err := store.GetChar(im.CharID)
		if err != nil {
			fail(w, err, "image meta char")
			return
		}
		c.Images, c.Reviews = nil, nil
		writeJSON(w, http.StatusOK, map[string]any{"image": im, "char": c})
	}
}

func handlePatchImage(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var body struct {
			Status  *string `json:"status"`
			Caption *string `json:"caption"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "patch image")
			return
		}
		im, err := store.UpdateImage(id, body.Status, body.Caption, userOf(r), botOf(r))
		if err != nil {
			fail(w, err, "patch image")
			return
		}
		writeJSON(w, http.StatusOK, im)
	}
}

func handleDeleteImage(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		if err := store.DeleteImage(id, userOf(r), botOf(r)); err != nil {
			fail(w, err, "delete image")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleCrop(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := idParam(r, "id")
		if !ok {
			fail(w, ErrNotFound, "")
			return
		}
		var rect CropRect
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&rect); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "crop")
			return
		}
		im, created, err := store.CropImage(id, rect, userOf(r), botOf(r))
		if err != nil {
			fail(w, err, "crop")
			return
		}
		status := http.StatusCreated
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"image": im, "created": created})
	}
}
