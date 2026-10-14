package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// madeUpSource is a checkout of the TrueNAS source made up for the tests, with every way the source declares a method and marks a secret: a
// service's own methods and those it answers for its entries without declaring them, a method found by its models' names alone, a model of one
// argument and one of a whole answer, fields sent under an alias, types named before they are made, and an older version of the API that is
// not the one read.
var madeUpSource = map[string]string{
	"api/base/__init__.py": `from pydantic import Field, Secret


class BaseModel:
    pass


class Credentials(BaseModel):
    username: str
    password: Secret[str]
    pass_: Annotated[Secret[str], Field(alias="pass")]
    api_key: Secret[str] = Field(alias="apiKey")
    keys: list[Secret[str]] = []
    extra: dict[str, "Credentials"] = {}
`,
	"api/v1_9/thing.py": `class ThingEntry(BaseModel):
    old_secret: Secret[str]
`,
	"api/v1_10/thing.py": `from middlewared.api.base import BaseModel, Credentials, query_result, single_argument_args, single_argument_result


class ThingEntry(BaseModel):
    id: int
    name: str
    token: Secret[str] | None = None


class ThingCreateArgs(BaseModel):
    data: Credentials
    note: str = ""


class ThingCreateResult(BaseModel):
    result: ThingEntry


@single_argument_args("data")
class ThingLoginArgs(BaseModel):
    user: str
    otp: Secret[int]


@single_argument_result
class ThingLoginResult(BaseModel):
    session: Secret[str]
    expires: int


ThingQueryResult = query_result(ThingEntry)


class CloudThingVerifyArgs(BaseModel):
    credentials: Credentials


class CloudThingVerifyResult(BaseModel):
    result: bool
`,
	"plugins/thing/crud.py": `from middlewared.service import CRUDService, api_method, filterable_api_method


class ThingService(CRUDService):

    class Config:
        namespace = "thing"
        entry = ThingEntry
        cli_private = True

    @api_method(ThingCreateArgs, ThingCreateResult, roles=["THING_WRITE"])
    async def do_create(self, data, note):
        return {"token": f"{data["password"]!r:>{self.width}}"}

    @api_method(ThingLoginArgs, ThingLoginResult)
    def login(self, data):
        pass

    # private, so the query the service answers for its entries stands
    @api_method(ThingLoginArgs, ThingLoginResult, private=True)
    def query(self, filters, options):
        pass

    @filterable_api_method(item=ThingEntry)
    def listed(self, filters, options):
        pass
`,
	"plugins/elsewhere.py": `class SSHService(SystemServiceService):

    class Config:
        entry = SSHEntry


class ReplicationService(CRUDService):

    @api_method(ReplicationUpdateArgs, ReplicationUpdateResult)
    async def do_update(self, id_, data):
        pass


class KeychainCredentialService(CRUDService):

    class Config:
        namespace = "keychaincredential"

    @api_method(KeychainCredentialGenerateSshKeyPairArgs, KeychainCredentialGenerateSshKeyPairResult)
    def generate_ssh_key_pair(self):
        pass
`,
	"service/core.py": `class CoreService(Service):
    pass
`,
}

// aString is how the description writes a value that is text.
var aString = map[string]any{"type": "string"}

// anObject is how the description writes an object of these fields.
func anObject(fields map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": fields}
}

// method is how the description writes a method that takes these and answers that.
func method(accepts []any, returns any) map[string]any {
	return map[string]any{"accepts": accepts, "returns": []any{returns}}
}

// madeUpDescription is the description a NAS of the made-up source gives of its API.
func madeUpDescription() map[string]any {
	entry := anObject(map[string]any{"id": aString, "name": aString, "token": map[string]any{"anyOf": []any{aString, map[string]any{"type": "null"}}}})
	credentials := anObject(map[string]any{"username": aString, "password": aString, "pass": aString, "apiKey": aString, "keys": map[string]any{"type": "array", "items": aString}, "extra": map[string]any{"type": "object", "additionalProperties": true}})
	entries := map[string]any{"anyOf": []any{map[string]any{"type": "array", "items": entry}, entry, map[string]any{"type": "integer"}}}

	return map[string]any{
		"cloud_thing.verify":                       method([]any{credentials}, map[string]any{"type": "boolean"}),
		"keychaincredential.generate_ssh_key_pair": method(nil, anObject(map[string]any{"private_key": aString, "public_key": aString})),
		"replication.update":                       method([]any{map[string]any{"type": "integer"}, anObject(map[string]any{"encryption_key": aString})}, anObject(map[string]any{"encryption_key": aString})),
		"ssh.config":                               method(nil, anObject(map[string]any{"host_rsa_key": aString, "keys": map[string]any{"type": "array", "items": anObject(map[string]any{"privatekey": aString})}})),
		"thing.create":                             method([]any{credentials, aString}, entry),
		"thing.get_instance":                       method([]any{map[string]any{"type": "integer"}}, entry),
		"thing.listed":                             method(nil, entries),
		"thing.login":                              method([]any{anObject(map[string]any{"user": aString, "otp": map[string]any{"type": "integer"}})}, anObject(map[string]any{"session": aString, "expires": map[string]any{"type": "integer"}})),
		"thing.query":                              method(nil, entries),
	}
}

