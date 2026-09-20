package icalmerge

import (
	"bytes"
	"fmt"
	"strings"

	ics "github.com/arran4/golang-ical"
)

const prodID = "-//Catical//catical//EN"

// NamedCalendar is one origin calendar to fold into the merge.
type NamedCalendar struct {
	// ID is a stable source key used as a UID prefix (e.g. "srcA:uid").
	ID string
	// SummaryPrefix, if non-empty, is prepended to each VEVENT SUMMARY as "Prefix: ".
	SummaryPrefix string
	ICS           []byte
}

// Merge combines source calendars into one VCALENDAR named name.
// Components are copied (not mapped to a DTO). Duplicate VTIMEZONE TZIDs
// are kept once. Event UIDs are prefixed with each source ID so collisions
// across feeds stay distinct while master/exception events in one source share
// a prefix.
func Merge(name string, sources []NamedCalendar) ([]byte, error) {
	out := ics.NewCalendar()
	out.SetVersion("2.0")
	out.SetProductId(prodID)
	if name != "" {
		out.SetXWRCalName(name)
	}

	seenTZ := make(map[string]struct{})

	for _, src := range sources {
		cal, err := parseICS(src.ICS)
		if err != nil {
			return nil, fmt.Errorf("parse source %s: %w", src.ID, err)
		}
		for _, comp := range cal.Components {
			if err := copyComponent(out, comp, src, seenTZ); err != nil {
				return nil, err
			}
		}
	}

	serialized := out.Serialize()
	return []byte(crlf(serialized)), nil
}

func parseICS(raw []byte) (*ics.Calendar, error) {
	return ics.ParseCalendarWithOptions(
		bytes.NewReader(raw),
		ics.WithPropertyParser(ics.FallbackParser(ics.LooseParser)),
	)
}

func copyComponent(out *ics.Calendar, comp ics.Component, src NamedCalendar, seenTZ map[string]struct{}) error {
	switch c := comp.(type) {
	case *ics.VTimezone:
		tzid := timezoneID(c)
		if tzid != "" {
			if _, ok := seenTZ[tzid]; ok {
				return nil
			}
			seenTZ[tzid] = struct{}{}
		}
		out.Components = append(out.Components, c)
		return nil
	case *ics.VEvent:
		prefixUID(c, src.ID)
		prefixSummary(c, src.SummaryPrefix)
		out.Components = append(out.Components, c)
		return nil
	default:
		if p, ok := comp.(interface {
			GetProperty(ics.ComponentProperty) *ics.IANAProperty
			SetProperty(ics.ComponentProperty, string, ...ics.PropertyParameter)
		}); ok {
			prefixUID(p, src.ID)
		}
		out.Components = append(out.Components, comp)
		return nil
	}
}

type uidSetter interface {
	GetProperty(ics.ComponentProperty) *ics.IANAProperty
	SetProperty(ics.ComponentProperty, string, ...ics.PropertyParameter)
}

func prefixUID(c uidSetter, sourceID string) {
	if sourceID == "" {
		return
	}
	prop := c.GetProperty(ics.ComponentPropertyUniqueId)
	if prop == nil || prop.Value == "" {
		return
	}
	c.SetProperty(ics.ComponentPropertyUniqueId, sourceID+":"+prop.Value)
}

func prefixSummary(ev *ics.VEvent, prefix string) {
	if prefix == "" {
		return
	}
	prop := ev.GetProperty(ics.ComponentPropertySummary)
	if prop == nil {
		ev.SetSummary(prefix + ":")
		return
	}
	ev.SetSummary(prefix + ": " + prop.Value)
}

func timezoneID(tz *ics.VTimezone) string {
	if p := tz.GetProperty(ics.ComponentPropertyTzid); p != nil {
		return p.Value
	}
	if p := tz.GetProperty(ics.ComponentProperty("TZID")); p != nil {
		return p.Value
	}
	return ""
}

func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return s
}
