package truenasrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/katbyte/go-kt/httpc"
)

// hidden stands in a trace for a value that is not shown, the word go-kt's HTTP trace uses.
const hidden = "REDACTED"

// scrubbed stands in an error for a credential the NAS quoted back.
const scrubbed = "***"

// secretNames are the fields a TrueNAS keeps a secret in under a name that does not read as one: names with password, secret or token in them are
// hidden unasked. Some are wider than they need be, since a trace that hides too much is better than one that shows a key: provider and attributes
// are where a cloud account's keys sit, and values is an app's whole configuration.
//
// The second line is what TrueNAS itself does not treat as secret and hands to anyone who may read: the private keys its SSH server proves itself
// with, and the key a replication encrypts what it copies with. api-defs/README.md says how each was found.
var secretNames = []string{
	"key", "pass", "passwd", "sed_passwd", "bindpw", "unixhash", "smbhash", "keyhash", "v3_privkey", "v3_authkey", "dhchap_key", "dhchap_ctrl_key",
	"host_key", "host_dsa_key", "host_ecdsa_key", "host_ed25519_key", "host_rsa_key", "private_key", "privatekey", "encryption_key",
	"service_key", "service_account_credentials", "access_key_id", "client_id", "encryption_salt", "provider", "attributes", "values", "oauth",
	"custom_compose_config", "custom_compose_config_string", "ui_certificate",
}

// scopedSecretNames are names that hold a secret in one part of the API and something plain everywhere else, by the start of the method's name: a
// keytab's file is the keytab, where any other file is a path.
var scopedSecretNames = map[string][]string{
	"kerberos.keytab.": {"file"},
	"app.registry.":    {"username"},
	"app.image.":       {"username"},
	"virt.instance.":   {"raw"},
}

// wholeSecrets are the calls that take a secret by position or answer one bare, with no name on it to hide it by, beside every call under auth:
// nothing of their arguments or answer is shown.
//
// None of these three lists is a guess. api-defs holds the place of every value the TrueNAS source marks secret, and
// TestEverySecretTheSourceMarksIsHiddenInATrace plants a marker at each one; TestNoMethodShowsASecretItTakesByPositionOrAnswersBare does the same
// for what the source leaves unmarked, from the NAS's own description of its API. A new version's secret under a name nobody thought of fails there.
var wholeSecrets = []string{
	"user.setup_local_administrator", "user.set_password", "pool.dataset.export_key", "system.advanced.sed_global_password", "nvmet.host.generate_key",
	"tn_connect.generate_claim_token", "cloudsync.credentials.verify",
	// these four carry another method's arguments or answer, whichever method it is, or answer an address that is good for one download by
	// anyone who has it
	"core.job_wait", "core.bulk", "core.download", "core.job_download_logs",
}

// lazy is text built only when it is written, so a trace that is switched off costs nothing.
type lazy func() string

func (l lazy) String() string { return l() }

func (c *Conn) debugf(format string, args ...any) {
	if c.o.log != nil {
		c.o.log.Debugf(format, args...)
	}
}

// trace writes a message that was sent ("->") or came back ("<-") to the log, with its secrets hidden.
func (c *Conn) trace(arrow, method string, frame []byte) {
	if c.o.log != nil {
		c.o.log.Tracef("TrueNAS %s %s", arrow, lazy(func() string { return c.shown(method, frame) }))
	}
}

