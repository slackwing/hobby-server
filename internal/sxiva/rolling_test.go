package sxiva

import (
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func day(date string) time.Time {
	t, err := time.Parse(dateLayout, date)
	if err != nil {
		panic(err)
	}
	return t
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

// categoryFixture: Thu 2026-10-01 … Mon 2026-10-12, Wed 10-07 missing.
// art = hobby, sp = work, fam = other; meet on three days.
//
//	idx date   dow  art  sp   fam  meet  → work
//	0   10-01  Thu  10   100  1          100
//	1   10-02  Fri  20   200  2    5     205
//	2   10-03  Sat  30        3          0
//	3   10-04  Sun  40        4          0
//	4   10-05  Mon  50   300  5          300
//	5   10-06  Tue  60   400  6    10    410
//	6   10-08  Thu  70   500  7          500
//	7   10-09  Fri  80   600  8          600
//	8   10-10  Sat  90        9          0
//	9   10-11  Sun  100       10         0
//	10  10-12  Mon  110  700  11   20    720
func categoryFixture() []Day {
	type r struct {
		date         string
		art, sp, fam int64
		meet         *int64
	}
	rows := []r{
		{"2026-10-01", 10, 100, 1, nil},
		{"2026-10-02", 20, 200, 2, i64(5)},
		{"2026-10-03", 30, 0, 3, nil},
		{"2026-10-04", 40, 0, 4, nil},
		{"2026-10-05", 50, 300, 5, nil},
		{"2026-10-06", 60, 400, 6, i64(10)},
		{"2026-10-08", 70, 500, 7, nil},
		{"2026-10-09", 80, 600, 8, nil},
		{"2026-10-10", 90, 0, 9, nil},
		{"2026-10-11", 100, 0, 10, nil},
		{"2026-10-12", 110, 700, 11, i64(20)},
	}
	var days []Day
	for _, x := range rows {
		cats := map[string]int64{"art": x.art, "fam": x.fam}
		if x.sp != 0 {
			cats["sp"] = x.sp
		}
		days = append(days, Day{Date: day(x.date), CategoryMinutes: cats, Meet: x.meet})
	}
	return days
}

func TestFixtureWeekdays(t *testing.T) {
	if wd := day("2026-10-01").Weekday(); wd != time.Thursday {
		t.Fatalf("2026-10-01 is %v, the fixture assumes Thursday", wd)
	}
}

func TestRollingCategoryUnweighted(t *testing.T) {
	// λ = 0: every weight is 1, so weighted = mean × window — isolates
	// which rows each window takes.
	got := RollingCategory(categoryFixture(), []string{"art"}, []string{"sp"}, 3, 0)

	// Row windows count ROWS: on Thu 10-08 the 3 rows are Mon, Tue, Thu
	// (Wed is missing) …
	if r := got[6]; r.HobbyRaw != 50+60+70 {
		t.Errorf("10-08 hobby_raw = %d, want 180 (3 rows, not 3 days)", r.HobbyRaw)
	}
	// … but hobby_weighted's window is 3 CALENDAR days: Tue + Thu only.
	if r := got[6]; r.HobbyWeighted != (60+70)/2.0*3 {
		t.Errorf("10-08 hobby_weighted = %v, want 195 (calendar window)", r.HobbyWeighted)
	}
	// other excludes meet; total includes it.
	if r := got[6]; r.OtherRaw != 5+6+7 || r.TotalRaw != (50+300+5)+(60+400+6+10)+(70+500+7) {
		t.Errorf("10-08 other/total = %d/%d, want 18/1408", r.OtherRaw, r.TotalRaw)
	}
	// work = the last 5 weekday rows (sp + meet), whatever the window.
	wantWork := map[int]int64{
		0:  100,                         // Thu: only itself
		2:  100 + 205,                   // Sat: Thu, Fri
		6:  100 + 205 + 300 + 410 + 500, // Thu 10-08: all five weekdays so far
		8:  205 + 300 + 410 + 500 + 600, // Sat 10-10: Thu 10-01 has dropped out
		10: 300 + 410 + 500 + 600 + 720, // Mon 10-12
	}
	for i, want := range wantWork {
		if got[i].WorkRaw != want {
			t.Errorf("%s work_raw = %d, want %d", got[i].Date, got[i].WorkRaw, want)
		}
	}
	// Fewer than 5 weekdays: Σ · 5/n.
	if r := got[2]; r.WorkWeighted != 762.5 {
		t.Errorf("10-03 work_weighted = %v, want 762.5", r.WorkWeighted)
	}
	if r := got[10]; r.WorkWeighted != 2530 {
		t.Errorf("10-12 work_weighted = %v, want 2530", r.WorkWeighted)
	}
}

func TestRollingCategoryWeighted(t *testing.T) {
	// λ = ln 2: weights halve per day of age.
	got := RollingCategory(categoryFixture(), []string{"art"}, []string{"sp"}, 3, math.Ln2)

	// Thu 10-08: Tue (age 2, w ¼) and Thu (w 1): (15 + 70) · 3/1.25 = 204.
	if r := got[6]; r.HobbyWeighted != 204 {
		t.Errorf("10-08 hobby_weighted = %v, want 204", r.HobbyWeighted)
	}
	// Sat 10-10: weekdays Fri 10-02 … Fri 10-09, ages 8 5 4 2 1:
	// Σwork·w = 460.80078125, Σw = 217/256 → ·5/Σw = 2718.0876 → 2718.1.
	if r := got[8]; r.WorkWeighted != 2718.1 {
		t.Errorf("10-10 work_weighted = %v, want 2718.1", r.WorkWeighted)
	}
}

func TestRollingCategoryNoWeekdayYet(t *testing.T) {
	days := []Day{{Date: day("2026-10-03"), CategoryMinutes: map[string]int64{"sp": 50}}} // a Saturday
	got := RollingCategory(days, nil, []string{"sp"}, 7, 0.5)
	if got[0].WorkRaw != 0 || got[0].WorkWeighted != 0 {
		t.Errorf("weekend-only work = %d/%v, want 0/0", got[0].WorkRaw, got[0].WorkWeighted)
	}
}

func TestCategories(t *testing.T) {
	days := []Day{
		{Date: day("2026-10-01"), CategoryMinutes: map[string]int64{"art": 1, "sp": 1, "...": 1}},
		{Date: day("2026-10-02"), CategoryMinutes: map[string]int64{"fam": 1, "art": 2}},
	}
	h, w, o := Categories(days, []string{"art", "ms"}, []string{"sp"})
	if !reflect.DeepEqual(h, []string{"art"}) || !reflect.DeepEqual(w, []string{"sp"}) ||
		!reflect.DeepEqual(o, []string{"...", "fam"}) {
		t.Errorf("got hobby=%v work=%v other=%v", h, w, o)
	}
	_, w, _ = Categories(days, []string{"art"}, nil)
	if w == nil || len(w) != 0 {
		t.Errorf("empty list must be [] not null, got %#v", w)
	}
}

// alcoholFixture: 9 consecutive rows.
//
//	idx  alc   dep    sleep
//	0    1.0   -      -
//	1    -     1.0    80
//	2    2.5   -      -
//	3    0.5   -0.5   70
//	4    -     -      -
//	5    1.0   -      -
//	6    3.0   -      -
//	7    2.0   -      -
//	8    0.1   -      90
func alcoholFixture() []Day {
	alc := []*float64{f64(1), nil, f64(2.5), f64(0.5), nil, f64(1), f64(3), f64(2), f64(0.1)}
	dep := map[int]float64{1: 1.0, 3: -0.5}
	sleep := map[int]int64{1: 80, 3: 70, 8: 90}
	start := day("2026-10-01")
	days := make([]Day, len(alc))
	for i := range days {
		days[i] = Day{Date: start.AddDate(0, 0, i), Alc: alc[i]}
		if v, ok := dep[i]; ok {
			days[i].DepAvg = f64(v)
		}
		if v, ok := sleep[i]; ok {
			days[i].SleepScore = i64(v)
		}
	}
	return days
}

func TestAlcoholMood(t *testing.T) {
	got := AlcoholMood(alcoholFixture())
	want7 := []float64{1, 1, 3.5, 4, 4, 5, 8, 9, 9.1} // row 7 drops row 0; row 8 drops row 1
	for i, w := range want7 {
		if got[i].Alc7DaySum != w {
			t.Errorf("row %d alc_7day_sum = %v, want %v", i, got[i].Alc7DaySum, w)
		}
	}
	// 15-row mean of the 7-day sums (tenths): (10+10+35)/3 and 446/9.
	if got[2].Alc15DayAvg != 55.0/30 || got[8].Alc15DayAvg != 446.0/90 {
		t.Errorf("alc_15day_avg = %v, %v; want %v, %v", got[2].Alc15DayAvg, got[8].Alc15DayAvg, 55.0/30, 446.0/90)
	}
	if got[0].DepRaw != nil || got[1].DepRaw == nil || *got[1].DepRaw != 1 {
		t.Errorf("dep_raw rows 0/1 = %v/%v, want null/1", got[0].DepRaw, got[1].DepRaw)
	}
	// Mean of the LOGGED moods in the window; null when none.
	if got[0].Dep7DayAvg != nil {
		t.Errorf("row 0 dep_7day_avg = %v, want null", *got[0].Dep7DayAvg)
	}
	if d := got[7].Dep7DayAvg; d == nil || *d != 0.25 { // rows 1..7: 1.0, -0.5
		t.Errorf("row 7 dep_7day_avg = %v, want 0.25", d)
	}
	if d := got[8].Dep7DayAvg; d == nil || *d != -0.5 { // rows 2..8: -0.5
		t.Errorf("row 8 dep_7day_avg = %v, want -0.5", d)
	}
}

func TestSleep(t *testing.T) {
	got := Sleep(alcoholFixture())
	if got[0].SleepRaw != nil || got[0].Sleep7DayAvg != nil {
		t.Errorf("row 0 = %v/%v, want null/null", got[0].SleepRaw, got[0].Sleep7DayAvg)
	}
	if a := got[7].Sleep7DayAvg; a == nil || *a != 75 { // rows 1..7: 80, 70
		t.Errorf("row 7 sleep_7day_avg = %v, want 75", a)
	}
	if a := got[8].Sleep7DayAvg; a == nil || *a != 80 { // rows 2..8: 70, 90
		t.Errorf("row 8 sleep_7day_avg = %v, want 80", a)
	}
}

func TestRoundPG(t *testing.T) {
	cases := []struct {
		v      float64
		places int
		want   float64
	}{
		{2.25, 1, 2.3},   // half away from zero, not to even
		{-2.25, 1, -2.3}, // … on both sides
		{1.005, 2, 1.01}, // a float holds 1.00499999…; %.15g first makes it 1.005
		{2718.0875576, 1, 2718.1},
		{123.44, 1, 123.4},
	}
	for _, c := range cases {
		if got := roundPG(c.v, c.places); got != c.want {
			t.Errorf("roundPG(%v, %d) = %v, want %v", c.v, c.places, got, c.want)
		}
	}
}

func TestTail(t *testing.T) {
	if got := tail([]int{1, 2, 3, 4}, 2); !reflect.DeepEqual(got, []int{3, 4}) {
		t.Errorf("tail = %v", got)
	}
	if got := tail([]int{1, 2}, 5); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Errorf("tail = %v", got)
	}
}

func TestRequireToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		token, header string
		want          int
	}{
		{"s3cret", "", http.StatusUnauthorized},
		{"s3cret", "Bearer wrong", http.StatusUnauthorized},
		{"s3cret", "Basic s3cret", http.StatusUnauthorized},
		{"s3cret", "Bearer s3cret", http.StatusOK},
		{"s3cret", "bearer s3cret", http.StatusOK},
		{"", "Bearer ", http.StatusServiceUnavailable}, // no token configured: sync off
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/status/last-sync", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		requireToken(c.token)(ok).ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("token %q, header %q: status %d, want %d", c.token, c.header, rec.Code, c.want)
		}
	}
}
