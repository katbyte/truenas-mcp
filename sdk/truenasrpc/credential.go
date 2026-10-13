package truenasrpc

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Credential is how a connection logs in. APIKey, APIKeyFile, APIKeyOnly and Password make one.
type Credential struct {
	// login logs a newly opened connection in, in as many calls as the way of logging in takes
	login func(ctx context.Context, s *session) error
	// secrets are what the login holds that must never be written down: the key, its secret part, a password
	secrets []string
	// invalid is why the credential cannot be used, said when the connection is made
	invalid error
	// local says the login is the one a NAS gives a program running on it, which only its own socket can give
	local bool
}

// session is a newly opened connection as a login sees it.
type session struct {
	conn *Conn
	link *link
	// shown is the certificate the NAS showed, or nil where the connection has none
	shown *x509.Certificate
	// checked says whether anything vouches for that certificate: an authority, or its being the one the caller handed over
	checked bool
}

func (s *session) call(ctx context.Context, method string, params []any, into any) error {
	_, err := s.link.call(ctx, method, params, into, func() {})

	return err
}

// givenUp is a login this side would not or could not finish, which trying again will not change.
func (s *session) givenUp(why error) error {
	return &LoginError{Address: s.conn.address, Err: why}
}

// The names TrueNAS gives its ways of logging in, and its answers to a login.
const (
	mechanismSCRAM    = "SCRAM"
	mechanismKeyPlain = "API_KEY_PLAIN"
	loggedIn          = "SUCCESS"
	scramResponse     = "SCRAM_RESPONSE"
)

// LoginOption changes how a credential logs in.
type LoginOption func(*loginSettings)

type loginSettings struct {
	unbound       bool
	evenUnchecked bool
	otp           func() (string, error)
}

// WithoutChannelBinding logs in with the newer login without tying it to the certificate the NAS showed: for a NAS reached through something that
// shows a certificate of its own, such as a reverse proxy. The key is still never sent, but something in the middle could then pass the login along.
func WithoutChannelBinding() LoginOption {
	return func(l *loginSettings) { l.unbound = true }
}

// EvenUnchecked lets a key or a password be sent as it is to a NAS whose certificate nothing vouches for. Without it, a connection made with
// WithoutCertificateCheck logs in only where the NAS takes the newer login, which sends neither: otherwise whatever answers at the address is handed
// the key, and something in the middle of a newer NAS's connection could claim to be an older one to be handed it.
func EvenUnchecked() LoginOption {
	return func(l *loginSettings) { l.evenUnchecked = true }
}

// WithOTP answers a NAS that asks for a one-time password after the password, with what the function returns when it is asked. It is asked afresh
// at each login, since a one-time password is good for one: a program that stays up, and so logs in again when the NAS restarts, wants an API key.
func WithOTP(source func() (string, error)) LoginOption {
	return func(l *loginSettings) { l.otp = source }
}

func settingsOf(opts []LoginOption) loginSettings {
	var l loginSettings
	for _, opt := range opts {
		opt(&l)
	}

	return l
}

// APIKey logs in with an API key and the name of the user it was made for (Credentials, API Keys in the TrueNAS interface). The key is the key as
// TrueNAS shows it, or the contents of a key file.
//
// A TrueNAS from version 26 on is logged into without the key being sent at all, and the login is tied to the certificate the NAS showed, so that
// nothing in the middle can pass it along. An older one is sent the key, which is refused where nothing vouches for the NAS's certificate unless
// EvenUnchecked says otherwise.
func APIKey(username, key string, opts ...LoginOption) Credential {
	username = strings.TrimSpace(username)
	o := settingsOf(opts)

	material, err := readKey(key)
	cred := Credential{secrets: []string{material.raw, material.secret, strings.TrimSpace(key)}}

	switch {
	case err != nil:
		cred.invalid = err
	case username == "":
		cred.invalid = errors.New("the name of the user the TrueNAS API key was made for is required")
	case o.otp != nil:
		cred.invalid = errors.New("a one-time password goes with a password, not with an API key")
	}

	cred.login = func(ctx context.Context, s *session) error {
		offered, err := mechanisms(ctx, s)
		if err != nil {
			return err
		}
		if slices.Contains(offered, mechanismSCRAM) {
			return scramLogin(ctx, s, username, material, o.unbound)
		}

		if material.worked() {
			return s.givenUp(errors.New("it takes an API key only as it is, as every version before 26 does, and the key file holds only the parts worked out of one: it needs the key itself"))
		}
		if err := mayBeSent(s, o, "API key"); err != nil {
			return err
		}

		var answer loginAnswer
		if err := s.call(ctx, "auth.login_ex", []any{map[string]any{"mechanism": mechanismKeyPlain, "username": username, "api_key": material.raw, "login_options": map[string]any{"user_info": false}}}, &answer); err != nil {
			return err
		}

		return answer.taken(s)
	}

	return cred
}

