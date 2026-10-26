package sxiva

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSquaresFromMinutes(t *testing.T) {
	cases := map[int64]int{0: 0, 4: 0, 5: 1, 6: 1, 14: 1, 15: 2, 100: 10, -3: 0}
	for m, want := range cases {
		if got := squaresFromMinutes(m); got != want {
			t.Errorf("%d minutes → %d squares, want %d", m, got, want)
		}
	}
}

func TestHealthSquares(t *testing.T) {
	cases := []struct {
		exe, alc *float64
		want     int
		why      string
	}{
		{nil, nil, 0, "nothing logged"},
		{f64(1.5), nil, 3, "every 0.5 [exe] is a square; blank [alc] earns nothing"},
		{f64(0), f64(0), 2, "no drinks → 2"},
		{nil, f64(0.5), 1, "half a drink rounds to one → 1"},
		{nil, f64(1), 1, "one drink → 1"},
		{nil, f64(1.5), 0, "1.5 rounds to 2 → 0"},
		{nil, f64(3.5), 0, "never below zero"},
		{f64(1), f64(0), 4, "both parts add"},
		{f64(0.4), nil, 0, "less than half an [exe] earns nothing"},
	}
	for _, c := range cases {
		if got := healthSquares(Day{Exe: c.exe, Alc: c.alc}); got != c.want {
			t.Errorf("%s: got %d, want %d", c.why, got, c.want)
		}
	}
}

func TestSabbatical(t *testing.T) {
	days := []Day{
		{
			Date: day("2026-10-01"),
			CategoryMinutes: map[string]int64{
				"nflx": 25, "crco": 5, "staff": 4, "crit": 60, "rk": 15,
				"abi": 3, "hxh": 3, "art": 120, // abi + hxh summed first: 6 → 1
			},
			Exe: f64(1),
			Alc: f64(0),
		},
		{Date: day("2026-10-02"), CategoryMinutes: map[string]int64{"rv": 9, "abi": 1}}, // 10 → 1
		{Date: day("2026-10-03"), CategoryMinutes: map[string]int64{"art": 300}},        // nothing → left out
	}
	writing := map[string]int{"2026-10-01": 13, "2026-10-05": 4}
	got := Sabbatical(days, writing)
	want := []SabbaticalDay{
		{Date: "2026-10-01", Writing: 10, Netflix: 3, CrucialConversations: 1, StaffEngineer: 0,
			Abi: 1, Health: 4, Editing: 2, CriticalReading: 6},
		{Date: "2026-10-02", Abi: 1},
		{Date: "2026-10-05", Writing: 4}, // writing on a day with no minutes file
	}
	if len(got) != len(want) {
		t.Fatalf("got %d days %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("day %d:\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}

	if out := Sabbatical(days, nil); len(out) != 2 || out[0].Writing != 0 {
		t.Errorf("without writing: %+v", out)
	}
	if out := Sabbatical(nil, nil); out == nil || len(out) != 0 {
		t.Errorf("no data must be [] not null: %#v", out)
	}
}

func TestWritingSource(t *testing.T) {
	if NewWritingSource("", "mst_x") != nil || NewWritingSource("http://x", "") != nil {
		t.Fatal("an unconfigured link must give a nil source")
	}
	var calls int32
	var status int32 = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/manuscripts/api/v1/points-daily" || r.Header.Get("Authorization") != "Bearer mst_secret" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if s := atomic.LoadInt32(&status); s != http.StatusOK {
			http.Error(w, "nope", int(s))
			return
		}
		_, _ = w.Write([]byte(`{"today":"2026-10-26","days":[{"date":"2026-10-25","points":13,"daily":12},{"date":"2026-10-26","points":2,"daily":0}]}`))
	}))
	defer srv.Close()

	src := NewWritingSource(srv.URL+"/manuscripts/", "mst_secret")
	points, err := src.Points(context.Background())
	if err != nil || points["2026-10-25"] != 13 || points["2026-10-26"] != 2 || len(points) != 2 {
		t.Fatalf("points %v, err %v", points, err)
	}
	if _, err := src.Points(context.Background()); err != nil || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("second call within a minute must come from the cache (calls=%d)", calls)
	}

	src.ttl = 0 // expire the cache
	atomic.StoreInt32(&status, http.StatusInternalServerError)
	if _, err := src.Points(context.Background()); err == nil || strings.Contains(err.Error(), "mst_secret") {
		t.Fatalf("a failing upstream must error without the token: %v", err)
	}
	bad := NewWritingSource(srv.URL+"/manuscripts", "mst_wrong")
	if _, err := bad.Points(context.Background()); err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "mst_wrong") {
		t.Fatalf("a rejected token must error (status, no token): %v", err)
	}
}
