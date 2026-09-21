package hxh

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int, alpha bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha && x < w/2 {
				a = 0
			}
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 128, a})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodeUploadDerivesRow(t *testing.T) {
	d, err := decodeUpload(jpegBytes(t, 1280, 720))
	if err != nil {
		t.Fatal(err)
	}
	if d.mime != "image/jpeg" || d.width != 1280 || d.height != 720 || len(d.sha) != 64 {
		t.Fatalf("got %+v", d)
	}
	if d.thumbMime != "image/jpeg" {
		t.Fatalf("opaque picture should get a jpeg thumb, got %s", d.thumbMime)
	}
	timg, _, err := image.Decode(bytes.NewReader(d.thumb))
	if err != nil {
		t.Fatal(err)
	}
	if timg.Bounds().Dx() != thumbMaxSide || timg.Bounds().Dy() != 180 {
		t.Fatalf("thumb should fit %d on the long side, got %v", thumbMaxSide, timg.Bounds())
	}
}

func TestDecodeUploadKeepsAlphaInThumb(t *testing.T) {
	d, err := decodeUpload(pngBytes(t, 400, 800, true))
	if err != nil {
		t.Fatal(err)
	}
	if d.mime != "image/png" || d.thumbMime != "image/png" {
		t.Fatalf("transparent png should keep a png thumb, got %s / %s", d.mime, d.thumbMime)
	}
	timg, _, err := image.Decode(bytes.NewReader(d.thumb))
	if err != nil {
		t.Fatal(err)
	}
	if timg.Bounds().Dx() != 160 || timg.Bounds().Dy() != thumbMaxSide {
		t.Fatalf("portrait thumb wrong: %v", timg.Bounds())
	}
	if _, _, _, a := timg.At(2, 2).RGBA(); a != 0 {
		t.Fatalf("left half should stay transparent, alpha=%d", a)
	}
}

func TestDecodeUploadNeverUpscalesThumb(t *testing.T) {
	d, err := decodeUpload(pngBytes(t, 64, 64, false))
	if err != nil {
		t.Fatal(err)
	}
	timg, _, _ := image.Decode(bytes.NewReader(d.thumb))
	if timg.Bounds().Dx() != 64 {
		t.Fatalf("small pictures keep their size as thumbs, got %v", timg.Bounds())
	}
}

func TestDecodeUploadRejectsGarbage(t *testing.T) {
	for _, in := range [][]byte{nil, []byte("hello"), []byte("<html>")} {
		if _, err := decodeUpload(in); err == nil {
			t.Fatalf("%q should not decode", in)
		}
	}
}

func TestCropPNGCutsExactPixels(t *testing.T) {
	src := pngBytes(t, 100, 50, false)
	img, _, _ := image.Decode(bytes.NewReader(src))
	out, err := cropPNG(img, CropRect{X: 10, Y: 20, W: 30, H: 5})
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if c.Bounds().Dx() != 30 || c.Bounds().Dy() != 5 {
		t.Fatalf("crop size %v", c.Bounds())
	}
	// pixel (0,0) of the crop is pixel (10,20) of the source: R=x, G=y
	r, g, _, _ := c.At(c.Bounds().Min.X, c.Bounds().Min.Y).RGBA()
	if r>>8 != 10 || g>>8 != 20 {
		t.Fatalf("crop origin pixel = (%d,%d), want (10,20)", r>>8, g>>8)
	}
	for _, bad := range []CropRect{{X: -1, Y: 0, W: 10, H: 10}, {X: 95, Y: 0, W: 10, H: 10}, {X: 0, Y: 0, W: 0, H: 10}, {X: 0, Y: 45, W: 10, H: 10}} {
		if _, err := cropPNG(img, bad); err == nil {
			t.Fatalf("crop %+v should fail", bad)
		}
	}
}

