package truenasrpc

import "time"

// WithStartGrace sets how long a call that answered a number is given to turn out to be a job, so that a test of one that never does need not wait
// out the two seconds a program gives it.
func WithStartGrace(wait time.Duration) Option {
	return func(s *settings) error {
		s.startGrace = wait

		return nil
	}
}
