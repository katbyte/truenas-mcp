package truenasrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// A TrueNAS writes a few kinds of value in a way of its own, as an object with a key that starts with a dollar sign. The types here read and write
// them, for a field of a struct an answer is read into or an argument is made from.

// Time is a moment as a TrueNAS writes one: {"$date": 1791850926000}, the milliseconds since the start of 1970. Nothing, or null, reads as the zero
// time.
type Time struct{ time.Time }

func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}

	return json.Marshal(map[string]int64{"$date": t.UnixMilli()})
}

func (t *Time) UnmarshalJSON(data []byte) error {
	var wire *struct {
		Milliseconds *int64 `json:"$date"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("a time a TrueNAS wrote is not one: %w", err)
	}

	if wire == nil {
		t.Time = time.Time{}

		return nil
	}
	if wire.Milliseconds == nil {
		return errors.New(`a time a TrueNAS wrote has no "$date" in it`)
	}
	t.Time = time.UnixMilli(*wire.Milliseconds).UTC()

	return nil
}

// Date is a day with no time of day, as a TrueNAS writes one: {"$type": "date", "$value": "2026-10-12"}.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day) }

func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"$type": "date", "$value": d.String()})
}

func (d *Date) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type  string `json:"$type"`
		Value string `json:"$value"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Type != "date" {
		return errors.New("a date a TrueNAS wrote is not one")
	}

	day, err := time.Parse(time.DateOnly, wire.Value)
	if err != nil {
		return fmt.Errorf("a date a TrueNAS wrote is not one: %w", err)
	}
	d.Year, d.Month, d.Day = day.Date()

	return nil
}

// TimeOfDay is a time of day with no date, as a TrueNAS writes one: {"$time": "16:22:06"}. Zone is how far from UTC it says it is ("+01:00", "Z"),
// and empty when it does not say.
type TimeOfDay struct {
	Hour       int
	Minute     int
	Second     int
	Nanosecond int
	Zone       string
}

func (t TimeOfDay) String() string {
	out := fmt.Sprintf("%02d:%02d:%02d", t.Hour, t.Minute, t.Second)
	if t.Nanosecond != 0 {
		out += fmt.Sprintf(".%06d", t.Nanosecond/1000)
	}

	return out + t.Zone
}

func (t TimeOfDay) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"$time": t.String()})
}

func (t *TimeOfDay) UnmarshalJSON(data []byte) error {
	var wire struct {
		Value *string `json:"$time"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Value == nil {
		return errors.New("a time of day a TrueNAS wrote is not one")
	}

	clock, zone := *wire.Value, ""
	if at := strings.IndexAny(clock, "Z+-"); at > 0 {
		clock, zone = clock[:at], clock[at:]
	}
	clock, fraction, _ := strings.Cut(clock, ".")

	parts := strings.Split(clock, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("a time of day a TrueNAS wrote is not one: %q", *wire.Value)
	}

	read := TimeOfDay{Zone: zone}
	for i, into := range []*int{&read.Hour, &read.Minute, &read.Second}[:len(parts)] {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 || n > 59 || i == 0 && n > 23 {
			return fmt.Errorf("a time of day a TrueNAS wrote is not one: %q", *wire.Value)
		}
		*into = n
	}

	if fraction != "" {
		// a fraction is so many digits of a second: padded out to nine, it is nanoseconds
		n, err := strconv.Atoi((fraction + "000000000")[:9])
		if err != nil {
			return fmt.Errorf("a time of day a TrueNAS wrote is not one: %q", *wire.Value)
		}
		read.Nanosecond = n
	}
	*t = read

	return nil
}

// Set is a list a TrueNAS keeps as a set, without repeats and in no order worth relying on: {"$set": [1, 2, 3]}. A NAS writes some of them
// that way and others as a plain list, though its description calls both the same, so either reads.
type Set[T any] []T

func (s Set[T]) MarshalJSON() ([]byte, error) {
	items := []T(s)
	if items == nil {
		items = []T{}
	}

	return json.Marshal(map[string][]T{"$set": items})
}

func (s *Set[T]) UnmarshalJSON(data []byte) error {
	var plain []T
	if json.Unmarshal(data, &plain) == nil {
		*s = plain

		return nil
	}

	var wire struct {
		Items *[]T `json:"$set"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || wire.Items == nil {
		return errors.New("a set a TrueNAS wrote is not one")
	}
	*s = *wire.Items

	return nil
}

// IPInterface is an address together with the network it is on, as a TrueNAS writes one: {"$ipv4_interface": "192.168.0.10/24"}, or
// "$ipv6_interface" for the longer kind.
type IPInterface struct{ netip.Prefix }

func (i IPInterface) MarshalJSON() ([]byte, error) {
	key := "$ipv4_interface"
	if i.Addr().Is6() && !i.Addr().Is4In6() {
		key = "$ipv6_interface"
	}

	return json.Marshal(map[string]string{key: i.String()})
}

func (i *IPInterface) UnmarshalJSON(data []byte) error {
	var wire map[string]string
	if err := json.Unmarshal(data, &wire); err != nil || len(wire) != 1 {
		return errors.New("an address and its network a TrueNAS wrote is not one")
	}

	for key, value := range wire {
		if key != "$ipv4_interface" && key != "$ipv6_interface" {
			return errors.New("an address and its network a TrueNAS wrote is not one")
		}

		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("an address and its network a TrueNAS wrote is not one: %w", err)
		}
		i.Prefix = prefix
	}

	return nil
}

// HiddenSecret is what a TrueNAS answers in place of a secret the caller may not see.
const HiddenSecret = "********"

// Secret is a value a TrueNAS keeps secret: a password, a key, a certificate with its key. A caller who is not a full administrator, and may not
// change that part of the NAS, is answered HiddenSecret in its place whatever kind of value it is, a list and an object included, so a field read
// as its own kind would fail to read for a user who may only read. Hidden says the NAS kept it back, and Value is then empty.
//
// It prints as hidden with fmt, so one written to a log by mistake says nothing: the value is had by asking for Value.
type Secret[T any] struct {
	Value  T
	Hidden bool
}

func (s Secret[T]) MarshalJSON() ([]byte, error) {
	if s.Hidden {
		return json.Marshal(HiddenSecret)
	}

	return json.Marshal(s.Value)
}

func (s *Secret[T]) UnmarshalJSON(data []byte) error {
	var kept string
	if json.Unmarshal(data, &kept) == nil && kept == HiddenSecret {
		var none T
		s.Value, s.Hidden = none, true

		return nil
	}

	s.Hidden = false

	return json.Unmarshal(data, &s.Value)
}

func (Secret[T]) String() string { return hidden }

func (Secret[T]) GoString() string { return hidden }
