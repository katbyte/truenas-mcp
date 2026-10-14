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
	"github.com/katbyte/truenas-mcp/acceptance/layout"
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
var surprises = map[string]surprise{
	"app.available_space":             {refused: "No pool configured for Docker", why: "the test NAS has no pool chosen for apps to live on"},
	"tn_connect.get_registration_uri": {refused: "TrueNAS Connect is not enabled", why: "the test NAS is not joined to TrueNAS Connect"},
	"sharing.smb.share_precheck":      {refused: "at least one local SMB user", why: "the test NAS has no user who may use SMB, and saying so is what the check is for"},
	"sharing.smb.query":               {differs: []string{"$[].options has no purpose, which the description says it always has"}, why: "a share made with nothing said of its options has options with no purpose in them"},
	"sharing.smb.get_instance":        {differs: []string{"$.options has no purpose, which the description says it always has"}, why: "the same share, fetched by itself"},
	// the description says it needs no argument. TestReadsThatAreToldWhatToReadAnswerTheWayTheirDescriptionSays asks it with a limit
	"audit.query": {refused: "query-options must be set to either gather row count or contain a limit", why: "a NAS will not list its whole audit log: it wants a limit on the rows, or to be asked only how many there are"},
}

// keyed is what a kind of thing is fetched by, where that is not its id: a row of the list has no id at all.
var keyed = map[string]string{
	"disk": "identifier",
}

// heldTo holds an answer to the description: what is wrong is said by where it is, and what is known to be wrong has to be wrong still. A
// secret the NAS kept back has to be somewhere api-defs says a secret is, and the administrator is shown them all.
func heldTo(t *testing.T, m described.Method, answer json.RawMessage, administrator bool) {
	t.Helper()

	got, want := m.Differences(answer), surprises[m.Name].differs
	for _, difference := range got {
		if !slices.Contains(want, difference) {
			t.Errorf("not as described: %s", difference)
		}
	}
	// what is known to be wrong with a row can only show when there is a row
	rows := !bytes.Equal(bytes.TrimSpace(answer), []byte("[]"))
	for _, difference := range want {
		if !slices.Contains(got, difference) && (rows || !strings.HasPrefix(difference, "$[]")) {
			t.Errorf("as described after all, where it was known not to be: %s", difference)
		}
	}

	for _, place := range described.Blanked(answer) {
		switch {
		case administrator:
			t.Errorf("a secret was kept back from the administrator, at %s", place)
		case !slices.Contains(secrets(t)[m.Name], place):
			t.Errorf("something was kept back at %s, where api-defs lists no secret", place)
		}
	}
}

// secrets is where the secrets are in each method's answer, as api-defs keeps them for the NAS's version.
func secrets(t *testing.T) map[string][]string {
	t.Helper()

	places, err := described.Secrets("../../api-defs", release(t))
	if err != nil {
		t.Fatal(err)
	}

	return places
}

