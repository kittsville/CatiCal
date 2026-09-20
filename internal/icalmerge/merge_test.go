package icalmerge

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testdataICS(t *testing.T, name string) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "ics", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func mustMerge(t *testing.T, name string, sources []NamedCalendar) []byte {
	t.Helper()
	out, err := Merge(name, sources)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return out
}

func TestMerge_DisjointEvents(t *testing.T) {
	out := mustMerge(t, "Together", []NamedCalendar{
		{ID: "srcA", ICS: testdataICS(t, "calendar_a.ics")},
		{ID: "srcB", ICS: testdataICS(t, "calendar_b.ics")},
	})
	body := string(out)
	if !strings.Contains(body, "SUMMARY:Meeting A") {
		t.Fatalf("missing event A:\n%s", body)
	}
	if !strings.Contains(body, "SUMMARY:Meeting B") {
		t.Fatalf("missing event B:\n%s", body)
	}
}

func TestMerge_DuplicateTZIDOneVTimezone(t *testing.T) {
	out := mustMerge(t, "TZ", []NamedCalendar{
		{ID: "srcA", ICS: testdataICS(t, "tz_a.ics")},
		{ID: "srcB", ICS: testdataICS(t, "tz_b.ics")},
	})
	body := string(out)
	if n := strings.Count(body, "BEGIN:VTIMEZONE"); n != 1 {
		t.Fatalf("got %d VTIMEZONE components, want 1:\n%s", n, body)
	}
	if !strings.Contains(body, "TZID:Europe/London") {
		t.Fatalf("missing TZID:\n%s", body)
	}
	if !strings.Contains(body, "SUMMARY:London A") || !strings.Contains(body, "SUMMARY:London B") {
		t.Fatalf("missing events:\n%s", body)
	}
}

func TestMerge_SameUIDPrefixedPerSource(t *testing.T) {
	rec := testdataICS(t, "recurrence.ics")
	out := mustMerge(t, "Both", []NamedCalendar{
		{ID: "srcA", ICS: rec},
		{ID: "srcB", ICS: rec},
	})
	body := string(out)
	if !strings.Contains(body, "UID:srcA:weekly-standup") {
		t.Fatalf("missing prefixed srcA UID:\n%s", body)
	}
	if !strings.Contains(body, "UID:srcB:weekly-standup") {
		t.Fatalf("missing prefixed srcB UID:\n%s", body)
	}
	if strings.Contains(body, "\nUID:weekly-standup") || strings.Contains(body, "\r\nUID:weekly-standup") {
		t.Fatalf("unprefixed UID still present:\n%s", body)
	}
	if n := strings.Count(body, "UID:srcA:weekly-standup"); n != 2 {
		t.Fatalf("srcA master+exception should share UID, got %d:\n%s", n, body)
	}
	if n := strings.Count(body, "UID:srcB:weekly-standup"); n != 2 {
		t.Fatalf("srcB master+exception should share UID, got %d:\n%s", n, body)
	}
}

func TestMerge_RoundTripRecurrenceAlarmAndXProps(t *testing.T) {
	out := mustMerge(t, "Rec", []NamedCalendar{
		{ID: "srcA", ICS: testdataICS(t, "recurrence.ics")},
	})
	body := string(out)
	for _, want := range []string{
		"RRULE:FREQ=WEEKLY;BYDAY=MO",
		"RECURRENCE-ID:20260928T090000Z",
		"BEGIN:VALARM",
		"X-CUSTOM-FLAG:keep-me",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
}

func TestMerge_OptionalSummaryPrefix(t *testing.T) {
	out := mustMerge(t, "Work feed", []NamedCalendar{
		{ID: "srcA", SummaryPrefix: "Work", ICS: testdataICS(t, "summary.ics")},
	})
	body := string(out)
	if !strings.Contains(body, "SUMMARY:Work: Standup") {
		t.Fatalf("want prefixed summary, got:\n%s", body)
	}
}

func TestMerge_CRLFVersionProdIDAndCalName(t *testing.T) {
	out := mustMerge(t, "Family mix", []NamedCalendar{
		{ID: "srcA", ICS: testdataICS(t, "calendar_a.ics")},
	})
	if bytes.Contains(out, []byte("\r\r\n")) {
		t.Fatal("double CR in output")
	}
	if !bytes.Contains(out, []byte("\r\n")) {
		t.Fatal("output is not CRLF")
	}
	if bytes.Contains(out, []byte("\n")) && !bytes.Contains(out, []byte("\r\n")) {
		t.Fatal("bare LF without CR")
	}
	// Every line break should be CRLF (no lone LF).
	for i, b := range out {
		if b == '\n' && (i == 0 || out[i-1] != '\r') {
			t.Fatalf("lone LF at index %d", i)
		}
	}
	body := string(out)
	if !strings.Contains(body, "VERSION:2.0") {
		t.Fatalf("missing VERSION:2.0:\n%s", body)
	}
	if !strings.Contains(strings.ToUpper(body), "CATICAL") {
		t.Fatalf("PRODID should identify Catical:\n%s", body)
	}
	if !strings.Contains(body, "X-WR-CALNAME:Family mix") {
		t.Fatalf("missing calendar name:\n%s", body)
	}
}

func TestParse_RejectsNonCalendar(t *testing.T) {
	if err := Parse([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//t//EN\r\nEND:VCALENDAR\r\n")); err != nil {
		t.Fatalf("empty VCALENDAR should parse: %v", err)
	}
	if err := Parse([]byte("not a calendar")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestMerge_MalformedPropertyDoesNotDropEvent(t *testing.T) {
	out := mustMerge(t, "Google-ish", []NamedCalendar{
		{ID: "srcA", ICS: testdataICS(t, "google_malformed.ics")},
	})
	body := string(out)
	if !strings.Contains(body, "SUMMARY:Keep this event") {
		t.Fatalf("malformed extra property dropped the event:\n%s", body)
	}
	if !strings.Contains(body, "LOCATION:Office") {
		t.Fatalf("properties after malformed line should survive:\n%s", body)
	}
}
