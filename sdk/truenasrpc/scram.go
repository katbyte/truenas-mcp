package truenasrpc

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

// The login a TrueNAS takes from version 26 on proves the key to the NAS, and the NAS to the caller, without the key ever being sent: SCRAM (RFC
// 5802) over SHA-512, as TrueNAS words it. The messages are the RFC's, with two things of TrueNAS's own: the number of the key rides beside the
// user's name, since a user may have several, and a nonce is thirty-two random bytes written as base64.
const (
	scramNonceSize = 32
	// what a NAS may ask for the key to be stretched by: fewer rounds would make a stolen exchange cheap to guess from, and more is a NAS, or
	// something posing as one, keeping the caller busy
	scramMinRounds = 50_000
	scramMaxRounds = 5_000_000
	scramMaxSalt   = 1024
	// the name of the one way TrueNAS ties the exchange to the connection it runs over (RFC 5929): by the certificate the NAS showed
	scramEndPoint = "tls-server-end-point"
)

// scramKeys are what an exchange proves and checks with, worked out from the key and what the NAS says it was stretched with, or handed over ready
// in a key file.
type scramKeys struct {
	client []byte
	stored []byte
	server []byte
}

// stretchedKeys works the three keys out of a key's secret part, the salt and the number of rounds the NAS names.
func stretchedKeys(secret string, salt []byte, rounds int) (scramKeys, error) {
	salted, err := pbkdf2.Key(sha512.New, secret, salt, rounds, sha512.Size)
	if err != nil {
		return scramKeys{}, err
	}

	client := mac(salted, "Client Key")
	stored := sha512.Sum512(client)

	return scramKeys{client: client, stored: stored[:], server: mac(salted, "Server Key")}, nil
}

func mac(key []byte, message string) []byte {
	h := hmac.New(sha512.New, key)
	h.Write([]byte(message))

	return h.Sum(nil)
}

// scramExchange is one login in progress: what was sent first, what came back, and what both sides sign.
type scramExchange struct {
	// header is "n" for an exchange tied to nothing, or "p=tls-server-end-point" for one tied to the NAS's certificate
	header string
	// binding is what ties it: the certificate's hash, or nothing
	binding []byte
	nonce   []byte
	// bare is the first message without its header, and serverFirst the NAS's answer to it as it was sent: both are signed
	bare        string
	serverFirst string
	combined    []byte
}

// newScramExchange starts a login for a user and the number of one of their keys, tied to the certificate hash given, if any. nonce is thirty-two
// random bytes, or nil for new ones.
func newScramExchange(username string, keyID int, binding, nonce []byte) (*scramExchange, error) {
	// a name is sent as it is: a comma or an equals sign would be read as part of the message, and anything outside plain printable text would
	// need preparing in a way no NAS user name calls for
	for _, r := range username {
		if r == ',' || r == '=' || r < 0x21 || r > 0x7e {
			return nil, fmt.Errorf("the user name %q cannot be used with this login: it has a character other than plain letters, digits and marks, or a comma or an equals sign", username)
		}
	}
	if username == "" {
		return nil, errors.New("the name of the user the TrueNAS API key was made for is required")
	}

	if nonce == nil {
		nonce = make([]byte, scramNonceSize)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
	}

	x := &scramExchange{header: "n", binding: binding, nonce: nonce}
	if binding != nil {
		x.header = "p=" + scramEndPoint
	}

	name := username
	if keyID != 0 {
		name += ":" + strconv.Itoa(keyID)
	}
	x.bare = "n=" + name + ",r=" + base64.StdEncoding.EncodeToString(nonce)

	return x, nil
}

// first is the message that opens the exchange.
func (x *scramExchange) first() string { return x.header + ",," + x.bare }

