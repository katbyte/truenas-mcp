package client

// Where is one condition of a query's filters: a field, how it is compared as the server spells it (=, !=, >, ~, in and the rest)
// and the value compared with: ["name", "=", "tank"].
func Where(field, compare string, value any) []any {
	return []any{field, compare, value}
}

// Filters is a query's filters from conditions a record must all meet, written out as the list of lists the server reads;
// none is nil, which goes out as an empty list.
func Filters(conditions ...[]any) []any {
	if len(conditions) == 0 {
		return nil
	}
	out := make([]any, 0, len(conditions))
	for _, c := range conditions {
		out = append(out, c)
	}

	return out
}

// AnyOf is one condition a record meets by meeting any of the conditions given, as the server takes it: ["OR", [...]].
func AnyOf(conditions ...[]any) []any {
	return []any{"OR", Filters(conditions...)}
}
