package main

import (
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Warsaw")
	old := time.Local
	time.Local = loc
	defer func() { time.Local = old }()

	cases := map[string]time.Time{
		"2026-11-17":           time.Date(2026, 11, 17, 0, 0, 0, 0, loc),
		"2026-11-17T08:30":     time.Date(2026, 11, 17, 8, 30, 0, 0, loc),
		"2026-11-17 08:30:15":  time.Date(2026, 11, 17, 8, 30, 15, 0, loc),
		"2026-11-17T08:30:00Z": time.Date(2026, 11, 17, 8, 30, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseWhen(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want.UnixMilli() {
			t.Errorf("%s: got %v, want %v", in, time.UnixMilli(got), want)
		}
	}
	if v, err := parseWhen(""); err != nil || v != 0 {
		t.Errorf("empty: %v %v", v, err)
	}
	if _, err := parseWhen("yesterday"); err == nil {
		t.Error("expected error for free text")
	}
}

func TestTimeRange(t *testing.T) {
	if _, _, err := timeRange("2026-11-18", "2026-11-17"); err == nil {
		t.Error("expected error when after >= before")
	}
	from, until, err := timeRange("2026-11-17", "")
	if err != nil || from == 0 || until != 0 {
		t.Errorf("open range: %v %v %v", from, until, err)
	}
}
