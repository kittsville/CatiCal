package icalmerge

import (
	"fmt"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/teambition/rrule-go"
)

// maxOccurrenceChecks bounds how far a recurrence walk runs.
// Hitting the cap keeps the series: a missed include would drop a live calendar,
// while a missed exclude leaves one component in the feed.
const maxOccurrenceChecks = 2000

// Window is the mix-level range of events to keep, measured in calendar months
// from Now. A nil bound leaves that side open. Both nil disables filtering.
type Window struct {
	Now          time.Time
	PastMonths   *int
	FutureMonths *int
}

// Active reports whether either bound is set.
func (w Window) Active() bool {
	return w.PastMonths != nil || w.FutureMonths != nil
}

func (w Window) bounds() (earliest, latest *time.Time) {
	if w.PastMonths != nil {
		t := w.Now.AddDate(0, -*w.PastMonths, 0)
		earliest = &t
	}
	if w.FutureMonths != nil {
		t := w.Now.AddDate(0, *w.FutureMonths, 0)
		latest = &t
	}
	return earliest, latest
}

// eventsInWindow marks each VEVENT to keep. Recurring components are kept
// whole when any occurrence would overlap the window.
func eventsInWindow(cal *ics.Calendar, w Window) map[*ics.VEvent]bool {
	earliest, latest := w.bounds()
	groups := map[string][]*ics.VEvent{}
	var order []string
	var anonymous []*ics.VEvent
	for _, comp := range cal.Components {
		ev, ok := comp.(*ics.VEvent)
		if !ok {
			continue
		}
		uid := eventUID(ev)
		if uid == "" {
			anonymous = append(anonymous, ev)
			continue
		}
		if _, seen := groups[uid]; !seen {
			order = append(order, uid)
		}
		groups[uid] = append(groups[uid], ev)
	}
	keep := make(map[*ics.VEvent]bool)
	for _, uid := range order {
		evs := groups[uid]
		hit := groupHits(evs, earliest, latest)
		for _, ev := range evs {
			keep[ev] = hit
		}
	}
	for _, ev := range anonymous {
		keep[ev] = eventHits(ev, earliest, latest)
	}
	return keep
}

func eventUID(ev *ics.VEvent) string {
	prop := ev.GetProperty(ics.ComponentPropertyUniqueId)
	if prop == nil {
		return ""
	}
	return prop.Value
}

func groupHits(evs []*ics.VEvent, earliest, latest *time.Time) bool {
	for _, ev := range evs {
		if eventHits(ev, earliest, latest) {
			return true
		}
	}
	return false
}

func eventHits(ev *ics.VEvent, earliest, latest *time.Time) bool {
	if len(ev.GetProperties(ics.ComponentPropertyRrule)) > 0 || len(ev.GetProperties(ics.ComponentPropertyRdate)) > 0 {
		return seriesHits(ev, earliest, latest)
	}
	return spanHits(ev, earliest, latest)
}

func spanHits(ev *ics.VEvent, earliest, latest *time.Time) bool {
	start, end, ok := componentSpan(ev)
	if !ok {
		return true
	}
	return overlaps(start, end, earliest, latest)
}

func overlaps(start, end time.Time, earliest, latest *time.Time) bool {
	if end.Before(start) {
		end = start
	}
	if latest != nil && !start.Before(*latest) {
		return false
	}
	if earliest != nil && !end.After(*earliest) {
		return false
	}
	return true
}

func seriesHits(ev *ics.VEvent, earliest, latest *time.Time) bool {
	start, end, ok := componentSpan(ev)
	if !ok {
		return true
	}
	duration := end.Sub(start)
	if duration < 0 {
		duration = 0
	}
	rdates, err := ev.GetRDates()
	if err != nil {
		return true
	}
	exdates, err := ev.GetExDates()
	if err != nil {
		return true
	}
	set := &rrule.Set{}
	for _, prop := range ev.GetProperties(ics.ComponentPropertyRrule) {
		opt, err := rrule.StrToROption("RRULE:" + prop.Value)
		if err != nil {
			return true
		}
		opt.Dtstart = start
		rule, err := rrule.NewRRule(*opt)
		if err != nil {
			return true
		}
		set.RRule(rule)
	}
	for _, t := range rdates {
		set.RDate(t)
	}
	for _, t := range exdates {
		set.ExDate(t)
	}
	next := set.Iterator()
	var prev time.Time
	for i := 0; i < maxOccurrenceChecks; i++ {
		occ, ok := next()
		if !ok {
			return false
		}
		if !prev.IsZero() && !occ.After(prev) {
			return false
		}
		prev = occ
		if latest != nil && !occ.Before(*latest) {
			return false
		}
		if overlaps(occ, occ.Add(duration), earliest, latest) {
			return true
		}
	}
	return true
}