// APIKeyFile is APIKey with the key read from a file: the key as TrueNAS shows it, or a key file as TrueNAS hands one out.
func APIKeyFile(username, path string, opts ...LoginOption) Credential {
	text, err := os.ReadFile(path) //nolint:gosec // G304: the file is one the person running the program names
	if err != nil {
		return Credential{invalid: fmt.Errorf("reading the TrueNAS API key file: %w", err)}
	}

	return APIKey(username, string(text), opts...)
}

// APIKeyOnly logs in with an API key alone, leaving the NAS to work out whose it is. The key is always sent, and TrueNAS is taking this login away
// in version 27: use APIKey where the user name is known.
func APIKeyOnly(key string, opts ...LoginOption) Credential {
	o := settingsOf(opts)

	material, err := readKey(key)
	cred := Credential{secrets: []string{material.raw, material.secret, strings.TrimSpace(key)}}

	switch {
	case err != nil:
		cred.invalid = err
	case material.worked():
		cred.invalid = errors.New("a key file holding only the parts worked out of a key needs the name of the user the key was made for")
	case o.otp != nil:
		cred.invalid = errors.New("a one-time password goes with a password, not with an API key")
	}

	cred.login = func(ctx context.Context, s *session) error {
		if err := mayBeSent(s, o, "API key"); err != nil {
			return err
		}

		var ok bool
		if err := s.call(ctx, "auth.login_with_api_key", []any{material.raw}, &ok); err != nil {
			return err
		}
		if !ok {
			return &LoginError{Address: s.conn.address, Response: "AUTH_ERR"}
		}

		return nil
	}

	return cred
}

// Password logs in as a user with their password, and with a one-time password too where the account has two-factor login and WithOTP says where to
// get one. The password is sent as it is, which is refused where nothing vouches for the NAS's certificate unless EvenUnchecked says otherwise.
func Password(username, password string, opts ...LoginOption) Credential {
	username = strings.TrimSpace(username)
	o := settingsOf(opts)

	cred := Credential{secrets: []string{password}}
	switch {
	case username == "":
		cred.invalid = errors.New("the TrueNAS user name is required")
	case password == "":
		cred.invalid = errors.New("the TrueNAS password is required")
	}

	cred.login = func(ctx context.Context, s *session) error {
		if err := mayBeSent(s, o, "password"); err != nil {
			return err
		}

		var answer loginAnswer
		if err := s.call(ctx, "auth.login_ex", []any{map[string]any{"mechanism": "PASSWORD_PLAIN", "username": username, "password": password, "login_options": map[string]any{"user_info": false}}}, &answer); err != nil {
			return err
		}
		if answer.ResponseType != "OTP_REQUIRED" || o.otp == nil {
			return answer.taken(s)
		}

		token, err := o.otp()
		if err != nil {
			return s.givenUp(fmt.Errorf("getting the one-time password it asked for: %w", err))
		}

		var second loginAnswer
		if err := s.call(ctx, "auth.login_ex_continue", []any{map[string]any{"mechanism": "OTP_TOKEN", "otp_token": token, "login_options": map[string]any{"user_info": false}}}, &second); err != nil {
			return err
		}

		return second.taken(s)
	}

	return cred
}

// LocalUser logs in as whoever the program runs as: how a TrueNAS takes a connection made over its own socket, by a program running on the NAS, an
// address that starts unix://. Nothing is sent, since the NAS can see who is at the other end: root, or a user it has an account for.
func LocalUser() Credential {
	return Credential{local: true, login: func(ctx context.Context, s *session) error {
		// the NAS logged the connection in as it opened, or did not: asking who is logged in finds out which, here and not at the first call
		err := s.call(ctx, "auth.me", nil, nil)
		if refusal, ok := errors.AsType[*Error](err); ok && refusal.ErrName == "ENOTAUTHENTICATED" {
			return s.givenUp(errors.New("it does not know the user this program runs as: over its own socket a TrueNAS takes root, or a user it has an account for"))
		}

		return err
	}}
}