// writeSource writes the files of a made-up checkout under dir.
func writeSource(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, "src", "middlewared", "middlewared", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writeDescription writes a description of the API where the secrets command reads it.
func writeDescription(t *testing.T, defs, release string, description map[string]any) {
	t.Helper()

	out, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defs, "truenas-methods-"+release+".json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The secrets of every method are found in the source and written as places in its arguments and answer, those the source does not mark
// apart, in the layout Python's json.dumps wrote, so the file reads the same as it did when Python wrote it.
func TestTheSecretsAreReadFromTheSourceAndWrittenDown(t *testing.T) {
	t.Parallel()

	checkout, defs := t.TempDir(), t.TempDir()
	writeSource(t, checkout, madeUpSource)
	writeDescription(t, defs, "9.9", madeUpDescription())

	var said strings.Builder
	if err := secrets(t.Context(), defs, []string{checkout, "9.9"}, &said); err != nil {
		t.Fatalf("writing the secrets: %v\n%s", err, said.String())
	}

	written, err := os.ReadFile(filepath.Join(defs, "truenas-secrets-9.9.json")) //nolint:gosec // G304: the file the test had written
	if err != nil {
		t.Fatal(err)
	}
	// the made-up checkout is no git repository, so it has no commit, unless the folder tests are made in is inside one
	got := regexp.MustCompile(`"commit": "[0-9a-f]*"`).ReplaceAllString(string(written), `"commit": ""`)

	want := `{
  "source": {
    "repository": "https://github.com/truenas/middleware",
    "tag": "TS-9.9",
    "commit": "",
    "api": "v1_10"
  },
  "methods": {
    "cloud_thing.verify": {
      "params": [
        "$[0].apiKey",
        "$[0].keys[]",
        "$[0].pass",
        "$[0].password"
      ]
    },
    "thing.create": {
      "params": [
        "$[0].apiKey",
        "$[0].keys[]",
        "$[0].pass",
        "$[0].password"
      ],
      "result": [
        "$.token"
      ]
    },
    "thing.get_instance": {
      "result": [
        "$.token"
      ]
    },
    "thing.listed": {
      "result": [
        "$.token",
        "$[].token"
      ]
    },
    "thing.login": {
      "params": [
        "$[0].otp"
      ],
      "result": [
        "$.session"
      ]
    },
    "thing.query": {
      "result": [
        "$.token",
        "$[].token"
      ]
    }
  },
  "unmarked": {
    "ssh.config": {
      "result": [
        "$.host_rsa_key",
        "$.keys[].privatekey"
      ]
    },
    "replication.update": {
      "params": [
        "$[1].encryption_key"
      ],
      "result": [
        "$.encryption_key"
      ]
    },
    "keychaincredential.generate_ssh_key_pair": {
      "result": [
        "$.private_key"
      ]
    }
  },
  "not_read": []
}
`
	if got != want {
		t.Errorf("the secrets are written as\n%s\nwant\n%s", got, want)
	}

	for _, line := range []string{
		"truenas-secrets-9.9.json: 16 secret places in 6 of the 9 methods the NAS describes (source v1_10)",
		"secrets the source does not mark: 5 places in 3 methods",
		"methods the description has that this could not follow to their models: 0",
		"places the source marks that the description has nothing at: 0",
		"files of the source this could not read: 0",
	} {
		if !strings.Contains(said.String(), line+"\n") {
			t.Errorf("it said\n%s\nwhich lacks %q", said.String(), line)
		}
	}
}

// What cannot be trusted fails the run and is named: a method the source has nothing for, a secret the description has no place for, a kind
// of secret the source leaves unmarked that is nowhere to be found, and a file of the source that could not be read.
func TestWhatCannotBeTrustedIsNamedAndFailsTheRun(t *testing.T) {
	t.Parallel()

	checkout, defs := t.TempDir(), t.TempDir()
	broken := map[string]string{"plugins/broken.py": "x = \"never ends\n"}
	maps.Copy(broken, madeUpSource)
	writeSource(t, checkout, broken)

	description := madeUpDescription()
	description["core.ping"] = method(nil, aString)
	description["thing.login"] = method([]any{anObject(map[string]any{"user": aString})}, anObject(map[string]any{"session": aString}))
	delete(description, "ssh.config")
	writeDescription(t, defs, "9.9", description)

	var said strings.Builder
	err := secrets(t.Context(), defs, []string{checkout, "9.9"}, &said)
	if err == nil || err.Error() != "1 methods not followed, 2 places not described and 1 files not read" {
		t.Errorf("writing the secrets fails with %v; want the count of what could not be trusted", err)
	}

	for _, line := range []string{
		"methods the description has that this could not follow to their models: 1\n  core.ping\n",
		"places the source marks that the description has nothing at: 2\n  thing.login params $[0].otp\n  ssh.* unmarked privatekey, host_key, host_dsa_key, host_ecdsa_key, host_ed25519_key, host_rsa_key\n",
		"files of the source this could not read: 1\n",
		"broken.py: line 1: a string that runs off the end of its line\n",
	} {
		if !strings.Contains(said.String(), line) {
			t.Errorf("it said\n%s\nwhich lacks %q", said.String(), line)
		}
	}
}

// Without a checkout and a release it says how it is run.
func TestTheSecretsCommandSaysHowItIsRun(t *testing.T) {
	t.Parallel()

	if err := secrets(t.Context(), t.TempDir(), nil, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "go run ./sdk/pandorest secrets ../middleware 25.10.7") {
		t.Errorf("run with nothing, it fails with %v; want how it is run", err)
	}
}
