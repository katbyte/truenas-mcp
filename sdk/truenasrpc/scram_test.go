package truenasrpc

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
	"time"
)

// The newer login is arithmetic that either matches the NAS's to the last bit or does not log in at all, and a stand-in written by the same hand
// would share any mistake in it. These answers were worked out apart from this code, with Python's own hashing, for one fixed exchange each way.
func TestTheNewerLoginsMessagesAreTheOnesWorkedOutByHand(t *testing.T) {
	t.Parallel()

	clientNonce, serverNonce := make([]byte, 32), make([]byte, 32)
	for i := range 32 {
		clientNonce[i], serverNonce[i] = byte(i), byte(32+i)
	}
	shown := sha256.Sum256([]byte("the certificate the NAS showed"))
	const serverFirst = "r=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+Pw==,s=MDEyMzQ1Njc4OWFiY2RlZg==,i=50000"

	for _, tc := range []struct {
		name        string
		binding     []byte
		first       string
		final       string
		serverFinal string
	}{
		{
			name:        "tied to the certificate",
			binding:     shown[:],
			first:       "p=tls-server-end-point,,n=metrics:7,r=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
			final:       "c=cD10bHMtc2VydmVyLWVuZC1wb2ludCwslxkFSkFX1byT8l88uFsFyYk7AxyOyq35/UsFs82foHA=,r=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+Pw==,p=yjRid5octUexmRiXZPc6QSbRbAH+sv1b9qtDdIvtWQHBeLZSOwM+sHcRL04UGpbPZcpEOy87SwnvoRfKY1jfBA==",
			serverFinal: "v=xUemxDXNTv4Gqx+jwJG3v7rtQ5Kc0P5QpjaXRTeilfkvq73l0PMi+Lo8bWUCjAWx4LtdOGzC2wIXqQDppLnhvQ==",
		},
		{
			name:        "tied to nothing",
			first:       "n,,n=metrics:7,r=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
			final:       "c=biws,r=AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8gISIjJCUmJygpKissLS4vMDEyMzQ1Njc4OTo7PD0+Pw==,p=NwLpevVRIskNrbtKmvQkqOZhIV4HCU/RlPakMRdglm6/lGnlH1pYk+GKqGNUYiHI5GLiv2Q2ngBDhd9wh4DDnA==",
			serverFinal: "v=/5qjl4ORtbV5CM7jFStpk9k8YS2dgFrASvQCBPv2QerVKjSWNARhk9LyVxpHHSlWkORbjJ+qaErGDc3wjvngBA==",
		},
	} {
		x, err := newScramExchange("metrics", 7, tc.binding, clientNonce)
		if err != nil {
			t.Fatal(err)
		}
		if got := x.first(); got != tc.first {
			t.Errorf("%s: the first message is %s, want %s", tc.name, got, tc.first)
		}

		salt, rounds, err := x.challenged(serverFirst)
		if err != nil || string(salt) != "0123456789abcdef" || rounds != 50000 {
			t.Fatalf("%s: the NAS's challenge read as salt %q, %d rounds, %v", tc.name, salt, rounds, err)
		}

		keys, err := stretchedKeys("abcDEF123secretPART", salt, rounds)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := x.final(keys); err != nil || got != tc.final {
			t.Errorf("%s: the last message is %s (%v), want %s", tc.name, got, err, tc.final)
		}

		if err := x.verified(tc.serverFinal, keys); err != nil {
			t.Errorf("%s: the NAS's own proof was refused: %v", tc.name, err)
		}

		// a proof made for the other exchange, or by something that does not hold the key, is no proof
		if err := x.verified("v="+base64.StdEncoding.EncodeToString(mac([]byte("not the key"), x.signed())), keys); err == nil || !strings.Contains(err.Error(), "could not prove it holds the key") {
			t.Errorf("%s: a made-up proof was taken: %v", tc.name, err)
		}
	}
}

// What the NAS sends back is not to be trusted until it has proved itself, so each thing it can get wrong, or be made to get wrong by something in
// the middle, is turned away before any work is done on it: above all a count of rounds that would keep the caller busy for minutes.
func TestAChallengeThatIsNotOneIsTurnedAway(t *testing.T) {
	t.Parallel()

	nonce := make([]byte, 32)
	ours := base64.StdEncoding.EncodeToString(append(append([]byte{}, nonce...), make([]byte, 32)...))
	theirs := base64.StdEncoding.EncodeToString(append([]byte{1}, make([]byte, 63)...))

	for name, challenge := range map[string]string{
		"a nonce that does not start with the one sent": "r=" + theirs + ",s=c2FsdA==,i=50000",
		"a nonce cut short":                             "r=AAEC,s=c2FsdA==,i=50000",
		"too few rounds":                                "r=" + ours + ",s=c2FsdA==,i=4096",
		"rounds without end":                            "r=" + ours + ",s=c2FsdA==,i=2000000000",
		"no salt":                                       "r=" + ours + ",i=50000",
		"something else altogether":                     "e=other-error",
	} {
		x, err := newScramExchange("metrics", 7, nil, nonce)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := x.challenged(challenge); err == nil {
			t.Errorf("%s was taken as a challenge", name)
		}
	}

	if _, err := newScramExchange("name,with=marks", 7, nil, nil); err == nil {
		t.Error("a user name with a comma and an equals sign was put into a message they would break")
	}
}