func componentSpan(ev *ics.VEvent) (start, end time.Time, ok bool) {
	startProp := ev.GetProperty(ics.ComponentPropertyDtStart)
	if startProp == nil {
		return time.Time{}, time.Time{}, false
	}
	var err error
	start, err = parsePropTime(startProp)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	if endProp := ev.GetProperty(ics.ComponentPropertyDtEnd); endProp != nil {
		end, err = parsePropTime(endProp)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return start, end, true
	}
	if durProp := ev.GetProperty(ics.ComponentPropertyDuration); durProp != nil {
		d, err := parseISODuration(durProp.Value)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return start, start.Add(d), true
	}
	if dateOnly(startProp) {
		return start, start.AddDate(0, 0, 1), true
	}
	return start, start, true
}

func dateOnly(prop *ics.IANAProperty) bool {
	for _, v := range prop.ICalParameters["VALUE"] {
		if strings.EqualFold(v, "DATE") {
			return true
		}
	}
	val := strings.TrimSpace(prop.Value)
	return len(val) == 8 && !strings.Contains(val, "T")
}

func parsePropTime(prop *ics.IANAProperty) (time.Time, error) {
	val := strings.TrimSpace(prop.Value)
	if tzids := prop.ICalParameters["TZID"]; len(tzids) == 1 && tzids[0] != "" {
		loc, err := time.LoadLocation(tzids[0])
		if err != nil {
			return time.Time{}, err
		}
		if t, err := time.ParseInLocation("20060102T150405", val, loc); err == nil {
			return t, nil
		}
		if t, err := time.ParseInLocation("20060102", val, loc); err == nil {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("unparsed time %q", val)
	}
	if t, err := time.Parse("20060102T150405Z", val); err == nil {
		return t, nil
	}
	if t, err := time.Parse("20060102T150405", val); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("20060102", val); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unparsed time %q", val)
}

func parseISODuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	neg := false
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") || len(s) < 2 {
		return 0, fmt.Errorf("duration %q", raw)
	}
	s = s[1:]
	var weeks, days, hours, mins, secs int
	if i := strings.IndexByte(s, 'W'); i >= 0 && !strings.Contains(s, "T") {
		n, err := fmt.Sscanf(s, "%dW", &weeks)
		if err != nil || n != 1 {
			return 0, fmt.Errorf("duration %q", raw)
		}
	} else {
		date, timePart, _ := strings.Cut(s, "T")
		if date != "" {
			if _, err := fmt.Sscanf(date, "%dD", &days); err != nil || !strings.HasSuffix(date, "D") {
				return 0, fmt.Errorf("duration %q", raw)
			}
		}
		if timePart != "" {
			rest := timePart
			for _, spec := range []struct {
				suffix byte
				dst    *int
			}{
				{'H', &hours},
				{'M', &mins},
				{'S', &secs},
			} {
				i := strings.IndexByte(rest, spec.suffix)
				if i < 0 {
					continue
				}
				if _, err := fmt.Sscanf(rest[:i+1], "%d"+string(spec.suffix), spec.dst); err != nil {
					return 0, fmt.Errorf("duration %q", raw)
				}
				rest = rest[i+1:]
			}
			if rest != "" {
				return 0, fmt.Errorf("duration %q", raw)
			}
		}
	}
	d := (time.Duration(weeks)*7+time.Duration(days))*24*time.Hour +
		time.Duration(hours)*time.Hour +
		time.Duration(mins)*time.Minute +
		time.Duration(secs)*time.Second
	if neg {
		d = -d
	}
	return d, nil
}
