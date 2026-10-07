package protocol

import "time"

// TimestampLayout is the one wire form of every protocol timestamp: UTC, whole
// seconds, a literal Z ("YYYY-MM-DDTHH:MM:SSZ").
const TimestampLayout = "2006-01-02T15:04:05Z"

// FormatTimestamp renders t in the wire form, converted to UTC and truncated to
// whole seconds.
func FormatTimestamp(t time.Time) string { return t.UTC().Format(TimestampLayout) }

// ParseTimestamp parses the wire form exactly. Other layouts, offsets, fractional
// seconds and impossible dates are rejected.
func ParseTimestamp(s string) (time.Time, error) {
	t, err := time.Parse(TimestampLayout, s)
	if err != nil {
		return time.Time{}, invalid("timestamp is not YYYY-MM-DDTHH:MM:SSZ")
	}
	return t, nil
}
