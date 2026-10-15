package truenasrpc

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/katbyte/go-kt/httpc"
)

// What a connection does when it is told nothing else.
const (
	// DefaultDialWait is how long a NAS has to take the connection and the login.
	DefaultDialWait = 20 * time.Second
	// DefaultCallWait is how long a call may take when its context sets no deadline. A call that runs for longer, a scrub or a replication, is given
	// a context that says how long.
	DefaultCallWait = 2 * time.Minute
	// DefaultPingEvery is how often an idle connection is asked whether it is still there, and DefaultPingWait how long it has to say so.
	DefaultPingEvery = 30 * time.Second
	DefaultPingWait  = 10 * time.Second
	// DefaultMaxCalls is how many calls are sent at once. A TrueNAS runs ten for a connection, holds ten more waiting and refuses the rest.
	DefaultMaxCalls = 10
	// DefaultMaxAnswer is the longest answer held: a list of every dataset and snapshot on a large NAS runs to megabytes.
	DefaultMaxAnswer = 64 << 20
	// MaxRedialWait is the longest a NAS that cannot be reached is left alone between tries.
	MaxRedialWait = 30 * time.Second
)

// Option changes how a connection is made.
type Option func(*settings) error

// settings are what a connection is made with.
type settings struct {
	log       httpc.Logger
	roots     *x509.CertPool
	unchecked bool
	// keys are the public keys of the certificates the NAS is to be known by, each as a certificate writes it down
	keys        [][]byte
	userAgent   string
	dialWait    time.Duration
	callWait    time.Duration
	pingEvery   time.Duration
	pingWait    time.Duration
	maxCalls    int
	maxAnswer   int64
	redialWait  func(failures int) time.Duration
	secretNames []string
	transport   http.RoundTripper
	private     bool
	startGrace  time.Duration
}

func defaults() settings {
	return settings{
		userAgent:  "truenas-mcp",
		dialWait:   DefaultDialWait,
		callWait:   DefaultCallWait,
		pingEvery:  DefaultPingEvery,
		pingWait:   DefaultPingWait,
		maxCalls:   DefaultMaxCalls,
		maxAnswer:  DefaultMaxAnswer,
		redialWait: redialWait,
		startGrace: startGrace,
	}
}

// redialWait is how long a NAS is left alone after it could not be reached, or would not take the login, so many times running: a second, then
// two, then four, up to MaxRedialWait. A NAS limits how often an address may try to log in, so asking again at once only makes it refuse for longer.
func redialWait(failures int) time.Duration {
	wait := time.Second
	for range failures - 1 {
		if wait *= 2; wait >= MaxRedialWait {
			return MaxRedialWait
		}
	}

	return wait
}

// WithLog sends what the connection does to a log: dialling and losing the connection at debug, each message at trace with its secrets hidden. A
// logrus logger is one as it stands.
func WithLog(log httpc.Logger) Option {
	return func(s *settings) error {
		s.log = log

		return nil
	}
}

// WithCAFile trusts the certificate authorities in a PEM file as well as the system's own: for a NAS given a certificate made out to the name it is
// dialled by. A NAS still on the certificate it came with wants WithCertificateFile.
func WithCAFile(path string) Option {
	return func(s *settings) error {
		authorities, err := os.ReadFile(path) //nolint:gosec // G304: the file is one the person running the program names
		if err != nil {
			return fmt.Errorf("reading the certificate authority file: %w", err)
		}

		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(authorities) {
			return fmt.Errorf("the certificate authority file %s holds no PEM certificate", path)
		}
		s.roots = roots

		return nil
	}
}

// WithCertificateFile takes the NAS's own certificate from a PEM file, and accepts a NAS that proves it holds that certificate's key whatever name
// and dates are on the certificate it shows. It is the setting for a TrueNAS as it comes, whose certificate is made out to "localhost" and so can
// never be checked by name. A certificate renewed with the same key goes on being accepted, and so does one that has run out; one made with a new
// key is refused until the file is replaced.
func WithCertificateFile(path string) Option {
	return func(s *settings) error {
		rest, err := os.ReadFile(path) //nolint:gosec // G304: the file is one the person running the program names
		if err != nil {
			return fmt.Errorf("reading the TrueNAS's certificate file: %w", err)
		}

		var certs []*x509.Certificate
		for {
			var block *pem.Block
			if block, rest = pem.Decode(rest); block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}

			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return fmt.Errorf("the certificate file %s holds a certificate that cannot be read: %w", path, err)
			}
			certs = append(certs, cert)
		}
		if len(certs) == 0 {
			return fmt.Errorf("the certificate file %s holds no PEM certificate", path)
		}

		return WithCertificate(certs...)(s)
	}
}

// WithCertificate is WithCertificateFile for a program that holds the NAS's certificate already.
func WithCertificate(certs ...*x509.Certificate) Option {
	return func(s *settings) error {
		for _, cert := range certs {
			if cert == nil {
				return errors.New("the TrueNAS's certificate is missing")
			}
			s.keys = append(s.keys, cert.RawSubjectPublicKeyInfo)
		}

		return nil
	}
}

// errNotTheCertificate is a NAS, or something answering in its place, that showed a certificate with another key than the one it is known by.
var errNotTheCertificate = errors.New("it shows a certificate whose key is not the one it is known by: if the NAS's certificate was replaced, fetch the new one; if it was not, something else is answering at this address")

