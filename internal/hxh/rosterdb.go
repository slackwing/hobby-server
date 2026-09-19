// Roster DB — the curated character base (schema: liquibase/hxh/
// changelog/006-roster-db.xml). Characters are inserted as "pending" one
// at a time by the hxh-character skill (feathers .claude/skills/) and
// reviewed by hand in the admin page /hxh/roster/. Every picture is a
// row in hxh_char_image with a GLOBAL id, a type (raw, cropped,
// pixelated, upscaled, transparent) and the image it was made from.
//
// Public URLs (Apache maps /hxh/api/* → /api/hxh/*):
//
//	GET    /hxh/api/db/chars?status=&slug=      list, no blobs
//	POST   /hxh/api/db/chars                    create (409 on a taken slug)
//	GET    /hxh/api/db/chars/{id}               profile + image metadata
//	PATCH  /hxh/api/db/chars/{id}               partial update
//	DELETE /hxh/api/db/chars/{id}
//	POST   /hxh/api/db/chars/{id}/images        upload (multipart "file" or raw body)
//	GET    /hxh/api/db/images/{id}              the picture (any hxh role)
//	GET    /hxh/api/db/images/{id}/thumb        its preview (any hxh role)
//	GET    /hxh/api/db/images/{id}/meta         its row (what the crop page shows)
//	PATCH  /hxh/api/db/images/{id}              status / caption
//	DELETE /hxh/api/db/images/{id}
//	POST   /hxh/api/db/images/{id}/crop         {x,y,w,h} → a new "cropped" image
//
// Writes need role admin on hxh; reads of pictures need any hxh role
// (the guest-facing Binder will show avatars one day).
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
	ImageTypes = []string{"raw", "cropped", "pixelated", "upscaled", "transparent"}
	NenTypes   = []string{"enhancement", "transmutation", "conjuration", "emission", "manipulation", "specialization"}
	ArcSlugs   = []string{"hunter-exam", "zoldyck-family", "heavens-arena", "yorknew-city", "greed-island", "chimera-ant", "chairman-election"}
	Ranks      = []string{"S", "A", "B", "C"}
	CharStatus = []string{"pending", "approved", "rejected"}
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
	ID            int64     `json:"id"`
	Slug          string    `json:"slug"`
	Name          string    `json:"name"`
	NameJA        string    `json:"name_ja"`
	First         string    `json:"first"`
	Rank          string    `json:"rank"`
	NenTypes      []string  `json:"nen_types"`
	Affiliation   string    `json:"affiliation"`
	Arcs          []string  `json:"arcs"`
	Arms          []string  `json:"arms"`
	Description   string    `json:"description"`
	Notes         string    `json:"notes"`
	Status        string    `json:"status"`
	AvatarImageID *int64    `json:"avatar_image_id"`
	CardImageID   *int64    `json:"card_image_id"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	ImageCount    int       `json:"image_count"`
	Images        []Image   `json:"images,omitempty"`
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
	CreatedBy     string    `json:"created_by"`
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

// decodeUpload validates bytes as a picture and derives everything the
// row needs. GIFs keep only their first frame.
func decodeUpload(data []byte) (*decoded, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrBadInput)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
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

const charCols = `c.id, c.slug, c.name, c.name_ja, c.first, c.rank, c.nen_types, c.affiliation, c.arcs, c.arms,
	c.description, c.notes, c.status, c.avatar_image_id, c.card_image_id, c.created_by, c.created_at, c.updated_at,
	(SELECT count(*) FROM hxh_char_image i WHERE i.char_id = c.id AND i.status = 'kept')`

func scanChar(row pgx.Row) (*Char, error) {
	var c Char
	var nen, arcs, arms string
	if err := row.Scan(&c.ID, &c.Slug, &c.Name, &c.NameJA, &c.First, &c.Rank, &nen, &c.Affiliation, &arcs, &arms,
		&c.Description, &c.Notes, &c.Status, &c.AvatarImageID, &c.CardImageID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
		&c.ImageCount); err != nil {
		return nil, err
	}
	c.NenTypes, c.Arcs, c.Arms = splitSlugs(nen), splitSlugs(arcs), splitSlugs(arms)
	return &c, nil
}

func (s *Store) ListChars(status, slug string) ([]Char, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT `+charCols+` FROM hxh_char c
		WHERE ($1 = '' OR c.status = $1) AND ($2 = '' OR c.slug = $2) ORDER BY c.id`, status, slug)
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
	return c, rows.Err()
}