// challenged reads the NAS's answer to the first message: the nonce with its own half added, and the salt and rounds the key was stretched with.
func (x *scramExchange) challenged(serverFirst string) (salt []byte, rounds int, err error) {
	parts := map[string]string{}
	for part := range strings.SplitSeq(serverFirst, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || value == "" {
			return nil, 0, errors.New("it answered the login with something that is not the exchange's next message")
		}
		parts[key] = value
	}
	if len(parts) != 3 || parts["r"] == "" || parts["s"] == "" || parts["i"] == "" {
		return nil, 0, errors.New("it answered the login with something that is not the exchange's next message")
	}

	combined, err := base64.StdEncoding.DecodeString(parts["r"])
	if err != nil || len(combined) != 2*scramNonceSize || !bytes.HasPrefix(combined, x.nonce) {
		return nil, 0, errors.New("it answered the login with a nonce that is not the one it was sent with its own added: the answer is not to this login")
	}
	salt, err = base64.StdEncoding.DecodeString(parts["s"])
	if err != nil || len(salt) == 0 || len(salt) > scramMaxSalt {
		return nil, 0, errors.New("it answered the login with a salt that cannot be used")
	}
	rounds, err = strconv.Atoi(parts["i"])
	if err != nil || rounds < scramMinRounds || rounds > scramMaxRounds {
		return nil, 0, fmt.Errorf("it asked for the key to be stretched %s times, outside the %d to %d a NAS asks for", parts["i"], scramMinRounds, scramMaxRounds)
	}

	x.serverFirst, x.combined = serverFirst, combined

	return salt, rounds, nil
}

// unproven is the last message without its proof, which is the third thing both sides sign.
func (x *scramExchange) unproven() string {
	return "c=" + base64.StdEncoding.EncodeToString(append([]byte(x.header+",,"), x.binding...)) + ",r=" + base64.StdEncoding.EncodeToString(x.combined)
}

// signed is what both sides sign: the three messages that carry everything either of them chose.
func (x *scramExchange) signed() string { return x.bare + "," + x.serverFirst + "," + x.unproven() }

// final is the message that proves the caller holds the key, without saying what it is.
func (x *scramExchange) final(keys scramKeys) (string, error) {
	signature := mac(keys.stored, x.signed())
	if len(keys.client) != len(signature) {
		return "", errors.New("the key's worked-out parts are not the length this login uses")
	}

	proof := make([]byte, len(signature))
	subtle.XORBytes(proof, keys.client, signature)

	return x.unproven() + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

// verified checks the NAS's last message, which proves it holds the key too: without that, anything could have answered and said yes.
func (x *scramExchange) verified(serverFinal string, keys scramKeys) error {
	sent, ok := strings.CutPrefix(serverFinal, "v=")
	signature, err := base64.StdEncoding.DecodeString(sent)
	if !ok || err != nil || len(signature) == 0 {
		return errors.New("it ended the login with something that is not its proof")
	}
	if !hmac.Equal(signature, mac(keys.server, x.signed())) {
		return errors.New("what answered could not prove it holds the key, so it is not the TrueNAS the key was made on")
	}

	return nil
}

// endPointHash is what ties an exchange to the connection it runs over: the certificate the NAS showed, hashed the way the certificate itself was
// signed, with the two old ways that are no longer trusted taken up to SHA-256 (RFC 5929). Something in the middle showing a certificate of its own
// cannot pass the exchange on, since the NAS signs the hash of the one it showed.
func endPointHash(cert *x509.Certificate) ([]byte, error) {
	var h hash.Hash
	switch cert.SignatureAlgorithm {
	case x509.MD5WithRSA, x509.SHA1WithRSA, x509.DSAWithSHA1, x509.ECDSAWithSHA1, x509.SHA256WithRSA, x509.SHA256WithRSAPSS, x509.DSAWithSHA256, x509.ECDSAWithSHA256:
		h = sha256.New()
	case x509.SHA384WithRSA, x509.SHA384WithRSAPSS, x509.ECDSAWithSHA384:
		h = sha512.New384()
	case x509.SHA512WithRSA, x509.SHA512WithRSAPSS, x509.ECDSAWithSHA512:
		h = sha512.New()
	default:
		return nil, fmt.Errorf("its certificate is signed in a way (%v) that a login cannot be tied to", cert.SignatureAlgorithm)
	}
	h.Write(cert.Raw)

	return h.Sum(nil), nil
}
