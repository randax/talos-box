package daemon

// Unit picks the singular or plural form for count. Warnings, refusals and
// summaries the operator reads should inflect — "1 node removed", not
// "1 node(s) removed" — and every one of them needs the same two-line rule, so
// it lives here rather than in each of them.
func Unit(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
