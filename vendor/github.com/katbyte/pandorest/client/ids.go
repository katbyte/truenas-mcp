package client

import (
	"fmt"
	"strings"
)

// IDs turns a list of plain numbers into a list of a kind of id, which Go will not convert as it
// converts one value: client.IDs[sonarr.SeriesID](ints).
func IDs[T ~int | ~int64, F ~int | ~int64](from []F) []T {
	if from == nil {
		return nil
	}
	out := make([]T, len(from))
	for i, v := range from {
		out[i] = T(v)
	}

	return out
}

// StringIDs is IDs for a kind of id over a string: client.StringIDs[emby.ItemID](strings).
func StringIDs[T ~string, F ~string](from []F) []T {
	if from == nil {
		return nil
	}
	out := make([]T, len(from))
	for i, v := range from {
		out[i] = T(v)
	}

	return out
}

// Joined is a list of ids as one comma-joined query value, which is how a server that takes several
// in one parameter (Emby's Ids) reads them.
func Joined[T ~int | ~int64 | ~string](ids []T) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}

	return strings.Join(parts, ",")
}
