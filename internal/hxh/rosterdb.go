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
	CharStatus = []string{"pending", "requested", "accepted", "rejected"}
	Verdicts   = []string{"pending", "accepted", "rejected"} // what a reviewer sets; "requested" comes only from a request
	ImgStatus  = []string{"kept", "rejected"}
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
	ID            int64          `json:"id"`
	CardNumber    int            `json:"card_number"` // the binder position; not unique on purpose (Andrew, 2026-09-21)
	Name          string         `json:"name"`
	NameJA        string         `json:"name_ja"`
	First         string         `json:"first"`
	Rank          string         `json:"rank"`
	NenTypes      []string       `json:"nen_types"`
	Affiliation   string         `json:"affiliation"`
	Arcs          []string       `json:"arcs"`
	Arms          []string       `json:"arms"`
	Description   string         `json:"description"`
	CardDesc      string         `json:"card_description"`
	Notes         string         `json:"notes"`
	Version       int            `json:"version"`
	ReviewStatus  string         `json:"review_status"`
	ReviewReason  string         `json:"review_reason"`
	AvatarImageID *int64         `json:"avatar_image_id"`
	CardImageID   *int64         `json:"card_image_id"`
	Owner         string         `json:"owner"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	ImageCount    int            `json:"image_count"`
	ImageCounts   map[string]int `json:"image_counts"` // per type, for the list's Pics column
	Images        []Image        `json:"images,omitempty"`
	Reviews       []Review       `json:"reviews,omitempty"`
	Requests      []Request      `json:"requests,omitempty"`
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
	(SELECT count(*) FROM hxh_char_image i WHERE i.char_id = c.id),
	COALESCE((SELECT json_object_agg(t.type, t.n) FROM (SELECT type, count(*) AS n FROM hxh_char_image i WHERE i.char_id = c.id GROUP BY type) t), '{}'::json)`

