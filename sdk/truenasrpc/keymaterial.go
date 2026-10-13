package truenasrpc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// rawKey is an API key as TrueNAS shows it when it is made: the number of the key, a dash, and the secret.
var rawKey = regexp.MustCompile(`^\d+-[a-zA-Z0-9]+$`)

// keyMaterial is an API key as a login needs it: the key itself, or the parts worked out of it that the newer login proves itself with, which a key
// file may hold in place of the key so that the key is not kept anywhere.
type keyMaterial struct {
	// raw is the whole key, "7-abc...", and secret the part after the dash; both are empty when only the worked-out parts were given
	raw    string
	secret string
	// id is the number of the key, which the newer login names beside the user
	id   int
	keys scramKeys
}

// worked reports whether the key came as its worked-out parts and not as the key itself.
func (k keyMaterial) worked() bool { return k.raw == "" }

// readKey reads an API key in any of the forms a TrueNAS hands one out in: the key as it is shown ("7-abc..."), or the contents of a key file, which
// is JSON or an INI file holding either the key (raw_key) or its worked-out parts (api_key_id, client_key, stored_key, server_key).
func readKey(text string) (keyMaterial, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return keyMaterial{}, errors.New("the TrueNAS API key is required")
	}
	if rawKey.MatchString(text) {
		return splitKey(text)
	}

	fields := map[string]string{}
	var asJSON map[string]any
	if err := json.Unmarshal([]byte(text), &asJSON); err == nil {
		for name, value := range asJSON {
			switch v := value.(type) {
			case string:
				fields[name] = v
			case float64:
				fields[name] = strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
	} else if fields, err = iniSection(text); err != nil {
		// nothing of what was given is quoted: it is, or was meant to be, a key
		return keyMaterial{}, errors.New("the TrueNAS API key is not one: it is neither a key as TrueNAS shows it (a number, a dash, letters and digits) nor a key file")
	}

	if raw, ok := fields["raw_key"]; ok {
		return splitKey(strings.TrimSpace(raw))
	}

	k := keyMaterial{}
	id, err := strconv.Atoi(fields["api_key_id"])
	if err != nil || id < 1 {
		return keyMaterial{}, errors.New("the TrueNAS API key file holds neither a key (raw_key) nor the number of one (api_key_id)")
	}
	k.id = id

	for name, into := range map[string]*[]byte{"client_key": &k.keys.client, "stored_key": &k.keys.stored, "server_key": &k.keys.server} {
		part, err := base64.StdEncoding.DecodeString(fields[name])
		if err != nil || len(part) == 0 {
			return keyMaterial{}, fmt.Errorf("the TrueNAS API key file's %s is missing or is not base64", name)
		}
		*into = part
	}

	return k, nil
}

func splitKey(raw string) (keyMaterial, error) {
	if !rawKey.MatchString(raw) {
		return keyMaterial{}, errors.New("the TrueNAS API key is not one: a key is a number, a dash, then letters and digits")
	}

	number, secret, _ := strings.Cut(raw, "-")
	id, err := strconv.Atoi(number)
	if err != nil {
		return keyMaterial{}, errors.New("the TrueNAS API key is not one: the number before its dash is too long")
	}

	return keyMaterial{raw: raw, secret: secret, id: id}, nil
}

// iniSection reads the one section of an INI key file that holds the key: [TRUENAS_API_KEY] where there is one, else the only section there is.
// Names are read without regard to case, as TrueNAS's own client reads them.
func iniSection(text string) (map[string]string, error) {
	sections := map[string]map[string]string{}
	var order []string
	var current map[string]string

	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			if name == "" {
				return nil, errors.New("a section with no name")
			}
			if sections[name] == nil {
				sections[name] = map[string]string{}
				order = append(order, name)
			}
			current = sections[name]

			continue
		}

		name, value, ok := strings.Cut(line, "=")
		if !ok {
			name, value, ok = strings.Cut(line, ":")
		}
		if !ok || current == nil {
			return nil, errors.New("a line that is not a setting, or a setting before any section")
		}
		current[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}

	switch {
	case sections["TRUENAS_API_KEY"] != nil:
		return sections["TRUENAS_API_KEY"], nil
	case len(order) == 1:
		return sections[order[0]], nil
	default:
		return nil, errors.New("no one section to read the key from")
	}
}