// loginAnswer is what a TrueNAS answers a login with.
type loginAnswer struct {
	ResponseType string   `json:"response_type"`
	ScramType    string   `json:"scram_type"`
	RFC          string   `json:"rfc_str"`
	URLs         []string `json:"urls"`
}

// taken is nil for a login the NAS took, and its word for why not otherwise.
func (a loginAnswer) taken(s *session) error {
	if a.ResponseType == loggedIn {
		return nil
	}

	response := a.ResponseType
	if response == "" {
		response = "an answer that is not a login's"
	}

	return &LoginError{Address: s.conn.address, Response: response, URLs: a.URLs}
}

// mayBeSent refuses to send a secret as it is to a NAS whose certificate nothing vouches for, unless the caller has said it may be.
func mayBeSent(s *session, o loginSettings, what string) error {
	if s.checked || o.evenUnchecked {
		return nil
	}

	return s.givenUp(fmt.Errorf("it wants the %s sent as it is, and %w, so the %s would go to whatever answers at the address: hand over the NAS's certificate so that it is checked, or say the %s may be sent anyway", what, ErrNothingVouches, what, what))
}

// mechanisms is the ways of logging in a NAS says it takes. One that cannot say, because its version has no such call or has it broken (25.04.2 to
// 25.10.2), takes a key only as it is.
func mechanisms(ctx context.Context, s *session) ([]string, error) {
	var offered []string
	err := s.call(ctx, "auth.mechanism_choices", nil, &offered)
	if err == nil {
		return offered, nil
	}

	if refusal, ok := errors.AsType[*Error](err); ok && (refusal.Code == CodeNoSuchMethod || strings.Contains(refusal.Reason, "may_create_auth_token")) {
		return []string{mechanismKeyPlain}, nil
	}

	return nil, err
}

// scramLogin logs in without sending the key: the caller proves it holds the key, and then the NAS proves it does too.
func scramLogin(ctx context.Context, s *session, username string, key keyMaterial, unbound bool) error {
	var binding []byte
	if s.shown != nil && !unbound {
		var err error
		if binding, err = endPointHash(s.shown); err != nil {
			return s.givenUp(err)
		}
	}

	exchange, err := newScramExchange(username, key.id, binding, nil)
	if err != nil {
		return s.givenUp(err)
	}

	var challenge loginAnswer
	if err := s.call(ctx, "auth.login_ex", []any{map[string]any{"mechanism": mechanismSCRAM, "scram_type": "CLIENT_FIRST_MESSAGE", "rfc_str": exchange.first()}}, &challenge); err != nil {
		return err
	}
	if challenge.ResponseType != scramResponse || challenge.ScramType != "SERVER_FIRST_RESPONSE" {
		return challenge.taken(s)
	}

	salt, rounds, err := exchange.challenged(challenge.RFC)
	if err != nil {
		return s.givenUp(err)
	}

	keys := key.keys
	if !key.worked() {
		if keys, err = stretchedKeys(key.secret, salt, rounds); err != nil {
			return s.givenUp(err)
		}
	}

	final, err := exchange.final(keys)
	if err != nil {
		return s.givenUp(err)
	}

	var proof loginAnswer
	if err := s.call(ctx, "auth.login_ex", []any{map[string]any{"mechanism": mechanismSCRAM, "scram_type": "CLIENT_FINAL_MESSAGE", "rfc_str": final}}, &proof); err != nil {
		return err
	}
	if proof.ResponseType != scramResponse || proof.ScramType != "SERVER_FINAL_RESPONSE" {
		err := proof.taken(s)
		if refusal, ok := errors.AsType[*LoginError](err); ok {
			refusal.Tied = binding != nil
		}

		return err
	}

	// the NAS has let the connection in; whether it is the NAS is only known once it has proved it holds the key too
	if err := exchange.verified(proof.RFC, keys); err != nil {
		return s.givenUp(err)
	}

	return nil
}
