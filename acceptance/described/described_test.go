package described_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/truenas-mcp/acceptance/described"
)

// release is the description these tests read: the one kept in api-defs.
const release = "25.10.7"

func load(t *testing.T) map[string]described.Method {
	t.Helper()

	methods, err := described.Load("../../api-defs", release)
	if err != nil {
		t.Fatal(err)
	}

	return methods
}

// How a method can be tested follows from what the description says of it, and each kind is tested its own way: a method sorted wrongly is one
// called when it should not be, or never called at all.
func TestEveryMethodIsSortedByHowItCanBeTested(t *testing.T) {
	t.Parallel()

	methods := load(t)

	for name, want := range map[string]described.Kind{
		"system.info":               described.Read,
		"pool.query":                described.Read,
		"pool.dataset.get_instance": described.ReadWithArguments,
		"filesystem.stat":           described.ReadWithArguments,
		"pool.import_find":          described.ReadJob,
		"pool.create":               described.Write,
		"pool.dataset.delete":       described.Write,
		"config.save":               described.File,
		"filesystem.put":            described.File,
		"core.ping":                 described.Session,
		"auth.login_ex":             described.Session,
		// the role that may only read may still make, change and remove keys of its own, and those change the NAS
		"api_key.create": described.Write,
		"api_key.update": described.Write,
		"api_key.delete": described.Write,
		"api_key.query":  described.Read,
		// and may send a ticket to TrueNAS's support, which is nothing a program that only reads should do
		"support.new_ticket":    described.Write,
		"support.attach_ticket": described.File,
		"support.config":        described.Read,
	} {
		m, ok := methods[name]
		if !ok {
			t.Errorf("%s is not in the description", name)

			continue
		}
		if got := m.Kind(); got != want {
			t.Errorf("%s is sorted as %q, want %q", name, got, want)
		}
	}
}

