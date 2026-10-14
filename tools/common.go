package tools

import (
	"reflect"
	"strconv"
	"time"
)

// Instructions are what a client is told on connecting, after what go-kt says of every server: the rules that hold between these tools, said once so
// that no tool's description has to.
var Instructions = []string{
	"Every list takes limit (100 unless given, 1000 at most) and offset, and answers total, offset and, while there is another page, the next_offset to pass back as offset.",
	"A pool is named by its name or its number, a dataset by its whole name (tank/media/films), and a disk by its name (sda) or serial number.",
	"Sizes are in bytes and times in RFC 3339, in UTC. A pool's status stays ONLINE through damage ZFS can still read past: go by healthy, and by each disk's state and error counts.",
}

const (
	// defaultLimit is a page of a list asked for with no limit.
	defaultLimit = 100
	// maxLimit caps a page: a limit of a million is one call reading a whole NAS into a client's context.
	maxLimit = 1000
)

// pageIn is how every list is paged: the same limit and offset whatever is listed, so a client that has paged one list can page them all.
type pageIn struct {
	Limit  int `json:"limit,omitempty"  jsonschema:"rows to return, default 100, at most 1000"`
	Offset int `json:"offset,omitempty" jsonschema:"rows to skip: a previous page's next_offset"`
}

// pageOut is what every list answers beside its rows: how many there are in all, where this page starts, and where the next one does.
type pageOut struct {
	Total      int `json:"total"                 jsonschema:"rows there are in all, of which this page is a slice"`
	Offset     int `json:"offset"                jsonschema:"where this page starts"`
	NextOffset int `json:"next_offset,omitempty" jsonschema:"pass back as offset for the next page; absent on the last"`
}

// args settles the limit and offset the way every list takes them: the default when no limit is given, never more than maxLimit, never before the
// start.
func (p pageIn) args() (limit, offset int) {
	limit = p.Limit
	if limit <= 0 {
		limit = defaultLimit
	}

	return min(limit, maxLimit), max(p.Offset, 0)
}

// pageRows is one page of a list held whole: at most limit rows from offset, and where they sit in the whole.
func pageRows[T any](all []T, in pageIn) ([]T, pageOut) {
	limit, offset := in.args()
	from := min(offset, len(all))
	rows := all[from:min(from+limit, len(all))]

	return rows, paged(offset, len(rows), len(all))
}

// paged is where a page of n rows from offset sits in a list of total rows.
func paged(offset, n, total int) pageOut {
	out := pageOut{Total: total, Offset: offset}
	if n > 0 && offset+n < total {
		out.NextOffset = offset + n
	}

	return out
}

// when is a moment as an answer gives one, RFC 3339 in UTC, or "" for none.
func when(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return t.UTC().Format(time.RFC3339)
}

// property is the raw value of one of a dataset's ZFS properties, which the client holds each as a record of its own type with the same fields: ""
// when the dataset has no such property.
func property(p any) string {
	v := reflect.ValueOf(p)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return ""
	}
	raw := v.Elem().FieldByName("Rawvalue")
	if !raw.IsValid() || raw.Kind() != reflect.String {
		return ""
	}

	return raw.String()
}

// bytesOf is a size property as a number of bytes: 0 for none, as ZFS writes a quota that is not set.
func bytesOf(p any) int64 {
	n, err := strconv.ParseInt(property(p), 10, 64)
	if err != nil {
		return 0
	}

	return n
}
