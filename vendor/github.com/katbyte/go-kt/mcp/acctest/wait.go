package acctest

import "time"

// waitPoll is how often a wait looks again.
const waitPoll = 500 * time.Millisecond

// Holds reports whether check stays true for a few seconds, long enough for a background refresh that would undo something to show.
func Holds(check func() bool) bool {
	for range 10 {
		if !check() {
			return false
		}
		time.Sleep(waitPoll)
	}

	return true
}

// Eventually reports whether check comes true within about twenty seconds.
func Eventually(check func() bool) bool {
	return EventuallyWithin(20*time.Second, check)
}

// EventuallyWithin is Eventually with its own patience, for work queued behind a server's other work.
func EventuallyWithin(patience time.Duration, check func() bool) bool {
	for range int(patience / waitPoll) {
		if check() {
			return true
		}
		time.Sleep(waitPoll)
	}

	return false
}