// holdsKey is the check a connection makes of the certificate a NAS shows when it is known by its key: that the key is one of those it was given.
// The NAS has proved it holds the key by the time this is asked, so nothing else on the certificate needs to be believed.
func holdsKey(keys [][]byte) func(tls.ConnectionState) error {
	return func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("it showed no certificate")
		}

		for _, key := range keys {
			if bytes.Equal(key, state.PeerCertificates[0].RawSubjectPublicKeyInfo) {
				return nil
			}
		}

		return errNotTheCertificate
	}
}

// trust is how the NAS's certificate is checked, as the settings say: by its key, not at all, or by name against the authorities given or the
// system's own. Saying more than one way is refused, since the one that won would be a guess.
func (s *settings) trust() (*tls.Config, error) {
	ways := 0
	for _, said := range []bool{len(s.keys) > 0, s.unchecked, s.roots != nil} {
		if said {
			ways++
		}
	}
	if ways > 1 {
		return nil, errors.New("more than one way of trusting the TrueNAS's certificate was given: give its own certificate, or the authority that signed it, or say it is not to be checked")
	}

	cfg := &tls.Config{RootCAs: s.roots, MinVersion: tls.VersionTLS12}
	switch {
	case len(s.keys) > 0:
		// the usual check is replaced, not dropped: holdsKey is asked on every connection, new or resumed
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = holdsKey(s.keys)
	case s.unchecked:
		// only when WithoutCertificateCheck asks, for a NAS whose certificate nothing vouches for
		cfg.InsecureSkipVerify = true
	}

	return cfg, nil
}

// WithRootCAs trusts these certificate authorities and no others.
func WithRootCAs(roots *x509.CertPool) Option {
	return func(s *settings) error {
		s.roots = roots

		return nil
	}
}

// WithoutCertificateCheck takes whatever certificate the NAS shows. The connection is still encrypted, but nothing says it is the NAS at the other
// end, so the key can be handed to whoever answers: prefer WithCertificateFile.
func WithoutCertificateCheck() Option {
	return func(s *settings) error {
		s.unchecked = true

		return nil
	}
}

// WithUserAgent names the program to the NAS, "truenas-exporter/1.2.0".
func WithUserAgent(name string) Option {
	return func(s *settings) error {
		s.userAgent = name

		return nil
	}
}

// WithDialWait sets how long a NAS has to take the connection and the login, DefaultDialWait unless set.
func WithDialWait(wait time.Duration) Option {
	return func(s *settings) error {
		if wait <= 0 {
			return errors.New("the dial wait must be more than nothing")
		}
		s.dialWait = wait

		return nil
	}
}

// WithCallWait sets how long a call may take when its context sets no deadline, DefaultCallWait unless set. Zero is no limit but the context's.
func WithCallWait(wait time.Duration) Option {
	return func(s *settings) error {
		if wait < 0 {
			return errors.New("the call wait cannot be less than nothing")
		}
		s.callWait = wait

		return nil
	}
}

// WithPing sets how often an idle connection is asked whether it is still there and how long it has to answer. A connection that does not is
// dropped, and the next call dials again.
func WithPing(every, wait time.Duration) Option {
	return func(s *settings) error {
		if every <= 0 || wait <= 0 {
			return errors.New("the ping interval and wait must be more than nothing")
		}
		s.pingEvery, s.pingWait = every, wait

		return nil
	}
}

// WithMaxCalls sets how many calls are sent at once, DefaultMaxCalls unless set. The NAS refuses the twenty-first, so more than twenty is refused
// here.
func WithMaxCalls(n int) Option {
	return func(s *settings) error {
		if n < 1 || n > 20 {
			return errors.New("between 1 and 20 calls may be sent at once: a TrueNAS refuses the twenty-first")
		}
		s.maxCalls = n

		return nil
	}
}

// WithMaxAnswer sets the longest answer held, in bytes, DefaultMaxAnswer unless set. A longer one ends the connection.
func WithMaxAnswer(limit int64) Option {
	return func(s *settings) error {
		if limit < 1 {
			return errors.New("the longest answer must be at least a byte")
		}
		s.maxAnswer = limit

		return nil
	}
}

// WithRedialWait sets how long a NAS is left alone after failing so many times running, in place of a second doubling up to MaxRedialWait.
func WithRedialWait(wait func(failures int) time.Duration) Option {
	return func(s *settings) error {
		s.redialWait = wait

		return nil
	}
}

// WithSecretNames names more fields whose values a trace must not show, beside those hidden unasked.
func WithSecretNames(names ...string) Option {
	return func(s *settings) error {
		s.secretNames = append(s.secretNames, names...)

		return nil
	}
}

// WithPrivateMethods tells the NAS the connection means to call methods TrueNAS keeps for itself, which are no part of the API it describes and
// change without notice. A NAS takes such calls either way at present, and writes a warning for each one made without this.
func WithPrivateMethods() Option {
	return func(s *settings) error {
		s.private = true

		return nil
	}
}

// WithTransport sends the request that opens the connection through this in place of the default: for a proxy, or a test. How the NAS's certificate
// is checked is then the transport's own business, and the settings for it here are not used. A transport is taken to check it unless it is an
// *http.Transport told to skip the check, which is treated as WithoutCertificateCheck is: an older NAS is not sent the key.
func WithTransport(transport http.RoundTripper) Option {
	return func(s *settings) error {
		s.transport = transport

		return nil
	}
}
