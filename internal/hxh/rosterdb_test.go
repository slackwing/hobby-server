package hxh

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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
