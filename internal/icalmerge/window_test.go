package icalmerge

import (
	"strings"
	"testing"
	"time"
)

func TestMergeWithWindow(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	past, future := 2, 3
	window := Window{Now: now, PastMonths: &past, FutureMonths: &future}

	ics := []byte(`BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Test//Win//EN
BEGIN:VEVENT
UID:old
DTSTART:20200101T100000Z
DTEND:20200101T110000Z
SUMMARY:Old
END:VEVENT
BEGIN:VEVENT
UID:recent
DTSTART:20260920T100000Z
DTEND:20260920T110000Z
SUMMARY:Recent
END:VEVENT
BEGIN:VEVENT
UID:far
DTSTART:20270601T100000Z
DTEND:20270601T110000Z
SUMMARY:Far
END:VEVENT
BEGIN:VEVENT
UID:span
DTSTART:20260701T100000Z
DTEND:20260815T100000Z
SUMMARY:Spans
END:VEVENT
BEGIN:VEVENT
UID:ended
DTSTART:20260701T100000Z
DTEND:20260715T100000Z
SUMMARY:EndedBefore
END:VEVENT
BEGIN:VEVENT
UID:allday
DTSTART;VALUE=DATE:20260901
DTEND;VALUE=DATE:20260902
SUMMARY:AllDay
END:VEVENT
BEGIN:VEVENT
UID:dur
DTSTART:20261001T100000Z
DURATION:PT1H
SUMMARY:Duration
END:VEVENT
BEGIN:VEVENT
UID:rdate
DTSTART:20200101T100000Z
DTEND:20200101T110000Z
RDATE:20261002T100000Z
SUMMARY:ExtraDate
END:VEVENT
BEGIN:VEVENT
UID:weekly
DTSTART:20240101T090000Z
DTEND:20240101T093000Z
RRULE:FREQ=WEEKLY;BYDAY=MO
SUMMARY:Weekly
END:VEVENT
BEGIN:VEVENT
UID:dead
DTSTART:20200106T090000Z
DTEND:20200106T093000Z
RRULE:FREQ=WEEKLY;UNTIL=20200203T090000Z
SUMMARY:DeadWeekly
END:VEVENT
BEGIN:VEVENT
UID:excluded
DTSTART:20260907T090000Z
DTEND:20260907T100000Z
RRULE:FREQ=WEEKLY;COUNT=4
EXDATE:20260907T090000Z,20260914T090000Z,20260921T090000Z,20260928T090000Z
SUMMARY:Excluded
END:VEVENT
BEGIN:VEVENT
UID:moved
DTSTART:20200106T090000Z
DTEND:20200106T100000Z
RRULE:FREQ=WEEKLY;COUNT=1
SUMMARY:Once
END:VEVENT
BEGIN:VEVENT
UID:moved
RECURRENCE-ID:20200106T090000Z
DTSTART:20260928T100000Z
DTEND:20260928T110000Z
SUMMARY:Moved
END:VEVENT
END:VCALENDAR
`)

	out, err := MergeWithWindow("Mix", []NamedCalendar{{ID: "src", ICS: ics}}, window)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	for _, keep := range []string{"SUMMARY:Recent", "SUMMARY:Spans", "SUMMARY:AllDay", "SUMMARY:Duration", "SUMMARY:Weekly", "SUMMARY:Once", "SUMMARY:Moved", "SUMMARY:ExtraDate", "RRULE:FREQ=WEEKLY;BYDAY=MO"} {
		if !strings.Contains(body, keep) {
			t.Fatalf("missing %q:\n%s", keep, body)
		}
	}
	for _, drop := range []string{"SUMMARY:Old", "SUMMARY:Far", "SUMMARY:EndedBefore", "SUMMARY:DeadWeekly", "SUMMARY:Excluded"} {
		if strings.Contains(body, drop) {
			t.Fatalf("kept %q:\n%s", drop, body)
		}
	}
	if strings.Count(body, "SUMMARY:Weekly") != 1 {
		t.Fatalf("weekly series should stay one component:\n%s", body)
	}

	open, err := MergeWithWindow("Mix", []NamedCalendar{{ID: "src", ICS: ics}}, Window{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(open), "SUMMARY:Old") || !strings.Contains(string(open), "SUMMARY:Far") {
		t.Fatal("an open window should keep every event")
	}
}

func TestMergeWithWindow_ZeroPastDropsEarlierEvents(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	past := 0
	out := mustMergeWindow(t, Window{Now: now, PastMonths: &past}, testdataICS(t, "calendar_a.ics"))
	if strings.Contains(string(out), "SUMMARY:Meeting A") {
		t.Fatalf("event ended before now:\n%s", out)
	}
}

func mustMergeWindow(t *testing.T, w Window, ics []byte) []byte {
	t.Helper()
	out, err := MergeWithWindow("Mix", []NamedCalendar{{ID: "src", ICS: ics}}, w)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
