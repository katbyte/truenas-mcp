//go:build acceptance

package truenasrpc_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/described"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// readWait is how long one read is given. Most answer at once; a few ask a server out on the internet first.
const readWait = 45 * time.Second

// surprise is what a real NAS does with a read that its description does not say. Each is written down with why, so that the next one found is
// news, and one that stops being true fails too: the list is what is known of the NAS, not a way to quiet a test.
type surprise struct {
	// refused is part of what the NAS says when it will not answer, on a NAS like the test one
	refused string
	// differs is where its answer is not what the description says
	differs []string
	// absent is part of what it says of an id that is not there, where that is something other than plainly that it is not there
	absent string
	// outside says the call asks a server out on the internet, so whether it answers is not the NAS's doing: a failure is let be, and an answer
	// is held to the description like any other
	outside bool
	why     string
}

// surprises is every read a real TrueNAS 25.10.7 answers some other way than its description says, found by running these tests.
var surprises = map[string]surprise{}

// held to the description, answer by answer: what is wrong is said by where it is, and what is known to be wrong has to be wrong still.
func heldTo(t *testing.T, m described.Method, answer json.RawMessage) {
	t.Helper()

	got, want := m.Differences(answer), surprises[m.Name].differs
	for _, difference := range got {
		if !slices.Contains(want, difference) {
			t.Errorf("not as described: %s", difference)
		}
	}
	for _, difference := range want {
		if !slices.Contains(got, difference) {
			t.Errorf("as described after all, where it was known not to be: %s", difference)
		}
	}
}

// Every method that reads and needs no argument is called, as the user who may only read, and its answer held to the description. It is the
// one test every such method gets, and it is where a description that is wrong is found before anything generated from it is.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestEveryReadIsAnsweredTheWayItsDescriptionSays(t *testing.T) {
	methods := description(t)
	conn := reader(t)

	forms := map[string][]string{}
	for _, name := range named(methods, described.Read) {
		t.Run(name, func(t *testing.T) {
			known := surprises[name]

			var answer json.RawMessage
			err := conn.Call(within(t, readWait), name, nil, &answer)

			switch {
			case err != nil && known.outside:
				t.Skipf("it asks a server out on the internet, which did not answer: %v", err)
			case err != nil && known.refused != "" && strings.Contains(err.Error(), known.refused):
				return
			case err != nil:
				t.Fatalf("refused: %v", err)
			case known.refused != "":
				t.Fatalf("answered, where it was known to be refused with %q (%s)", known.refused, known.why)
			}

			heldTo(t, methods[name], answer)
			for _, form := range described.Forms(answer) {
				forms[form] = append(forms[form], name)
			}
		})
	}

	for name := range surprises {
		if _, ok := methods[name]; !ok {
			t.Errorf("%s is written down as a surprise, and the description has no such method", name)
		}
	}

	// the forms a NAS writes values in are what anything that reads an answer has to know, so the run says which it met and where
	for _, form := range sortedKeys(forms) {
		t.Logf("a value in a form of the NAS's own: %s (%s)", form, strings.Join(forms[form], ", "))
	}
}

// Whatever a NAS lists, it hands over one at a time too: every kind of thing with a list and a way to fetch one is listed, some of what is
// listed fetched by its id, and what comes back held to the description and to the row it was listed as. An id nothing has is asked for as
// well, since "it is not there" is the answer a program acts on most, and it has to be the same answer whatever was asked for.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestWhatIsListedIsFetchedByItsIDAndWhatIsNotThereIsMissing(t *testing.T) {
	methods := description(t)
	conn := reader(t)

	for _, name := range named(methods, described.ReadWithArguments) {
		kind, fetches := strings.CutSuffix(name, ".get_instance")
		if !fetches {
			continue
		}

		t.Run(kind, func(t *testing.T) {
			list, ok := methods[kind+".query"]
			if !ok || list.Kind() != described.Read {
				t.Fatalf("%s has no list to fetch from", kind)
			}

			var rows []map[string]json.RawMessage
			if err := conn.Call(within(t, readWait), list.Name, nil, &rows); err != nil {
				if refused := surprises[list.Name].refused; refused != "" && strings.Contains(err.Error(), refused) {
					t.Skipf("its list is refused on a NAS like this one: %v", err)
				}
				t.Fatalf("listing: %v", err)
			}
			t.Logf("%d listed", len(rows))

			for _, row := range rows[:min(len(rows), 3)] {
				id, has := row["id"]
				if !has {
					t.Fatal("what is listed has no id to fetch it by")
				}

				var one json.RawMessage
				if err := conn.Call(within(t, readWait), name, []any{id}, &one); err != nil {
					t.Errorf("fetching %s: %v", id, err)

					continue
				}
				heldTo(t, methods[name], one)

				var fetched struct {
					ID json.RawMessage `json:"id"`
				}
				if err := json.Unmarshal(one, &fetched); err != nil || !bytes.Equal(fetched.ID, id) {
					t.Errorf("asked for %s, the NAS handed over %s (%v)", id, fetched.ID, err)
				}
			}

			err := conn.Call(within(t, readWait), name, []any{nobody(methods[name])}, nil)
			switch absent := surprises[name].absent; {
			case absent != "" && err != nil && strings.Contains(err.Error(), absent):
			case absent != "":
				t.Errorf("an id that is not there was known to be answered with %q, and was answered: %v", absent, err)
			case !truenasrpc.IsNotFound(err):
				t.Errorf("an id that is not there was not answered as missing: %v", err)
			}
		})
	}
}

// nobody is an id nothing on the NAS has, of the kind the method takes: a number or a name.
func nobody(m described.Method) any {
	var takes struct {
		Type string `json:"type"`
	}
	if len(m.Accepts) > 0 && json.Unmarshal(m.Accepts[0], &takes) == nil && takes.Type == "integer" {
		return 2147480000
	}

	return "nothing-goes-by-this-name"
}

// named is the methods of a kind, in order.
func named(methods map[string]described.Method, kind described.Kind) []string {
	var names []string
	for name, m := range methods {
		if m.Kind() == kind {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	return names
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	return keys
}
