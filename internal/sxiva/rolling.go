package sxiva

// The dashboard's derived series, computed in Go over all rows (one query,
// ~one row per day). Semantics match the SQL of the Flask API this
// replaced (feathers 11.sxiv/dashboard/server/app.py) — the status page
// depends on the exact numbers:
//   - rolling windows count ROWS (a day with no file is not a row), except
//     hobby_weighted, whose window is calendar days;
//   - work is the last 5 WEEKDAY rows, whatever the window;
//   - meet (meeting minutes) counts as work and total, never other;
//   - study works like hobby (row sums, calendar-weighted "feels like").

import (
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"
)

const dateLayout = "2006-01-02"

// workDays is the work window: the last N weekday (Mon–Fri) rows.
const workDays = 5

// Day is one row of sxiva_daily_summary — the fields the dashboard reads.
// Pointers are NULL-able columns (NULL = not logged that day).
type Day struct {
	Date            time.Time // the calendar day, midnight UTC
	CategoryMinutes map[string]int64
	Meet            *int64
	Alc             *float64 // NUMERIC(2,1)
	Exe             *float64 // NUMERIC(2,1): exercise, in half steps
	DepAvg          *float64 // NUMERIC(2,1): the day's mood
	SleepScore      *int64
	DRCTriggers     *float64
	DRCResisted     *float64
	DRCIndulged     *float64
}

// Groups are the category codes the dashboard asks about. A code may sit
// in more than one group; a code in none of them is "other".
type Groups struct {
	Hobby, Work, Study []string
}

// CategoryRow is one day of GET /category-rolling-sum.
type CategoryRow struct {
	Date          string  `json:"date"`
	HobbyRaw      int64   `json:"hobby_raw"`
	HobbyWeighted float64 `json:"hobby_weighted"`
	WorkRaw       int64   `json:"work_raw"`
	WorkWeighted  float64 `json:"work_weighted"`
	StudyRaw      int64   `json:"study_raw"`
	StudyWeighted float64 `json:"study_weighted"`
	OtherRaw      int64   `json:"other_raw"`
	TotalRaw      int64   `json:"total_raw"`
}

// AlcoholMoodRow is one day of GET /alcohol-depression.
type AlcoholMoodRow struct {
	Date        string   `json:"date"`
	Alc7DaySum  float64  `json:"alc_7day_sum"`
	Alc15DayAvg float64  `json:"alc_15day_avg"`
	DepRaw      *float64 `json:"dep_raw"`
	Dep7DayAvg  *float64 `json:"dep_7day_avg"`
}

// SleepRow is one day of GET /sleep-score.
type SleepRow struct {
	Date         string   `json:"date"`
	SleepRaw     *float64 `json:"sleep_raw"`
	Sleep7DayAvg *float64 `json:"sleep_7day_avg"`
}

// DRCRow is one day of GET /drc.
type DRCRow struct {
	Date        string   `json:"date"`
	DRCTriggers *float64 `json:"drc_triggers"`
	DRCResisted *float64 `json:"drc_resisted"`
	DRCIndulged *float64 `json:"drc_indulged"`
}

// RollingCategory computes, per day:
//   - hobby_raw / study_raw / other_raw / total_raw: sums over the current
//     row and the window-1 rows before it;
//   - hobby_weighted / study_weighted: over the rows within the last
//     `window` CALENDAR days,
//     Σ minutes·w / Σ w · window with w = e^(−λ·age in days) — the "feels
//     like" figure, recent days counting more;
//   - work_raw / work_weighted: the same over the last 5 weekday rows up
//     to the current day (· 5 instead of · window); 0 before any weekday.
//
// days must be sorted by date, ascending; window ≥ 1.
func RollingCategory(days []Day, groups Groups, window int, lambda float64) []CategoryRow {
	inHobby, inWork, inStudy := setOf(groups.Hobby), setOf(groups.Work), setOf(groups.Study)
	n := len(days)
	hob, wrk, stu, oth, tot := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	for i, d := range days {
		for k, v := range d.CategoryMinutes {
			if inHobby[k] {
				hob[i] += v
			}
			if inWork[k] {
				wrk[i] += v
			}
			if inStudy[k] {
				stu[i] += v
			}
			if !inHobby[k] && !inWork[k] && !inStudy[k] {
				oth[i] += v
			}
			tot[i] += v
		}
		if d.Meet != nil {
			wrk[i] += *d.Meet
			tot[i] += *d.Meet
		}
	}

	out := make([]CategoryRow, n)
	var weekdays []int // indices of the Mon–Fri rows so far
	for i, d := range days {
		if wd := d.Date.Weekday(); wd >= time.Monday && wd <= time.Friday {
			weekdays = append(weekdays, i)
		}
		row := CategoryRow{Date: d.Date.Format(dateLayout)}

		for j := max(0, i-window+1); j <= i; j++ {
			row.HobbyRaw += hob[j]
			row.StudyRaw += stu[j]
			row.OtherRaw += oth[j]
			row.TotalRaw += tot[j]
		}

		first := i // earliest row inside the calendar window
		for first > 0 && ageDays(d.Date, days[first-1].Date) < int64(window) {
			first--
		}
		var sumHW, sumSW, sumW float64
		for j := first; j <= i; j++ {
			w := math.Exp(-lambda * float64(ageDays(d.Date, days[j].Date)))
			sumHW += float64(hob[j]) * w
			sumSW += float64(stu[j]) * w
			sumW += w
		}
		row.HobbyWeighted = roundPG(sumHW*(float64(window)/sumW), 1)
		row.StudyWeighted = roundPG(sumSW*(float64(window)/sumW), 1)

		if last := weekdays[max(0, len(weekdays)-workDays):]; len(last) > 0 {
			var sumWW, sumW float64
			for _, j := range last {
				w := math.Exp(-lambda * float64(ageDays(d.Date, days[j].Date)))
				row.WorkRaw += wrk[j]
				sumWW += float64(wrk[j]) * w
				sumW += w
			}
			row.WorkWeighted = roundPG(sumWW*(float64(workDays)/sumW), 1)
		}
		out[i] = row
	}
	return out
}

