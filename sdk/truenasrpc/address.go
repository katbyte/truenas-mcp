package truenasrpc

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// DefaultPath is where a TrueNAS serves the newest version of its API. A setting may name another, /api/v25.10.5, to hold a NAS to the version a
// program was written for after the NAS itself is upgraded.
const DefaultPath = "/api/current"

// DefaultSocket is where a TrueNAS listens for the programs that run on the NAS itself.
const DefaultSocket = "/run/middleware/middlewared.sock"

// OverSocket reports whether an address is that of a NAS's own socket, "unix://" and a path, and not of a NAS reached over the network.
func OverSocket(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))

	return strings.HasPrefix(address, "unix://") || strings.HasPrefix(address, "ws+unix://")
}

// endpoint is the address a TrueNAS's API is dialled at, from what a setting holds: a bare host, a host and port, or a whole https or wss address,
// with DefaultPath added when it names no path. An address that starts unix:// is the NAS's own socket, for a program running on the NAS: socket is
// then its path, DefaultSocket when the address names none, and the address dialled is only there for the request to carry.
func endpoint(address string) (dial, socket string, err error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", "", errors.New("the TrueNAS address is required, e.g. nas.example.net")
	}

	if OverSocket(address) {
		_, socket, _ = strings.Cut(address, "://")
		if socket == "" {
			socket = DefaultSocket
		}
		if !strings.HasPrefix(socket, "/") {
			return "", "", fmt.Errorf("the TrueNAS address %s names a socket by a path that does not start at the root: three slashes, as in unix://%s", address, DefaultSocket)
		}

		return "ws://localhost" + DefaultPath, socket, nil
	}

	if !strings.Contains(address, "://") {
		address = "wss://" + address
	}

	u, err := url.Parse(address)
	if err != nil || u.Host == "" {
		return "", "", errors.New("the TrueNAS address is not one: give a host, a host and port, or an https address, e.g. nas.example.net")
	}
	if u.User != nil {
		return "", "", errors.New("the TrueNAS address holds a user name or password: a NAS is logged into with an API key, which is given apart from the address")
	}

	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		u.Scheme = "wss"
	case "ws", "http":
		return "", "", fmt.Errorf("the TrueNAS address %s has no TLS, and TrueNAS revokes an API key sent without it: use https://%s", address, u.Host)
	default:
		return "", "", fmt.Errorf("the TrueNAS address %s is neither https nor wss", address)
	}

	if u.Path == "" || u.Path == "/" {
		u.Path = DefaultPath
	}
	u.RawQuery, u.Fragment = "", ""

	return u.String(), "", nil
}