func TestValidSlug(t *testing.T) {
	for _, ok := range []string{"gon", "biscuit-krueger", "zetsk-bellam", "a1"} {
		if !validSlug(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "Gon", "gon ", "-gon", "gon-", "gon--freecss", "ゴン", "gon_freecss"} {
		if validSlug(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestApplyPatch(t *testing.T) {
	base := func() *Char {
		return &Char{Name: "Gon Freecss", Rank: "S", ReviewStatus: "pending", NenTypes: []string{"enhancement"}}
	}
	patch := func(s string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	c := base()
	if err := applyPatch(c, patch(`{"name_ja":"ゴン＝フリークス","arcs":["hunter-exam","greed-island"],"avatar_image_id":7,"card_image_id":null}`)); err != nil {
		t.Fatal(err)
	}
	if c.NameJA != "ゴン＝フリークス" || len(c.Arcs) != 2 || c.AvatarImageID == nil || *c.AvatarImageID != 7 || c.CardImageID != nil {
		t.Fatalf("patch not applied: %+v", c)
	}
	for _, bad := range []string{
		`{"rank":"X"}`, `{"review_status":"accepted"}`, `{"status":"accepted"}`, `{"version":9}`, `{"nen_types":["enhancement","emission","conjuration"]}`,
		`{"nen_types":["fire"]}`, `{"arcs":["dark-continent"]}`, `{"arms":["Fishing Rod"]}`, `{"slug":"gon"}`,
		`{"name":"  "}`, `{"glyph":"🎣"}`, `{"avatar_image_id":"seven"}`, `{"arcs":"hunter-exam"}`,
	} {
		if err := applyPatch(base(), patch(bad)); err == nil {
			t.Errorf("patch %s should be rejected", bad)
		}
	}
	// a list set to null becomes empty, not nil
	c = base()
	if err := applyPatch(c, patch(`{"nen_types":null}`)); err != nil || c.NenTypes == nil || len(c.NenTypes) != 0 {
		t.Fatalf("null list: err=%v types=%#v", err, c.NenTypes)
	}
}

// A 64×64 lossy WebP: left half black, right half white (Pillow, q90).
const bwWebP = "UklGRj4AAABXRUJQVlA4IDIAAACQAwCdASpAAEAAPjEWiUMiISEVBAAgAwS0gAAmimEoVOTMSCFAAP7+fLK/y6gAAAAAAA=="

// Lossy WebP is limited-range YCbCr; Go's default conversion would
// read the black half as (16,16,16) and the white as (235,235,235).
func TestDecodeUploadReadsWebPAtFullRange(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString(bwWebP)
	if err != nil {
		t.Fatal(err)
	}
	d, err := decodeUpload(data)
	if err != nil {
		t.Fatal(err)
	}
	if d.mime != "image/webp" || d.width != 64 || d.height != 64 {
		t.Fatalf("got %+v", d)
	}
	r, g, b, _ := d.img.At(8, 32).RGBA()
	if r>>8 > 2 || g>>8 > 2 || b>>8 > 2 {
		t.Fatalf("black half decoded as (%d,%d,%d), want ~0", r>>8, g>>8, b>>8)
	}
	r, g, b, _ = d.img.At(56, 32).RGBA()
	if r>>8 < 253 || g>>8 < 253 || b>>8 < 253 {
		t.Fatalf("white half decoded as (%d,%d,%d), want ~255", r>>8, g>>8, b>>8)
	}
}

// A bot never passes a verdict; "requested" is no longer a state.
func TestReviewRefusesBotsAndUnknownStates(t *testing.T) {
	if _, err := (&Store{}).Review(1, "accepted", "", "claude", true); !errors.Is(err, ErrBadInput) {
		t.Fatalf("bot verdict: want ErrBadInput, got %v", err)
	}
	if _, err := (&Store{}).Review(1, "requested", "", "abi", false); !errors.Is(err, ErrBadInput) {
		t.Fatalf("requested: want ErrBadInput, got %v", err)
	}
	if in(CharStatus, "requested") {
		t.Fatal("requested must not be a character status")
	}
}

func TestMoveOrder(t *testing.T) {
	seq := func(ns ...int) []numbered {
		out := []numbered{}
		for i, n := range ns {
			out = append(out, numbered{ID: int64(i + 1), N: n})
		}
		return out
	}
	check := func(name string, got map[int64]int, err error, want map[int64]int) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}
	got, err := moveOrder(seq(1, 2, 3, 4, 5), 5, 2)
	check("up: 5 after 2", got, err, map[int64]int{5: 3, 3: 4, 4: 5})
	got, err = moveOrder(seq(1, 2, 3, 4, 5), 4, 0)
	check("to the front", got, err, map[int64]int{4: 1, 1: 2, 2: 3, 3: 4})
	got, err = moveOrder(seq(1, 2, 3, 4, 5), 1, 3)
	check("down: 1 after 3", got, err, map[int64]int{2: 1, 3: 2, 1: 3})
	got, err = moveOrder(seq(1, 2, 3, 4, 5), 3, 2)
	check("already there", got, err, map[int64]int{})
	got, err = moveOrder(seq(1, 2, 3, 4, 5), 3, 3)
	check("after itself", got, err, map[int64]int{})
	// gaps and duplicates are kept: numbers 1,4,4,9 — 4 (No. 9) after 1 hands 4,4,9 out again
	got, err = moveOrder(seq(1, 4, 4, 9), 4, 1)
	check("gaps and duplicates", got, err, map[int64]int{4: 4, 3: 9})
	if _, err := moveOrder(seq(1, 2), 9, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown character: %v", err)
	}
	if _, err := moveOrder(seq(1, 2), 1, 9); !errors.Is(err, ErrBadInput) {
		t.Fatalf("unknown anchor: %v", err)
	}
}

func TestDiffCharListsOnlyWhatChanged(t *testing.T) {
	seven := int64(7)
	before := &Char{Name: "Gon", NenTypes: []string{"enhancement"}, Arcs: []string{"hunter-exam"}, Arms: []string{}, CardNumber: 1}
	after := *before
	after.Name = "Gon Freecss"
	after.NenTypes = []string{"enhancement", "emission"}
	after.AvatarImageID = &seven
	after.CardNumber = 3
	rows := diffChar(before, &after)
	got := map[string][2]string{}
	for _, r := range rows {
		if r.Kind != "field" || r.Action != "set" {
			t.Fatalf("bad row %+v", r)
		}
		got[r.Field] = [2]string{r.Old, r.New}
	}
	want := map[string][2]string{"name": {"Gon", "Gon Freecss"}, "nen_types": {"enhancement", "enhancement, emission"}, "avatar_image_id": {"", "7"}, "card_number": {"1", "3"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %v, want %v", k, got[k], v)
		}
	}
	if n := len(diffChar(before, before)); n != 0 {
		t.Fatalf("no change should diff to nothing, got %d rows", n)
	}
}

func TestSummarize(t *testing.T) {
	id := int64(1)
	rows := []Change{{Kind: "field", Field: "description"}, {Kind: "field", Field: "notes"}, {Kind: "image", Action: "added", ImageID: &id}, {Kind: "image", Action: "added", ImageID: &id}, {Kind: "image", Action: "removed", ImageID: &id}}
	if got := summarize(rows); got != "changed description, notes; added 2 pictures; removed a picture" {
		t.Fatalf("got %q", got)
	}
}

// The snapshot JSON must unmarshal into BinderCard by key.
func TestSnapshotKeysMatchBinderCard(t *testing.T) {
	snap := `{"name":"Gon","first":"Gon","rank":"S","nen_types":["enhancement"],"affiliation":"Hunter","arcs":["hunter-exam"],"arms":[],"card_description":"A boy.","description":"A boy from Whale Island.","avatar_image_id":12,"card_image_id":14}`
	var card BinderCard
	if err := json.Unmarshal([]byte(snap), &card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "Gon" || card.CardDesc != "A boy." || card.AvatarImageID == nil || *card.CardImageID != 14 || len(card.NenTypes) != 1 {
		t.Fatalf("got %+v", card)
	}
	for _, key := range []string{"'name'", "'first'", "'rank'", "'nen_types'", "'affiliation'", "'arcs'", "'arms'", "'card_description'", "'description'", "'avatar_image_id'", "'card_image_id'"} {
		if !strings.Contains(snapshotExpr, key) {
			t.Fatalf("snapshotExpr lacks %s", key)
		}
	}
}