// AlcoholMood computes, per day: alc_7day_sum = the drinks over the
// current and 6 previous rows (unlogged = 0); alc_15day_avg = the mean of
// alc_7day_sum over the current and 14 previous rows; dep_raw = the day's
// mood; dep_7day_avg = the mean of the logged moods among the current and
// 6 previous rows (null if none). Sums run in exact tenths (both columns
// are NUMERIC(2,1)).
func AlcoholMood(days []Day) []AlcoholMoodRow {
	alc7 := make([]int64, len(days)) // tenths
	out := make([]AlcoholMoodRow, len(days))
	for i, d := range days {
		var depSum, depN int64
		for j := max(0, i-6); j <= i; j++ {
			if days[j].Alc != nil {
				alc7[i] += tenths(*days[j].Alc)
			}
			if days[j].DepAvg != nil {
				depSum += tenths(*days[j].DepAvg)
				depN++
			}
		}
		lo := max(0, i-14)
		var sum15 int64
		for j := lo; j <= i; j++ {
			sum15 += alc7[j]
		}
		row := AlcoholMoodRow{
			Date:        d.Date.Format(dateLayout),
			Alc7DaySum:  float64(alc7[i]) / 10,
			Alc15DayAvg: float64(sum15) / float64(10*(i-lo+1)),
		}
		if d.DepAvg != nil {
			row.DepRaw = ptr(float64(tenths(*d.DepAvg)) / 10)
		}
		if depN > 0 {
			row.Dep7DayAvg = ptr(float64(depSum) / float64(10*depN))
		}
		out[i] = row
	}
	return out
}

// Sleep computes, per day, the sleep score and the mean of the logged
// scores among the current and 6 previous rows (null if none).
func Sleep(days []Day) []SleepRow {
	out := make([]SleepRow, len(days))
	for i, d := range days {
		row := SleepRow{Date: d.Date.Format(dateLayout)}
		if d.SleepScore != nil {
			row.SleepRaw = ptr(float64(*d.SleepScore))
		}
		var sum, n int64
		for j := max(0, i-6); j <= i; j++ {
			if days[j].SleepScore != nil {
				sum += *days[j].SleepScore
				n++
			}
		}
		if n > 0 {
			row.Sleep7DayAvg = ptr(float64(sum) / float64(n))
		}
		out[i] = row
	}
	return out
}

// DRC returns the raw daily counts.
func DRC(days []Day) []DRCRow {
	out := make([]DRCRow, len(days))
	for i, d := range days {
		out[i] = DRCRow{
			Date:        d.Date.Format(dateLayout),
			DRCTriggers: d.DRCTriggers,
			DRCResisted: d.DRCResisted,
			DRCIndulged: d.DRCIndulged,
		}
	}
	return out
}

// CategoryLists is every category code that occurs in the data, sorted,
// split by group ("other" = in no group).
type CategoryLists struct {
	Hobby, Work, Study, Other []string
}

// Categories splits the codes that occur in the data by group.
func Categories(days []Day, groups Groups) CategoryLists {
	seen := map[string]bool{}
	for _, d := range days {
		for k := range d.CategoryMinutes {
			seen[k] = true
		}
	}
	all := make([]string, 0, len(seen))
	for k := range seen {
		all = append(all, k)
	}
	sort.Strings(all)
	inHobby, inWork, inStudy := setOf(groups.Hobby), setOf(groups.Work), setOf(groups.Study)
	out := CategoryLists{Hobby: []string{}, Work: []string{}, Study: []string{}, Other: []string{}}
	for _, k := range all {
		if inHobby[k] {
			out.Hobby = append(out.Hobby, k)
		}
		if inWork[k] {
			out.Work = append(out.Work, k)
		}
		if inStudy[k] {
			out.Study = append(out.Study, k)
		}
		if !inHobby[k] && !inWork[k] && !inStudy[k] {
			out.Other = append(out.Other, k)
		}
	}
	return out
}

// roundPG rounds the way the old SQL did (float8 → numeric → ROUND(x, n)):
// first to 15 significant digits (Postgres' float8-to-numeric conversion
// prints %.15g), then to `places` decimals, half away from zero. Plain
// math.Round(v*10)/10 differs on values like 1.005, which a float holds as
// 1.00499999….
func roundPG(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'g', 15, 64))
	if !ok {
		return v
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	r.Mul(r, new(big.Rat).SetInt(scale))
	neg := r.Sign() < 0
	r.Abs(r)
	r.Add(r, big.NewRat(1, 2))
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if neg {
		q.Neg(q)
	}
	f, _ := new(big.Rat).SetFrac(q, scale).Float64()
	return f
}

// ageDays is how many calendar days `then` is before `now` (both dates).
func ageDays(now, then time.Time) int64 {
	return (now.Unix() - then.Unix()) / 86400
}

// tenths converts a NUMERIC(·,1) value to an exact integer of tenths.
func tenths(v float64) int64 {
	return int64(math.Round(v * 10))
}

func setOf(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func ptr[T any](v T) *T {
	return &v
}
