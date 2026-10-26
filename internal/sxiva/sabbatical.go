package sxiva

// Sabbatical '27 — the squares grid at the top of the status page (feathers
// foundry/website/html/status/sabbatical-27.js; plan:
// foundry/website/docs/SABBATICAL_27_PLAN.md). This is how many squares of
// each type a day earns, BEFORE the page's grid bulldozes any overflow.
// Only these counts leave the server: the page is public, the per-category
// minutes stay private. Temporary — delete with the plugin.

import (
	"math"
	"sort"
)

const (
	minutesPerSquare  = 10 // every 10 minutes is a square, rounded half-up (5 → 1)
	maxWritingSquares = 10 // writing: up to 10 of the day's manuscript-studio points
	drinksAllowance   = 2  // health: 2 − drinks (rounded half-up), never below 0
)

// SabbaticalDay is one day's square counts, one field per grid type.
type SabbaticalDay struct {
	Date                 string `json:"date"`
	Writing              int    `json:"writing"`
	Netflix              int    `json:"netflix"`
	CrucialConversations int    `json:"crucial-conversations"`
	StaffEngineer        int    `json:"staff-engineer"`
	Abi                  int    `json:"abi"`
	Health               int    `json:"health"`
	Editing              int    `json:"editing"`
	CriticalReading      int    `json:"critical-reading"`
}

func (d SabbaticalDay) empty() bool {
	return d.Writing+d.Netflix+d.CrucialConversations+d.StaffEngineer+d.Abi+d.Health+d.Editing+d.CriticalReading == 0
}

// Types fed by category minutes: the codes' minutes are summed, then
// turned into squares.
var sabbaticalMinutes = []struct {
	codes []string
	set   func(*SabbaticalDay, int)
}{
	{[]string{"nflx"}, func(d *SabbaticalDay, n int) { d.Netflix = n }},
	{[]string{"crco"}, func(d *SabbaticalDay, n int) { d.CrucialConversations = n }},
	{[]string{"staff"}, func(d *SabbaticalDay, n int) { d.StaffEngineer = n }},
	{[]string{"crit"}, func(d *SabbaticalDay, n int) { d.CriticalReading = n }},
	{[]string{"rk"}, func(d *SabbaticalDay, n int) { d.Editing = n }},
	{[]string{"abi", "hxh", "rv"}, func(d *SabbaticalDay, n int) { d.Abi = n }},
}

// squaresFromMinutes: one per 10 minutes, half-up.
func squaresFromMinutes(m int64) int {
	if m <= 0 {
		return 0
	}
	return int((m + minutesPerSquare/2) / minutesPerSquare)
}

// healthSquares: one per 0.5 of [exe], plus 2 − drinks ([alc] rounded
// half-up, so half a drink counts as one). A blank [alc] is unknown, not
// zero: it earns nothing.
func healthSquares(d Day) int {
	n := 0
	if d.Exe != nil && *d.Exe > 0 {
		n += int(math.Floor(*d.Exe*2 + 1e-9))
	}
	if d.Alc != nil {
		drinks := int(math.Floor(*d.Alc + 0.5))
		if left := drinksAllowance - drinks; left > 0 {
			n += left
		}
	}
	return n
}

// Sabbatical turns the sxiva days and manuscript-studio's daily writing
// points (date → points; nil when unavailable) into per-day square counts.
// Days that earn nothing are left out; oldest first.
func Sabbatical(days []Day, writing map[string]int) []SabbaticalDay {
	byDate := map[string]*SabbaticalDay{}
	get := func(date string) *SabbaticalDay {
		if byDate[date] == nil {
			byDate[date] = &SabbaticalDay{Date: date}
		}
		return byDate[date]
	}
	for _, d := range days {
		out := get(d.Date.Format(dateLayout))
		for _, m := range sabbaticalMinutes {
			var sum int64
			for _, code := range m.codes {
				sum += d.CategoryMinutes[code]
			}
			m.set(out, squaresFromMinutes(sum))
		}
		out.Health = healthSquares(d)
	}
	for date, points := range writing {
		get(date).Writing = min(maxWritingSquares, max(0, points))
	}
	out := []SabbaticalDay{}
	for _, d := range byDate {
		if !d.empty() {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}
