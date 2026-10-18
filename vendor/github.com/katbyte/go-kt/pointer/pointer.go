// Package pointer reads and makes the optional fields API models type as *T, where a statement is awkward: inside a literal, an argument or a return.
package pointer

// From is the value behind p, or the zero value when there is none: an optional field read without a nil check at every site.
func From[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// To is a pointer to a copy of v. new(v) does the same since Go 1.26; To stays for a call that reads better named.
func To[T any](v T) *T {
	return new(v)
}