// shown is a message as a trace shows it: every value under a secret's name blanked, the arguments and answer of a call that carries a bare secret
// left out whole, the connection's own key blanked wherever it sits, and no more of it than a trace prints of anything.
func (c *Conn) shown(method string, frame []byte) string {
	var message map[string]any
	dec := json.NewDecoder(bytes.NewReader(frame))
	dec.UseNumber()
	if err := dec.Decode(&message); err != nil {
		return fmt.Sprintf("%d bytes that are not a JSON object", len(frame))
	}

	if named, ok := message["method"].(string); ok && method == "" {
		method = named
	}

	// something the NAS sends unasked about a list it keeps is treated as a call on that list would be: its names mean what they mean there
	scope := method
	if event, ok := message["params"].(map[string]any); ok {
		if collection, ok := event["collection"].(string); ok {
			scope = collection

			if job, ok := event["fields"].(map[string]any); ok && collection == jobsCollection {
				c.jobShown(job)
			}
		}
	}
	if rows, ok := message["result"].([]any); ok && method == jobsCollection {
		for _, row := range rows {
			if job, ok := row.(map[string]any); ok {
				c.jobShown(job)
			}
		}
	}

	also := scopedNames(scope)
	whole := wholeSecret(method)
	for _, part := range []string{"params", "result"} {
		value, has := message[part]
		if !has {
			continue
		}
		if whole {
			message[part] = hidden

			continue
		}

		message[part] = c.blanked(value, also)
	}

	// what a NAS says of a failure can quote what it was sent, and its record of where the failure happened holds whatever was in hand at the time
	if failure, ok := message["error"].(map[string]any); ok {
		if data, ok := failure["data"].(map[string]any); ok {
			delete(data, "trace")
			delete(data, "py_exception")
		}

		message["error"] = c.blanked(failure, also)
	}

	out, err := json.Marshal(message)
	if err != nil {
		return fmt.Sprintf("%d bytes that cannot be shown", len(frame))
	}

	return cut(c.scrub(string(out), hidden), httpc.DefaultTraceBody)
}

// jobShown blanks, in a NAS's account of a job, what the job was called with and what it answered, as they would be in the call's own trace: a
// job's account carries both, whatever the method. Its record of where a failure happened is dropped, as a call's is.
func (c *Conn) jobShown(job map[string]any) {
	var ran string
	if named, ok := job["method"].(string); ok {
		ran = named
	}

	for _, part := range []string{"arguments", "result"} {
		value, has := job[part]
		if !has || value == nil {
			continue
		}
		if wholeSecret(ran) {
			job[part] = hidden

			continue
		}

		job[part] = c.blanked(value, scopedNames(ran))
	}

	if job["exception"] != nil {
		job["exception"] = hidden
	}
}

// wholeSecret reports whether nothing of a method's arguments or answer is shown.
func wholeSecret(method string) bool {
	return strings.HasPrefix(method, "auth.") || containsFold(wholeSecrets, method)
}

// scopedNames are the names that hold a secret in the part of the API a method belongs to, beside the ones that do everywhere.
func scopedNames(method string) []string {
	var names []string
	for prefix, scoped := range scopedSecretNames {
		if strings.HasPrefix(method, prefix) {
			names = append(names, scoped...)
		}
	}

	return names
}

// blanked is a value with everything under a secret's name hidden, however deep, and the value beside a secret's name where the two are sent as a
// pair ({"name": "api_key", "value": "..."}). also are names that hold a secret in the part of the API the message belongs to.
func (c *Conn) blanked(value any, also []string) any {
	switch v := value.(type) {
	case map[string]any:
		named, paired := v["name"].(string)
		for key, inner := range v {
			if c.secretName(key, also) || key == "value" && paired && c.secretName(named, also) {
				v[key] = hidden

				continue
			}

			v[key] = c.blanked(inner, also)
		}
	case []any:
		for i, inner := range v {
			v[i] = c.blanked(inner, also)
		}
	}

	return value
}

// secretName reports whether a field of this name holds a secret: by the rule every trace of go-kt's goes by, by the names that are TrueNAS's own,
// by those the caller added, and by those that are secret only where this message comes from.
func (c *Conn) secretName(name string, also []string) bool {
	return httpc.SecretName(name, secretNames...) || containsFold(c.o.secretNames, name) || containsFold(also, name)
}

// scrub is text with what the connection logs in with replaced wherever it appears: the key, its secret part, a password. One too short to tell
// from an ordinary word is left, as hiding it would hide pieces of everything.
func (c *Conn) scrub(text, with string) string {
	for _, secret := range c.cred.secrets {
		if len(secret) >= 6 {
			text = strings.ReplaceAll(text, secret, with)
		}
	}

	return text
}

// cut is text no longer than limit bytes, ended between characters, saying how much more there was.
func cut(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}

	return fmt.Sprintf("%s... (%d more bytes)", text[:end], len(text)-end)
}

func containsFold(list []string, text string) bool {
	for _, have := range list {
		if strings.EqualFold(have, text) {
			return true
		}
	}

	return false
}
