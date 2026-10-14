//go:build acceptance

package truenasrpc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// signed makes a certificate that vouches for itself, good between two moments, with its key: what a NAS is handed when a certificate made
// somewhere else is brought to it.
func signed(t *testing.T, name string, from, until time.Time) (certificate, key string) {
	t.Helper()

	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name, Organization: []string{"made by the tests"}}, DNSNames: []string{name}, NotBefore: from, NotAfter: until, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	held, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}

	key = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: held}))
	// no line of trace may hold the key from here on
	if lines := strings.Split(strings.TrimSpace(key), "\n"); len(lines) > 2 {
		ledger.hide(lines[1])
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), key
}

// certified is a certificate as a NAS lists it, as far as a check of certificates reads one.
type certified struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"cert_type"`
	Request   bool     `json:"cert_type_CSR"`
	Parsed    bool     `json:"parsed"`
	Expired   *bool    `json:"expired"`
	From      *string  `json:"from"`
	Until     *string  `json:"until"`
	Lifetime  *int     `json:"lifetime"`
	Common    *string  `json:"common"`
	Names     []string `json:"san"`
	KeyType   *string  `json:"key_type"`
	Key       *string  `json:"privatekey"`
	RenewDays *int     `json:"renew_days"`
}

// A NAS reads a certificate's dates the way a clock on the wall is read, with no word of where the clock is: they are in UTC.
const certificateTime = "Mon Jan _2 15:04:05 2006"

// A certificate made somewhere else is brought to the NAS, three times over: one that ran out yesterday, one that runs out in five days, and
// one good for a year. Each is listed with when it runs out and whether it has, which is what a check of certificates reads: a NAS looks at
// its certificates itself only once a day, at midnight, so an alert is not to be waited for. The key is kept back from a user who may only
// read. A certificate that has run out is not taken as the one the NAS shows its own pages with. A request for a certificate is made on the
// NAS, and one certificate is renamed. Each is removed.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestACertificateSaysWhenItRunsOutAndOneThatHasIsNotTakenForTheNASItself(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	// the certificate the NAS shows its own pages with, to put back if a test of what it refuses is not refused
	var shown struct {
		Certificate struct {
			ID int64 `json:"id"`
		} `json:"ui_certificate"`
	}
	if err := conn.Call(ctx, "system.general.config", nil, &shown); err != nil || shown.Certificate.ID == 0 {
		t.Fatalf("reading which certificate the NAS shows its pages with: %d, %v", shown.Certificate.ID, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	bring := func(name string, from, until time.Time) certified {
		certificate, key := signed(t, name+".tests.example", from, until)

		var made certified
		if err := conn.Call(ctx, "certificate.create", []any{map[string]any{"name": name, "create_type": "CERTIFICATE_CREATE_IMPORTED", "certificate": certificate, "privatekey": key}}, &made); err != nil {
			t.Fatalf("bringing the certificate %s to the NAS: %v", name, err)
		}
		t.Cleanup(func() {
			if err := conn.Call(context.WithoutCancel(ctx), "certificate.delete", []any{made.ID, true}, nil); err != nil && !truenasrpc.IsNotFound(err) {
				t.Errorf("removing the certificate %s: %v", name, err)
			}
		})

		return made
	}

	for _, brought := range []struct {
		name    string
		until   time.Time
		expired bool
	}{
		{"ran-out-yesterday", now.Add(-24 * time.Hour), true},
		{"runs-out-in-five-days", now.Add(5 * 24 * time.Hour), false},
		{"good-for-a-year", now.Add(365 * 24 * time.Hour), false},
	} {
		made := bring(brought.name, now.Add(-30*24*time.Hour), brought.until)

		// read back by the user who may only read, as whatever watches a NAS reads it
		var listed []certified
		asked(ctx, t, reader(t), "certificate.query", []any{[]any{[]any{"id", "=", made.ID}}}, &listed)
		if len(listed) != 1 {
			t.Fatalf("%s: %d are listed by its id", brought.name, len(listed))
		}
		read := listed[0]

		if !read.Parsed || read.Kind != "CERTIFICATE" || read.Request || read.Expired == nil || *read.Expired != brought.expired {
			t.Errorf("%s reads as %s, parsed %t, run out %v; want a certificate that has run out: %t", brought.name, read.Kind, read.Parsed, read.Expired, brought.expired)
		}
		if read.Until == nil || read.From == nil {
			t.Fatalf("%s does not say when it is good from, or when it runs out", brought.name)
		}
		if until, err := time.Parse(certificateTime, *read.Until); err != nil || !until.Equal(brought.until) {
			t.Errorf("%s is said to run out at %q, which reads as %v (%v); want %v", brought.name, *read.Until, until, err, brought.until)
		}
		if read.Common == nil || *read.Common != brought.name+".tests.example" || read.KeyType == nil || *read.KeyType != "EC" {
			t.Errorf("%s reads as made out to %v with a key of kind %v", brought.name, read.Common, read.KeyType)
		}
		if read.Key == nil || *read.Key != truenasrpc.HiddenSecret {
			t.Errorf("%s: its key is not kept back from the user who may only read", brought.name)
		}
		t.Logf("%s: from %q until %q, names %v", brought.name, *read.From, *read.Until, read.Names)

		if !brought.expired {
			continue
		}

		// the NAS will not show its own pages with a certificate that has run out
		err := conn.Call(ctx, "system.general.update", []any{map[string]any{"ui_certificate": made.ID}}, nil)
		if err == nil {
			t.Error("a certificate that has run out is taken as the NAS's own")

			if err := conn.Call(ctx, "system.general.update", []any{map[string]any{"ui_certificate": shown.Certificate.ID}}, nil); err != nil {
				t.Fatalf("putting the NAS's own certificate back: %v", err)
			}
		} else if !truenasrpc.IsInvalid(err) || !strings.Contains(err.Error(), "expired") {
			t.Errorf("a certificate that has run out is refused as the NAS's own some other way than for having run out: %v", err)
		}
		t.Logf("a certificate that has run out is refused as the NAS's own with: %v", err)

		for _, one := range alerted(ctx, t, conn, "CertificateExpired") {
			t.Logf("the NAS says (%s): %s", one.Level, one.Formatted)
		}
	}

	// a request for a certificate, made on the NAS for someone else to sign
	var request certified
	if err := conn.Call(ctx, "certificate.create", []any{map[string]any{"name": "asked-for", "create_type": "CERTIFICATE_CREATE_CSR", "key_type": "EC", "ec_curve": "SECP256R1", "common": "asked-for.tests.example", "country": "US", "state": "Somewhere", "city": "Anywhere", "organization": "made by the tests", "email": "tests@tests.example", "san": []string{"asked-for.tests.example"}, "digest_algorithm": "SHA256"}}, &request); err != nil {
		t.Fatalf("having the NAS make a request for a certificate: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "certificate.delete", []any{request.ID, true}, nil); err != nil && !truenasrpc.IsNotFound(err) {
			t.Errorf("removing the request: %v", err)
		}
	})
	// a request is listed as a certificate like any other: that it is a request, with no dates, is all that tells them apart
	if !request.Request || request.Kind != "CERTIFICATE" || request.Until != nil || request.Expired != nil {
		t.Errorf("the request reads as %s (a request: %t) that runs out at %v; want a request, which never does", request.Kind, request.Request, request.Until)
	}

	var renamed certified
	if err := conn.Call(ctx, "certificate.update", []any{request.ID, map[string]any{"name": "asked-for-and-renamed"}}, &renamed); err != nil || renamed.Name != "asked-for-and-renamed" {
		t.Errorf("renamed, the request reads as %q (%v)", renamed.Name, err)
	}

	var raw json.RawMessage
	asked(ctx, t, conn, "certificate.get_instance", []any{request.ID}, &raw)

	if err := conn.Call(ctx, "certificate.delete", []any{request.ID, false}, nil); err != nil {
		t.Errorf("removing the request: %v", err)
	}
	if err := conn.Call(ctx, "certificate.get_instance", []any{request.ID}, nil); !truenasrpc.IsNotFound(err) {
		t.Errorf("removed, asking for the request is answered: %v", err)
	}
}