// A login is tied to the NAS's certificate by a hash of it, and both ends must pick the same hash: the one the certificate was signed with. A
// certificate signed in a way that names no hash cannot be tied to, and saying so beats a login that fails for no visible reason.
func TestALoginIsTiedToACertificateByTheHashItWasSignedWith(t *testing.T) {
	t.Parallel()

	signed := func(key any, public any) *x509.Certificate {
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}

		return cert
	}

	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	edPublic, edPrivate, _ := ed25519.GenerateKey(rand.Reader)

	with256 := signed(p256, &p256.PublicKey)
	want256 := sha256.Sum256(with256.Raw)
	if got, err := endPointHash(with256); err != nil || !bytes.Equal(got, want256[:]) {
		t.Errorf("a certificate signed with SHA-256 is tied by %x (%v), want its SHA-256", got, err)
	}

	with384 := signed(p384, &p384.PublicKey)
	want384 := sha512.Sum384(with384.Raw)
	if got, err := endPointHash(with384); err != nil || !bytes.Equal(got, want384[:]) {
		t.Errorf("a certificate signed with SHA-384 is tied by %x (%v), want its SHA-384", got, err)
	}

	if _, err := endPointHash(signed(edPrivate, edPublic)); err == nil {
		t.Error("a certificate signed in a way that names no hash was tied to")
	}
}

// A key reaches a program as TrueNAS shows it, or as one of the key files TrueNAS hands out, and what is wrong with one is said without quoting it:
// whatever was given is a key, or was meant to be.
func TestAKeyIsReadInEveryFormTrueNASHandsOneOutIn(t *testing.T) {
	t.Parallel()

	const parts = `"api_key_id": 12, "client_key": "Y2xpZW50", "stored_key": "c3RvcmVk", "server_key": "c2VydmVy"`

	for name, text := range map[string]string{
		"the key as shown":           " 12-abcDEF123 \n",
		"a JSON file with the key":   `{"raw_key": "12-abcDEF123"}`,
		"an INI file with the key":   "# made by TrueNAS\n[TRUENAS_API_KEY]\nraw_key = 12-abcDEF123\n",
		"an INI file of one section": "[nas]\nRAW_KEY: 12-abcDEF123\n",
	} {
		k, err := readKey(text)
		if err != nil || k.raw != "12-abcDEF123" || k.secret != "abcDEF123" || k.id != 12 || k.worked() {
			t.Errorf("%s read as %+v (%v), want key 12 with its secret", name, k, err)
		}
	}

	for name, text := range map[string]string{
		"a JSON file of the worked-out parts": "{" + parts + "}",
		"an INI file of the worked-out parts": "[TRUENAS_API_KEY]\napi_key_id = 12\nclient_key = Y2xpZW50\nstored_key = c3RvcmVk\nserver_key = c2VydmVy\n",
	} {
		k, err := readKey(text)
		if err != nil || !k.worked() || k.id != 12 || string(k.keys.client) != "client" || string(k.keys.stored) != "stored" || string(k.keys.server) != "server" {
			t.Errorf("%s read as %+v (%v), want key 12 as its three parts", name, k, err)
		}
	}

	for name, text := range map[string]string{
		"nothing":                         "  ",
		"a key with no number":            "abcDEF123-a-secret-not-to-quote",
		"a file with neither":             `{"note": "a-secret-not-to-quote"}`,
		"parts with one missing":          `{"api_key_id": 12, "client_key": "Y2xpZW50", "stored_key": "c3RvcmVk"}`,
		"parts that are not base64":       `{"api_key_id": 12, "client_key": "a-secret-not-to-quote!", "stored_key": "c3RvcmVk", "server_key": "c2VydmVy"}`,
		"an INI file of several sections": "[one]\nraw_key = 1-a\n[two]\nraw_key = 2-b\n",
	} {
		_, err := readKey(text)
		if err == nil {
			t.Errorf("%s was read as a key", name)

			continue
		}
		if strings.Contains(err.Error(), "a-secret-not-to-quote") {
			t.Errorf("%s: the error quotes what was given: %v", name, err)
		}
	}
}