// The counts are what the description of this release holds. They are written down so that a refreshed description shows what moved, and so
// that a change to the sorting that quietly empties a kind fails here.
func TestTheDescriptionHoldsWhatItIsSaidToHold(t *testing.T) {
	t.Parallel()

	got := map[described.Kind]int{}
	for _, m := range load(t) {
		got[m.Kind()]++
	}

	want := map[described.Kind]int{described.Read: 252, described.ReadWithArguments: 121, described.ReadJob: 4, described.Write: 343, described.File: 16, described.Session: 33}
	for kind, n := range want {
		if got[kind] != n {
			t.Errorf("%d methods are sorted as %q, want %d", got[kind], kind, n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the methods are sorted into %d kinds, want %d: %v", len(got), len(want), got)
	}
}

// Most methods the read-only role may call, it may call because they need a role that only reads. These are the ones it is given by name, and
// nothing in the description says which of them only read: each was looked at, and five do more. A release that hands the role another one
// fails here until that one has been looked at too.
func TestEveryMethodTheReadOnlyRoleIsGivenByNameHasBeenLookedAt(t *testing.T) {
	t.Parallel()

	reads := []string{
		"acme.dns.authenticator.authenticator_schemas", "auth.mechanism_choices", "boot.get_state", "device.get_info", "ipmi.is_loaded", "nfs.get_nfs3_clients", "nfs.get_nfs4_clients",
		"sharing.smb.share_precheck", "system.debug", "system.global.id", "system.host_id", "system.info", "truenas.get_chassis_hardware", "truenas.get_eula", "truenas.is_eula_accepted",
		"truenas.is_ix_hardware", "truenas.is_production", "vmware.dataset_has_vms", "vmware.get_datastores", "vmware.match_datastores_with_datasets",
	}
	more := []string{"api_key.create", "api_key.update", "api_key.delete", "support.new_ticket", "support.attach_ticket"}

	var given []string
	for name, m := range load(t) {
		if !slices.Contains(m.Roles, "READONLY_ADMIN") || slices.ContainsFunc(m.Roles, func(role string) bool { return strings.HasSuffix(role, "_READ") }) {
			continue
		}
		given = append(given, name)

		switch reading := m.Kind() != described.Write && name != "support.attach_ticket"; {
		case slices.Contains(reads, name) && !reading:
			t.Errorf("%s only reads, and is sorted as %q", name, m.Kind())
		case slices.Contains(more, name) && reading:
			t.Errorf("%s does more than read, and is sorted as %q", name, m.Kind())
		case !slices.Contains(reads, name) && !slices.Contains(more, name):
			t.Errorf("%s is given to the read-only role by name, and nobody has said whether it only reads", name)
		}
	}

	if len(given) != len(reads)+len(more) {
		t.Errorf("%d methods are given to the read-only role by name, and %d have been looked at", len(given), len(reads)+len(more))
	}
}

// An answer is held to the description a piece at a time, and what is wrong is said by where it is.
func TestAnAnswerIsHeldToItsDescription(t *testing.T) {
	t.Parallel()

	row := `{"type":"object","additionalProperties":false,"required":["id","name"],"properties":{
		"id":{"type":"integer"},
		"name":{"type":"string"},
		"state":{"type":"string","enum":["ONLINE","DEGRADED"]},
		"kind":{"const":"POOL","type":"string"},
		"size":{"anyOf":[{"type":"integer"},{"type":"null"}]},
		"made":{"type":"string","format":"date-time"},
		"extra":{"type":"object","additionalProperties":{"type":"integer"}},
		"loose":{"type":"object","additionalProperties":true},
		"either":{"oneOf":[{"type":"object","additionalProperties":false,"properties":{"a":{"type":"string"}}},{"type":"object","additionalProperties":false,"properties":{"b":{"type":"integer"},"c":{"type":"string"}}}]}
	}}`

	for name, c := range map[string]struct {
		described, answer string
		want              []string
	}{
		"an answer that fits":                       {row, `{"id":1,"name":"tank","state":"ONLINE","kind":"POOL","size":null}`, nil},
		"a name the description always has":         {row, `{"id":1}`, []string{"$ has no name, which the description says it always has"}},
		"a name it does not have":                   {row, `{"id":1,"name":"tank","guid":"7"}`, []string{"$ has a guid, which the description does not name"}},
		"a value of another kind":                   {row, `{"id":"1","name":"tank"}`, []string{"$.id is a string, and the description says integer"}},
		"a number that is not whole":                {row, `{"id":1.5,"name":"tank"}`, []string{"$.id is a number (1.5), and the description says integer"}},
		"a whole number written with a point":       {row, `{"id":2.0,"name":"tank"}`, nil},
		"a value not among those allowed":           {row, `{"id":1,"name":"tank","state":"FAULTED"}`, []string{`$.state is "FAULTED", which is not one of the 2 values the description allows`}},
		"a value other than the one it must be":     {row, `{"id":1,"name":"tank","kind":"DATASET"}`, []string{`$.kind is "DATASET", which is not one of the 1 values the description allows`}},
		"none of the kinds it may be":               {row, `{"id":1,"name":"tank","size":"big"}`, []string{"$.size is a string, and the description says integer"}},
		"a moment written the way a NAS writes one": {row, `{"id":1,"name":"tank","made":{"$date":1760000000000}}`, nil},
		"names that are all of one kind":            {row, `{"id":1,"name":"tank","extra":{"a":1,"b":"two"}}`, []string{"$.extra.* is a string, and the description says integer"}},
		"names nothing is said of":                  {row, `{"id":1,"name":"tank","loose":{"a":1,"b":"two"}}`, nil},
		"the closest of the shapes it may be":       {row, `{"id":1,"name":"tank","either":{"b":1,"c":2}}`, []string{"$.either.c is a number (2), and the description says string"}},
		"rows wrong the same way are told of once":  {`{"type":"array","items":[` + row + `]}`, `[{"id":1},{"id":2},{"id":"3","name":"x"}]`, []string{"$[] has no name, which the description says it always has", "$[].id is a string, and the description says integer"}},
		"a list where anything may be":              {`{"type":"array","items":[{}]}`, `[1,"two",null]`, nil},
		"nothing where a list is described":         {`{"type":"array","items":[{}]}`, `null`, []string{"$ is null, and the description says array"}},
		"an answer that is not JSON":                {row, `{`, []string{"the answer is not JSON: unexpected EOF"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := described.Method{Returns: []json.RawMessage{json.RawMessage(c.described)}}.Differences(json.RawMessage(c.answer))
			if !slices.Equal(got, c.want) {
				t.Errorf("the differences are %q, want %q", got, c.want)
			}
		})
	}
}

// A NAS writes some values in forms of its own, which the description calls text. Whatever reads an answer has to know each, so each is found.
func TestTheFormsANASWritesValuesInAreFound(t *testing.T) {
	t.Parallel()

	got := described.Forms(json.RawMessage(`{"made":{"$date":1},"rows":[{"at":{"$time":"01:00"}},{"at":{"$time":"02:00"}},{"plain":{"$a":1,"b":2}}],"name":"x"}`))
	want := []string{"$.made is a $date", "$.rows[].at is a $time"}
	if !slices.Equal(got, want) {
		t.Errorf("the forms found are %q, want %q", got, want)
	}
}

// Every shape in the description is one the checking reads: a keyword it does not know would be a claim it never holds an answer to.
func TestEveryAnswerInTheDescriptionIsDescribedInWordsTheCheckingKnows(t *testing.T) {
	t.Parallel()

	for name, m := range load(t) {
		if len(m.Returns) != 1 {
			t.Errorf("%s has %d shapes for its answer, want 1", name, len(m.Returns))

			continue
		}
		for _, unknown := range described.Unknown(m.Returns[0]) {
			t.Errorf("%s: %s", name, unknown)
		}
	}
}
