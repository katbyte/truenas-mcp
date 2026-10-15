package registry

import "time"

// LimitOr is the page size a call asked for, or def when it asked for none.
func LimitOr(limit, def int) int {
	if limit <= 0 {
		return def
	}

	return limit
}

// NextOffset is where the page after one of n rows from offset starts, out of total, or 0 when that page was the last.
func NextOffset(offset, n, total int) int {
	if n == 0 || offset+n >= total {
		return 0
	}

	return offset + n
}

// WaitFor is how long a call asked to wait for a job it started, in seconds: def when it did not say, and no wait at all when it asked for less
// than none.
func WaitFor(seconds int, def time.Duration) time.Duration {
	switch {
	case seconds < 0:
		return 0
	case seconds == 0:
		return def
	}

	return time.Duration(seconds) * time.Second
}