func (s *Store) CreateChar(c Char) (*Char, error) {
	if !validSlug(c.Slug) {
		return nil, fmt.Errorf("%w: slug must be lowercase kebab-case", ErrBadInput)
	}
	if strings.TrimSpace(c.Name) == "" {
		return nil, fmt.Errorf("%w: name required", ErrBadInput)
	}
	if c.Rank == "" {
		c.Rank = "C"
	}
	if c.Status == "" {
		c.Status = "pending"
	}
	if err := validateCharValues(&c); err != nil {
		return nil, err
	}
	ctx, cancel := withCtx()
	defer cancel()
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO hxh_char
		(slug, name, name_ja, first, rank, nen_types, affiliation, arcs, arms, description, notes, status, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		c.Slug, strings.TrimSpace(c.Name), c.NameJA, c.First, c.Rank, joinSlugs(c.NenTypes), c.Affiliation,
		joinSlugs(c.Arcs), joinSlugs(c.Arms), c.Description, c.Notes, c.Status, c.CreatedBy).Scan(&id)
	if isUnique(err) {
		return nil, fmt.Errorf("%w: slug %q is taken", ErrConflict, c.Slug)
	}
	if err != nil {
		return nil, err
	}
	return s.GetChar(id)
}

func validateCharValues(c *Char) error {
	if !in(Ranks, c.Rank) {
		return fmt.Errorf("%w: rank must be S, A, B or C", ErrBadInput)
	}
	if !in(CharStatus, c.Status) {
		return fmt.Errorf("%w: status must be pending, approved or rejected", ErrBadInput)
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
var patchKeys = []string{"slug", "name", "name_ja", "first", "rank", "nen_types", "affiliation", "arcs", "arms",
	"description", "notes", "status", "avatar_image_id", "card_image_id"}

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
	for k, dst := range map[string]*string{"slug": &c.Slug, "name": &c.Name, "name_ja": &c.NameJA, "first": &c.First,
		"rank": &c.Rank, "affiliation": &c.Affiliation, "description": &c.Description, "notes": &c.Notes, "status": &c.Status} {
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
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return fmt.Errorf("%w: name required", ErrBadInput)
	}
	if !validSlug(c.Slug) {
		return fmt.Errorf("%w: slug must be lowercase kebab-case", ErrBadInput)
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
	_, err = s.pool.Exec(ctx, `UPDATE hxh_char SET slug=$2, name=$3, name_ja=$4, first=$5, rank=$6, nen_types=$7,
		affiliation=$8, arcs=$9, arms=$10, description=$11, notes=$12, status=$13, avatar_image_id=$14, card_image_id=$15,
		updated_at=NOW() WHERE id=$1`,
		id, c.Slug, c.Name, c.NameJA, c.First, c.Rank, joinSlugs(c.NenTypes), c.Affiliation, joinSlugs(c.Arcs),
		joinSlugs(c.Arms), c.Description, c.Notes, c.Status, c.AvatarImageID, c.CardImageID)
	if isUnique(err) {
		return nil, fmt.Errorf("%w: slug %q is taken", ErrConflict, c.Slug)
	}
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
	i.source_url, i.caption, i.status, i.created_by, i.created_at`

func scanImage(row pgx.Row) (*Image, error) {
	var im Image
	if err := row.Scan(&im.ID, &im.CharID, &im.Type, &im.SourceImageID, &im.Mime, &im.Width, &im.Height, &im.Bytes,
		&im.SHA256, &im.SourceURL, &im.Caption, &im.Status, &im.CreatedBy, &im.CreatedAt); err != nil {
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
		(char_id, type, source_image_id, mime, width, height, data, thumb, thumb_mime, sha256, source_url, caption, created_by)
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
	tag, err := s.pool.Exec(ctx, `UPDATE hxh_char_image SET status = COALESCE($2, status), caption = COALESCE($3, caption) WHERE id = $1`, id, status, caption)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return s.GetImage(id)
}

func (s *Store) DeleteImage(id int64) error {
	ctx, cancel := withCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `DELETE FROM hxh_char_image WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
	img, _, err := image.Decode(bytes.NewReader(data))
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
// role admin for hxh; otherwise any hxh role will do. The username lands
// in the request context (ctxUser / userOf, shared with chat.go).
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
			var allowed bool
			if admin {
				allowed, err = auth.HasRole(username, "hxh", "admin")
			} else {
				allowed, err = auth.IsMember(username, "hxh")
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
			m.Get("/images/{id}", handleImageData(store, false))
			m.Get("/images/{id}/thumb", handleImageData(store, true))
		})
		g.Group(func(a chi.Router) {
			a.Use(requireHxh(auth, true))
			a.Get("/chars", handleListChars(store))
			a.Post("/chars", handleCreateChar(store))
			a.Get("/chars/{id}", handleGetChar(store))
			a.Patch("/chars/{id}", handlePatchChar(store))
			a.Delete("/chars/{id}", handleDeleteChar(store))
			a.Post("/chars/{id}/images", handleUpload(store))
			a.Get("/images/{id}/meta", handleImageMeta(store))
			a.Patch("/images/{id}", handlePatchImage(store))
			a.Delete("/images/{id}", handleDeleteImage(store))
			a.Post("/images/{id}/crop", handleCrop(store))
		})
	})
}

func handleListChars(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		chars, err := store.ListChars(r.URL.Query().Get("status"), r.URL.Query().Get("slug"))
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
		c.CreatedBy = userOf(r)
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
		c.Images = nil
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
