package truenasrpc_test

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// A TrueNAS writes a moment, a day, a time of day, a set and an address with its network each in a way of its own. A program reading an answer
// into a struct, or making an argument from one, gets them right both ways or gets nothing: this is each of them, as a NAS writes it, read and
// written back.
func TestTheValuesTrueNASWritesItsOwnWayAreReadAndWrittenBack(t *testing.T) {
	t.Parallel()

	var all struct {
		When    truenasrpc.Time            `json:"when"`
		Never   truenasrpc.Time            `json:"never"`
		Day     truenasrpc.Date            `json:"day"`
		At      truenasrpc.TimeOfDay       `json:"at"`
		Exact   truenasrpc.TimeOfDay       `json:"exact"`
		Days    truenasrpc.Set[string]     `json:"days"`
		Address truenasrpc.IPInterface     `json:"address"`
		Longer  truenasrpc.IPInterface     `json:"longer"`
		Many    []truenasrpc.Set[int]      `json:"many"`
		Maybe   *truenasrpc.Time           `json:"maybe"`
		ByName  map[string]truenasrpc.Date `json:"by_name"`
	}

	const wire = `{"when":{"$date":1791850926500},"never":null,"day":{"$type":"date","$value":"2026-10-12"},"at":{"$time":"16:22:06"},"exact":{"$time":"16:22:06.250000+01:00"},"days":{"$set":["mon","wed"]},"address":{"$ipv4_interface":"192.168.0.10/24"},"longer":{"$ipv6_interface":"fd00::10/64"},"many":[{"$set":[1,2]},{"$set":[]}],"maybe":null,"by_name":{"first":{"$type":"date","$value":"1999-12-31"}}}`
	if err := json.Unmarshal([]byte(wire), &all); err != nil {
		t.Fatal(err)
	}

	if want := time.UnixMilli(1791850926500).UTC(); !all.When.Equal(want) || !all.Never.IsZero() || all.Maybe != nil {
		t.Errorf("a moment read as %v, nothing as %v and %v, want %v and nothing", all.When, all.Never, all.Maybe, want)
	}
	if all.Day != (truenasrpc.Date{Year: 2026, Month: time.October, Day: 12}) || all.ByName["first"].String() != "1999-12-31" {
		t.Errorf("a day read as %v and %v", all.Day, all.ByName)
	}
	if all.At != (truenasrpc.TimeOfDay{Hour: 16, Minute: 22, Second: 6}) || all.Exact != (truenasrpc.TimeOfDay{Hour: 16, Minute: 22, Second: 6, Nanosecond: 250_000_000, Zone: "+01:00"}) {
		t.Errorf("a time of day read as %+v and %+v", all.At, all.Exact)
	}
	if len(all.Days) != 2 || all.Days[1] != "wed" || len(all.Many) != 2 || len(all.Many[0]) != 2 || len(all.Many[1]) != 0 {
		t.Errorf("a set read as %v and %v", all.Days, all.Many)
	}
	if all.Address.Prefix != netip.MustParsePrefix("192.168.0.10/24") || all.Longer.Prefix != netip.MustParsePrefix("fd00::10/64") {
		t.Errorf("an address and its network read as %v and %v", all.Address, all.Longer)
	}

	back, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != wire {
		t.Errorf("written back:\n%s\nwant what was read:\n%s", back, wire)
	}

	// what is not one of them is refused, not read as nothing
	for _, into := range []any{&all.When, &all.Day, &all.At, &all.Days, &all.Address} {
		for _, wrong := range []string{`"2026-10-12"`, `{"$other": 1}`, `7`} {
			if err := json.Unmarshal([]byte(wrong), into); err == nil {
				t.Errorf("%s was read into a %T", wrong, into)
			}
		}
	}
}

