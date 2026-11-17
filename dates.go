package main

import (
	"fmt"
	"strings"
	"time"
)

var dateLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}

// parseWhen parses a user-supplied date or timestamp into unix milliseconds.
// Values without a zone are interpreted in the server's local time zone (TZ). Empty input returns 0.
func parseWhen(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	for _, layout := range dateLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("invalid date %q: use YYYY-MM-DD, YYYY-MM-DDTHH:MM or RFC 3339", s)
}

// timeRange parses optional after/before bounds. before == 0 means no upper bound.
func timeRange(after, before string) (from, until int64, err error) {
	if from, err = parseWhen(after); err != nil {
		return 0, 0, err
	}
	if until, err = parseWhen(before); err != nil {
		return 0, 0, err
	}
	if until != 0 && from >= until {
		return 0, 0, fmt.Errorf("after (%s) must be earlier than before (%s)", after, before)
	}
	return from, until, nil
}
