package hxh

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNewBugClean(t *testing.T) {
	if _, err := (NewBug{Body: "   "}).clean(); !errors.Is(err, ErrBadInput) {
		t.Fatalf("no text and no picture: want ErrBadInput, got %v", err)
	}
	b, err := NewBug{Body: "  the binder shrank  ", Context: json.RawMessage(`{"url":"/hxh/"}`)}.clean()
	if err != nil || b.Body != "the binder shrank" || string(b.Context) != `{"url":"/hxh/"}` {
		t.Fatalf("trimmed text, context kept: %+v %v", b, err)
	}
	if b, err := (NewBug{ImageID: 7}).clean(); err != nil || b.Body != "" {
		t.Fatalf("a picture alone is a report: %+v %v", b, err)
	}
	if _, err := (NewBug{Body: strings.Repeat("x", MaxBugBody+1)}).clean(); !errors.Is(err, ErrBadInput) {
		t.Fatalf("too long: want ErrBadInput, got %v", err)
	}
	for _, bad := range []string{``, `[1,2]`, `"text"`, `{broken`, `{"a":"` + strings.Repeat("x", MaxBugCtx) + `"}`} {
		b, err := NewBug{Body: "x", Context: json.RawMessage(bad)}.clean()
		if err != nil || string(b.Context) != `{}` {
			t.Fatalf("context %.20q: want {} , got %s %v", bad, b.Context, err)
		}
	}
}

func TestBugStatuses(t *testing.T) {
	for s, want := range map[string]bool{"pending": true, "resolved": true, "open": false, "": false, "all": false} {
		if validStatus(s) != want {
			t.Fatalf("validStatus(%q) = %v", s, !want)
		}
	}
}
