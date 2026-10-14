package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ParseTime reads a JSON date as a server writes it, trying each layout in turn: null and "" are the
// zero time, which is how a server says it has no date, and a date no layout reads is an error naming
// it, so a layout a server adds is found rather than read as nothing. A generated package's Time and
// Day decode with it, each with the layouts its service's config gives.
func ParseTime(data []byte, layouts []string) (time.Time, error) {
	if string(data) == "null" {
		return time.Time{}, nil
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' && slices.Contains(layouts, DateObjectMillis) {
		// a date written as an object holding milliseconds since 1970, as TrueNAS writes every date
		var obj struct {
			Date *int64 `json:"$date"`
		}
		if err := json.Unmarshal(trimmed, &obj); err != nil || obj.Date == nil {
			return time.Time{}, &UnreadDateError{Text: string(trimmed), Layouts: layouts}
		}
		if *obj.Date == 0 {
			return time.Time{}, nil
		}

		return time.UnixMilli(*obj.Date).UTC(), nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return time.Time{}, fmt.Errorf("a date that is not a string: %s", data)
	}
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, &UnreadDateError{Text: s, Layouts: layouts}
}

// DateObjectMillis is the layout of a date written as an object holding milliseconds since 1970, {"$date": 1788181994000},
// which is how TrueNAS writes every date on the wire whatever its description says: a service names it in Dates.Send to write
// dates so, and in Dates.Read to read them so beside the text layouts.
const DateObjectMillis = "$date"

// MarshalTime writes a date in a layout as JSON: the object form for DateObjectMillis, the text otherwise.
func MarshalTime(t time.Time, layout string) ([]byte, error) {
	if layout == DateObjectMillis {
		return fmt.Appendf(nil, `{"$date":%d}`, t.UnixMilli()), nil
	}

	return json.Marshal(t.Format(layout))
}

// FormatTime is a date in a layout as text: milliseconds since 1970 for DateObjectMillis, which has no text form.
func FormatTime(t time.Time, layout string) string {
	if layout == DateObjectMillis {
		return strconv.FormatInt(t.UnixMilli(), 10)
	}

	return t.Format(layout)
}

// UnreadDateError is the error for a date no layout reads, carrying the text so a generated type set to keep
// such dates can hold it rather than fail.
type UnreadDateError struct {
	Text    string
	Layouts []string
}

func (e *UnreadDateError) Error() string {
	return fmt.Sprintf("a date no layout reads: %q (layouts %s)", e.Text, strings.Join(e.Layouts, ", "))
}

// ParseUnix reads a JSON date written as seconds or milliseconds since 1970: a number, or a string of
// digits as some servers write it; null and 0 are zero, which is how a server says it has no date.
func ParseUnix(data []byte) (int64, error) {
	text := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if text == "null" || text == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("a date that is not a whole number of seconds or milliseconds: %s", data)
	}

	return n, nil
}
