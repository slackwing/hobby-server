package sxiva

// Sabbatical '27 — the squares grid at the top of the status page (feathers
// foundry/website/html/status/sabbatical-27.js). This is how many squares of
// each type a day earns, BEFORE the page's grid caps and bulldozes them.
// Only these counts leave the server: the page is public, the per-category
// minutes stay private. Types are named by their grid color, not by what
// they measure — what each color means is Andrew's business. Temporary:
// delete with the plugin.

import (
	"math"
	"sort"
)

const (
	minutesPerSquare = 10 // every 10 minutes is a square, rounded half-up (5 → 1)
	drinksAllowance  = 2  // blue: 2 − drinks (rounded half-up), never below 0
)

// SabbaticalDay is one day's square counts, one field per grid color.
type SabbaticalDay struct {
	Date   string `json:"date"`
	Green  int    `json:"green"`  // manuscript-studio points (the page caps them)
	Red    int    `json:"red"`    // [nflx] + [leet] minutes
	Orange int    `json:"orange"` // [crco] minutes
	White  int    `json:"white"`  // [staff] minutes
	Yellow int    `json:"yellow"` // [abi] + [hxh] + [rv] minutes
	Blue   int    `json:"blue"`   // [exe] and [alc]
	Brown  int    `json:"brown"`  // [rk] minutes
	Purple int    `json:"purple"` // [crit] minutes
}

func (d SabbaticalDay) empty() bool {
	return d.Green+d.Red+d.Orange+d.White+d.Yellow+d.Blue+d.Brown+d.Purple == 0
}

// Colors fed by category minutes: the codes' minutes are summed, then
// turned into squares.
var sabbaticalMinutes = []struct {
	codes []string
	set   func(*SabbaticalDay, int)
}{
	{[]string{"nflx", "leet"}, func(d *SabbaticalDay, n int) { d.Red = n }},
	{[]string{"crco"}, func(d *SabbaticalDay, n int) { d.Orange = n }},
	{[]string{"staff"}, func(d *SabbaticalDay, n int) { d.White = n }},
	{[]string{"crit"}, func(d *SabbaticalDay, n int) { d.Purple = n }},
	{[]string{"rk"}, func(d *SabbaticalDay, n int) { d.Brown = n }},
	{[]string{"abi", "hxh", "rv"}, func(d *SabbaticalDay, n int) { d.Yellow = n }},
}

// squaresFromMinutes: one per 10 minutes, half-up.
func squaresFromMinutes(m int64) int {
	if m <= 0 {
		return 0
	}
	return int((m + minutesPerSquare/2) / minutesPerSquare)
}

// blueSquares: one per 0.5 of [exe], plus 2 − drinks ([alc] rounded
// half-up, so half a drink counts as one). A blank [alc] is unknown, not
// zero: it earns nothing.
func blueSquares(d Day) int {
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

// Sabbatical turns the sxiva days and manuscript-studio's daily points
// (date → points; nil when unavailable) into per-day square counts. Days
// that earn nothing are left out; oldest first. No caps here — the page's
// grid applies them (schema max), so the counts stay the raw truth.
func Sabbatical(days []Day, points map[string]int) []SabbaticalDay {
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
		out.Blue = blueSquares(d)
	}
	for date, p := range points {
		get(date).Green = max(0, p)
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
