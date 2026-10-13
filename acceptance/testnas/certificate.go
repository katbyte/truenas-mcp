package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
)

// certificateOf is the certificate a NAS shows, as a PEM file holds one. It is taken on sight, with nothing to check it against, which is right
// for a NAS that was made a minute ago by the program asking and for no other.
func certificateOf(ctx context.Context, server string) ([]byte, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%s is not the address of a NAS: give it as https://host:port", server)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}

	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}} //nolint:gosec // G402: see above; the certificate is what is being fetched
	raw, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = raw.Close() }()

	conn, ok := raw.(*tls.Conn)
	if !ok || len(conn.ConnectionState().PeerCertificates) == 0 {
		return nil, fmt.Errorf("the NAS at %s showed no certificate", server)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: conn.ConnectionState().PeerCertificates[0].Raw}), nil
}