// Every method that reads and needs no argument is called, and its answer held to the description. It is the one test every such method gets,
// and it is where a description that is wrong is found before anything generated from it is. Each is asked twice: by the user who may only
// read, who is answered with every secret kept back, and by the administrator, who is shown them.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestEveryReadIsAnsweredTheWayItsDescriptionSays(t *testing.T) {
	methods := description(t)

	forms, kept := map[string][]string{}, map[string][]string{}
	for _, by := range []struct {
		name          string
		conn          *truenasrpc.Conn
		administrator bool
	}{{"asked by the user who may only read", reader(t), false}, {"asked by the administrator", admin(t), true}} {
		t.Run(by.name, func(t *testing.T) {
			for _, name := range named(methods, described.Read) {
				t.Run(name, func(t *testing.T) {
					known := surprises[name]

					var answer json.RawMessage
					err := by.conn.Call(within(t, readWait), name, nil, &answer)

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

					heldTo(t, methods[name], answer, by.administrator)
					for _, form := range described.Forms(answer) {
						if !slices.Contains(forms[form], name) {
							forms[form] = append(forms[form], name)
						}
					}
					kept[name] = append(kept[name], described.Blanked(answer)...)
				})
			}
		})
	}

	for name := range surprises {
		if _, ok := methods[name]; !ok {
			t.Errorf("%s is written down as a surprise, and the description has no such method", name)
		}
	}

	// the forms a NAS writes values in, and the secrets it keeps back, are what anything that reads an answer has to know, so the run says
	// which it met and where
	for _, form := range sortedKeys(forms) {
		t.Logf("a value in a form of the NAS's own: %s (%s)", form, strings.Join(forms[form], ", "))
	}
	for _, name := range sortedKeys(kept) {
		if len(kept[name]) == 0 {
			continue
		}
		t.Logf("kept back from the user who may only read: %s %s", name, strings.Join(kept[name], " "))
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

			by := "id"
			if other, ok := keyed[kind]; ok {
				by = other
			}

			for _, row := range rows[:min(len(rows), 3)] {
				id, has := row[by]
				if !has {
					t.Fatalf("what is listed has no %s to fetch it by", by)
				}

				var one json.RawMessage
				if err := conn.Call(within(t, readWait), name, []any{id}, &one); err != nil {
					t.Errorf("fetching %s: %v", id, err)

					continue
				}
				heldTo(t, methods[name], one, false)

				var fetched map[string]json.RawMessage
				if err := json.Unmarshal(one, &fetched); err != nil || !bytes.Equal(fetched[by], id) {
					t.Errorf("asked for %s, the NAS handed over %s (%v)", id, fetched[by], err)
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

// A read that has to be told what to read is asked about something the journey made, or something every NAS has, and its answer held to the
// description the same way. What is not asked here needs something the test NAS does not have yet: an app, a virtual machine, a task.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestReadsThatAreToldWhatToReadAnswerTheWayTheirDescriptionSays(t *testing.T) {
	methods := description(t)
	conn := reader(t)

	var pools []struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(within(t, readWait), "pool.query", []any{[]any{[]any{"name", "=", layout.Pool}}}, &pools); err != nil || len(pools) != 1 {
		t.Fatalf("the pool the journey made is not found: %d found, %v", len(pools), err)
	}

	var disks []struct {
		Name string `json:"name"`
	}
	if err := conn.Call(within(t, readWait), "disk.query", nil, &disks); err != nil || len(disks) == 0 {
		t.Fatalf("the NAS lists no disks: %v", err)
	}
	names := make([]string, len(disks))
	for i, disk := range disks {
		names[i] = disk.Name
	}

	pool, dataset, path := pools[0].ID, layout.Datasets[0], "/mnt/"+layout.Datasets[0]
	for name, params := range map[string][]any{
		"filesystem.stat":                         {path},
		"filesystem.statfs":                       {path},
		"filesystem.listdir":                      {"/mnt/" + layout.Pool},
		"filesystem.getacl":                       {path},
		"filesystem.get_zfs_attributes":           {path},
		"filesystem.acltemplate.by_path":          {map[string]any{"path": path}},
		"pool.attachments":                        {pool},
		"pool.processes":                          {pool},
		"pool.is_upgraded":                        {pool},
		"pool.validate_name":                      {"another"},
		"pool.dataset.attachments":                {dataset},
		"pool.dataset.processes":                  {dataset},
		"pool.dataset.get_quota":                  {dataset, "USER"},
		"pool.dataset.snapshot_count":             {dataset},
		"pool.dataset.recommended_zvol_blocksize": {layout.Pool},
		"disk.temperature_agg":                    {names, 7},
		"disk.temperature_alerts":                 {names},
		"device.get_info":                         {map[string]any{"type": "DISK"}},
		"group.get_group_obj":                     {map[string]any{"groupname": "root"}},
		"user.get_user_obj":                       {map[string]any{"username": "root"}},
		"group.has_password_enabled_user":         {[]int{0}},
		"service.started":                         {"ssh"},
		"service.started_or_enabled":              {"ssh"},
		"system.feature_enabled":                  {"DEDUP"},
		"reporting.netdata_graph":                 {"cpu"},
		"reporting.netdata_get_data":              {[]any{map[string]any{"name": "cpu"}}},
		"reporting.graph":                         {"cpu"},
		"reporting.get_data":                      {[]any{map[string]any{"name": "cpu"}}},
		// the address a machine run this way is given as its way out
		"route.ipv4gw_reachable":               {"10.0.2.2"},
		"nvmet.port.transport_address_choices": {"TCP"},
		"sharing.smb.share_precheck":           {map[string]any{"name": "nothing-shared"}},
		"virt.device.gpu_choices":              {"PHYSICAL"},
		"virt.device.nic_choices":              {"BRIDGED"},
		// the description says it needs no argument, and a NAS refuses it without a limit on the rows
		"audit.query": {map[string]any{"query-options": map[string]any{"limit": 10}}},
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := methods[name]
			if !ok || (m.Kind() != described.ReadWithArguments && m.Kind() != described.Read) {
				t.Fatalf("%s is not a read: the description sorts it as %q", name, m.Kind())
			}

			// what is refused with no argument is asked here with one, so only a method never called bare can be known to be refused
			known := surprises[name]
			if m.Kind() == described.Read {
				known = surprise{}
			}

			var answer json.RawMessage
			err := conn.Call(within(t, readWait), name, params, &answer)

			switch {
			case err != nil && known.refused != "" && strings.Contains(err.Error(), known.refused):
				return
			case err != nil:
				t.Fatalf("refused: %v", err)
			case known.refused != "":
				t.Fatalf("answered, where it was known to be refused with %q (%s)", known.refused, known.why)
			}

			heldTo(t, m, answer, false)
		})
	}
}