func scanChar(row pgx.Row) (*Char, error) {
	var c Char
	var nen, arcs, arms string
	var counts []byte
	if err := row.Scan(&c.ID, &c.CardNumber, &c.Name, &c.NameJA, &c.First, &c.Rank, &nen, &c.Affiliation, &arcs, &arms,
		&c.Description, &c.CardDesc, &c.Notes, &c.Version, &c.ReviewStatus, &c.ReviewReason, &c.AvatarImageID, &c.CardImageID, &c.Owner, &c.CreatedAt, &c.UpdatedAt,
		&c.ImageCount, &counts); err != nil {
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
	return c, rq.Err()
}

// bump counts a change to the character or its pictures.
func (s *Store) bump(ctx context.Context, charID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE hxh_char SET version = version + 1, updated_at = NOW() WHERE id = $1`, charID)
	return err
}

// Review records a verdict on the character's current version. A
// rejection may carry a reason (Andrew, 2026-09-19: optional).
func (s *Store) Review(id int64, status, reason, owner string) (*Char, error) {
	if !in(Verdicts, status) {
		return nil, fmt.Errorf("%w: status must be pending, accepted or rejected", ErrBadInput)
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
	err = tx.QueryRow(ctx, `UPDATE hxh_char SET review_status = $2, review_reason = $3 WHERE id = $1 RETURNING version`, id, status, reason).Scan(&version)
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
	// a reviewer's own verdict overrides whatever they had asked the bot for
	if _, err := tx.Exec(ctx, `UPDATE hxh_char_request SET status = 'withdrawn', resolved_by = $2, resolved_at = NOW()
		WHERE char_id = $1 AND status = 'open'`, id, owner); err != nil {
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
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Sort  int    `json:"sort"`
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
}

const requestCols = `q.id, q.char_id, q.kind, k.label, q.text, q.status, q.version, q.owner, q.created_at, q.resolved_by, q.resolved_at`
const requestFrom = ` FROM hxh_char_request q JOIN hxh_request_kind k ON k.slug = q.kind `

func scanRequest(row pgx.Row, q *Request) error {
	return row.Scan(&q.ID, &q.CharID, &q.Kind, &q.Label, &q.Text, &q.Status, &q.Version, &q.Owner, &q.CreatedAt, &q.ResolvedBy, &q.ResolvedAt)
}

func (s *Store) RequestKinds() ([]RequestKind, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT slug, label, sort FROM hxh_request_kind ORDER BY sort, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestKind{}
	for rows.Next() {
		var k RequestKind
		if err := rows.Scan(&k.Slug, &k.Label, &k.Sort); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// requestLogReason is what the review log shows for a request.
func requestLogReason(label, text string) string {
	if text == "" {
		return label
	}
	return label + ": " + text
}

// Request files an ask against the character's current version and
// marks the character "requested" until the bot resolves it.
func (s *Store) Request(charID int64, kind, text, owner string) (*Char, error) {
	kind, text = strings.TrimSpace(kind), strings.TrimSpace(text)
	ctx, cancel := withCtx()
	defer cancel()
	var label string
	err := s.pool.QueryRow(ctx, `SELECT label FROM hxh_request_kind WHERE slug = $1`, kind).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: unknown request kind %q", ErrBadInput, kind)
	}
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int
	err = tx.QueryRow(ctx, `UPDATE hxh_char SET review_status = 'requested', review_reason = '' WHERE id = $1 RETURNING version`, charID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_request (char_id, kind, text, version, owner) VALUES ($1, $2, $3, $4, $5)`,
		charID, kind, text, version, owner); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_review (char_id, version, status, reason, owner) VALUES ($1, $2, 'requested', $3, $4)`,
		charID, version, requestLogReason(label, text), owner); err != nil {
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
		if err := rows.Scan(&q.ID, &q.CharID, &q.Kind, &q.Label, &q.Text, &q.Status, &q.Version, &q.Owner, &q.CreatedAt, &q.ResolvedBy, &q.ResolvedAt, &q.CharName); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ResolveRequest marks an open request done; when it was the character's
// last open one, the character goes back to pending for the reviewer.
func (s *Store) ResolveRequest(id int64, by string) (*Request, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var charID int64
	err = tx.QueryRow(ctx, `UPDATE hxh_char_request SET status = 'done', resolved_by = $2, resolved_at = NOW()
		WHERE id = $1 AND status = 'open' RETURNING char_id`, id, by).Scan(&charID)
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
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM hxh_char_request WHERE char_id = $1 AND status = 'open'`, charID).Scan(&open); err != nil {
		return nil, err
	}
	if open == 0 {
		var version int
		err := tx.QueryRow(ctx, `UPDATE hxh_char SET review_status = 'pending' WHERE id = $1 AND review_status = 'requested' RETURNING version`, charID).Scan(&version)
		if err == nil {
			if _, err := tx.Exec(ctx, `INSERT INTO hxh_char_review (char_id, version, status, reason, owner) VALUES ($1, $2, 'pending', $3, $4)`,
				charID, version, fmt.Sprintf("request #%d done", id), by); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
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
	c.ReviewStatus = "pending"
	if err := validateCharValues(&c); err != nil {
		return nil, err
	}
	ctx, cancel := withCtx()
	defer cancel()
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO hxh_char
		(name, name_ja, first, rank, nen_types, affiliation, arcs, arms, description, card_description, notes, owner, card_number)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, (SELECT COALESCE(MAX(card_number), 0) + 1 FROM hxh_char)) RETURNING id`,
		c.Name, c.NameJA, c.First, c.Rank, joinSlugs(c.NenTypes), c.Affiliation,
		joinSlugs(c.Arcs), joinSlugs(c.Arms), c.Description, c.CardDesc, c.Notes, c.Owner).Scan(&id)
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
		return fmt.Errorf("%w: review_status must be pending, requested, accepted or rejected", ErrBadInput)
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
	if len(c.First) > 20 || len(c.NameJA) > 100 || len(c.Affiliation) > 60 || len(c.Name) > 100 {
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

func (s *Store) UpdateChar(id int64, patch map[string]json.RawMessage) (*Char, error) {
	c, err := s.GetChar(id)
	if err != nil {
		return nil, err
	}
	if err := applyPatch(c, patch); err != nil {
		return nil, err
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
		version = version + 1, updated_at=NOW() WHERE id=$1`,
		id, c.Name, c.NameJA, c.First, c.Rank, joinSlugs(c.NenTypes), c.Affiliation, joinSlugs(c.Arcs),
		joinSlugs(c.Arms), c.Description, c.CardDesc, c.Notes, c.AvatarImageID, c.CardImageID, c.CardNumber)
	if err != nil {
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
func (s *Store) AddImage(charID int64, typ string, sourceID *int64, sourceURL, caption, by string, data []byte) (im *Image, created bool, err error) {
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
	if err := s.bump(ctx, charID); err != nil {
		return nil, false, err
	}
	im, err = s.GetImage(id)
	return im, true, err
}

func (s *Store) UpdateImage(id int64, status, caption *string) (*Image, error) {
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
	if err := s.bump(ctx, charID); err != nil {
		return nil, err
	}
	return s.GetImage(id)
}

func (s *Store) DeleteImage(id int64) error {
	ctx, cancel := withCtx()
	defer cancel()
	var charID int64
	err := s.pool.QueryRow(ctx, `DELETE FROM hxh_char_image WHERE id = $1 RETURNING char_id`, id).Scan(&charID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.bump(ctx, charID)
}

// CropImage cuts rect out of image id at native resolution and stores the
// result as a "cropped" image of the same character.
func (s *Store) CropImage(id int64, rect CropRect, by string) (*Image, bool, error) {
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
	return s.AddImage(src.CharID, "cropped", &id, "", src.Caption, by, out)
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
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser, username)))
		})
	}
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
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrBadInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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

func handleBinder(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chars, err := store.ListChars("accepted", "")
		if err != nil {
			fail(w, err, "binder")
			return
		}
		out := make([]BinderCard, 0, len(chars))
		for _, c := range chars {
			if c.AvatarImageID == nil || c.CardImageID == nil { // a card needs both pictures (Andrew, 2026-09-21)
				continue
			}
			out = append(out, BinderCard{ID: c.ID, CardNumber: c.CardNumber, Name: c.Name, First: c.First, Rank: c.Rank, NenTypes: c.NenTypes, Affiliation: c.Affiliation,
				Arcs: c.Arcs, Arms: c.Arms, CardDesc: c.CardDesc, Description: c.Description, AvatarImageID: c.AvatarImageID, CardImageID: c.CardImageID, Version: c.Version})
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
			Kind string `json:"kind"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			fail(w, fmt.Errorf("%w: bad json", ErrBadInput), "request")
			return
		}
		c, err := store.Request(id, body.Kind, body.Text, userOf(r))
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
		q, err := store.ResolveRequest(id, userOf(r))
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
		c, err := store.UpdateChar(id, patch)
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
		c, err := store.Review(id, body.Status, body.Reason, userOf(r))
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
		im, created, err := store.AddImage(charID, typ, sourceID, get("source_url"), get("caption"), userOf(r), data)
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
		im, err := store.UpdateImage(id, body.Status, body.Caption)
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
		if err := store.DeleteImage(id); err != nil {
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
		im, created, err := store.CropImage(id, rect, userOf(r))
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