// A user who may only read is answered "********" where a secret is, whatever the secret is: a list of password hashes and a certificate with its
// key come back as that one piece of text. Seen on a real TrueNAS 25.10.7, in mail.config, system.general.config and user.query. A secret reads
// either way, says which it was, writes back as it was read, and says nothing of itself when printed.
func TestASecretTheNASKeptBackIsReadAsOneAndNeverPrinted(t *testing.T) {
	t.Parallel()

	type certificate struct {
		Name       string `json:"name"`
		PrivateKey string `json:"privatekey"`
	}
	var got struct {
		History     truenasrpc.Secret[[]string]     `json:"password_history"`
		Certificate truenasrpc.Secret[certificate]  `json:"ui_certificate"`
		Password    truenasrpc.Secret[string]       `json:"pass"`
		Shown       truenasrpc.Secret[[]string]     `json:"shown"`
		Whole       truenasrpc.Secret[certificate]  `json:"whole"`
		None        truenasrpc.Secret[*certificate] `json:"none"`
	}

	const wire = `{"password_history":"********","ui_certificate":"********","pass":"********","shown":["a-hash","another"],"whole":{"name":"truenas_default","privatekey":"a-private-key"},"none":null}`
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("an answer with its secrets kept back does not read: %v", err)
	}

	if !got.History.Hidden || got.History.Value != nil || !got.Certificate.Hidden || got.Certificate.Value != (certificate{}) || !got.Password.Hidden || got.Password.Value != "" {
		t.Errorf("secrets the NAS kept back read as shown: %t %t %t", got.History.Hidden, got.Certificate.Hidden, got.Password.Hidden)
	}
	if got.Shown.Hidden || len(got.Shown.Value) != 2 || got.Whole.Hidden || got.Whole.Value.PrivateKey != "a-private-key" || got.None.Hidden || got.None.Value != nil {
		t.Errorf("secrets the NAS showed read as kept back, or not as they were: %t %d, %t, %t", got.Shown.Hidden, len(got.Shown.Value), got.Whole.Hidden, got.None.Hidden)
	}

	back, err := json.Marshal(got)
	if err != nil || string(back) != wire {
		t.Errorf("written back as %s (%v), want %s", back, err, wire)
	}

	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		if printed := fmt.Sprintf(format, got.Whole) + fmt.Sprintf(format, &got.Whole) + fmt.Sprintf(format, got); strings.Contains(printed, "a-private-key") || strings.Contains(printed, "a-hash") {
			t.Errorf("printed with %s, a secret shows: %s", format, printed)
		}
	}

	// a secret read once as kept back and then as shown is the second, not a mix of the two
	again := truenasrpc.Secret[string]{Hidden: true}
	if err := json.Unmarshal([]byte(`"swordfish"`), &again); err != nil || again.Hidden || again.Value != "swordfish" {
		t.Errorf("read again as %q hidden=%t (%v)", again.Value, again.Hidden, err)
	}
}

// A NAS's errors carry a number, Linux's or one of TrueNAS's own, and a job that fails gives the number alone. The name is what the rest of the
// connection goes by, so every number a NAS is known to use has to come out as the name the NAS itself would give.
func TestAnErrorsNumberIsNamedAsTheNASNamesIt(t *testing.T) {
	t.Parallel()

	for number, name := range map[int]string{
		1: "EPERM", 2: "ENOENT", 13: "EACCES", 14: "EFAULT", 16: "EBUSY", 17: "EEXIST", 22: "EINVAL", 28: "ENOSPC", 110: "ETIMEDOUT", 122: "EDQUOT",
		truenasrpc.ErrnoNoMethod: "ENOMETHOD", truenasrpc.ErrnoDatasetIsLocked: "EDATASETISLOCKED", truenasrpc.ErrnoNotAuthenticated: "ENOTAUTHENTICATED",
		truenasrpc.ErrnoRebootRequired: "EREBOOTREQUIRED", truenasrpc.ErrnoHAUnavailable: "EHAUNAVAILABLE", 0: "EUNKNOWN", 9999: "EUNKNOWN",
	} {
		if got := truenasrpc.ErrnoName(number); got != name {
			t.Errorf("%d is named %s, want %s", number, got, name)
		}
	}
}
