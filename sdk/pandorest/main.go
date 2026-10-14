// Command pandorest generates sdk/truenas from the description a TrueNAS gives of its own API, with github.com/katbyte/pandorest: the description is
// imported into definitions under api-defs, and the client is written from those.
//
// It is run from the repository root, as the make targets do:
//
//	go run ./sdk/pandorest import     # the description -> api-defs/truenas-<version>/
//	go run ./sdk/pandorest generate   # the definitions -> sdk/truenas
//	go run ./sdk/pandorest check      # the definitions match the description, and every method has one in sdk/truenas
package main

import (
	"github.com/katbyte/pandorest"
	"github.com/katbyte/pandorest/config"
	"github.com/katbyte/pandorest/importer/workarounds"
)

func main() {
	pandorest.Main(pandorest.Config{Services: services, Workarounds: fixes})
}

// fixes are what a real TrueNAS does that its description cannot say.
var fixes = []workarounds.Workaround{
	// a pool's scrub, expansion, auto-trim setting and disk layout, which the description leaves shapeless (pools.go)
	workarounds.DeclaredSchemas(workarounds.About{Name: "truenas-pool-records", Service: "truenas", Bug: "a pool's scrub, expansion, auto-trim setting and vdevs are described as values of no shape, and a NAS writes each the same way"}, poolShapes),
	workarounds.DeclaredShapes(workarounds.About{Name: "truenas-pool-shapes", Service: "truenas", Bug: "a pool's scan, expand and autotrim are described as records of no fields"}, poolFields),
	vdevLists{},
	datasetExtras{},
	// a task's state and its last run, which the description leaves shapeless (tasks.go)
	workarounds.DeclaredSchemas(workarounds.About{Name: "truenas-task-records", Service: "truenas", Bug: "a snapshot task's and a replication's state are described as values of no shape, and a NAS writes each the same way"}, taskShapes),
	workarounds.DeclaredShapes(workarounds.About{Name: "truenas-task-shapes", Service: "truenas", Bug: "a task's state and its last run (job) are described as values or records of no shape, and a NAS writes the job as core.get_jobs lists one"}, taskFields),
	// made from api-defs/truenas-secrets-<version>.json: every secret in an answer that would be read as something other than text, the members
	// of a value that is one of several records included
	workarounds.NamedTypes(workarounds.About{Name: "truenas-secrets", Service: "truenas", Bug: "a caller who is not a full administrator is sent ******** in place of a secret, whatever its type"}, config.DefaultClient, map[string]string{
		"SSHCredentialsEntry.attributes":            "client.Secret[SSHCredentials]",
		"SystemGeneralConfigAnswer.ui_certificate":  "client.Secret[map[string]any]",
		"SystemGeneralUpdateAnswer.ui_certificate":  "client.Secret[map[string]any]",
		"UserCreateAnswer.password_history":         "client.Secret[[]any]",
		"UserQueryResultItem.password_history":      "client.Secret[[]any]",
		"UserRenew2faSecretAnswer.password_history": "client.Secret[[]any]",
		"UserUpdateAnswer.password_history":         "client.Secret[[]any]",
		"VirtInstanceCreateAnswer.raw":              "client.Secret[map[string]any]",
		"VirtInstanceGetInstanceAnswer.raw":         "client.Secret[map[string]any]",
		"VirtInstanceQueryResultItem.raw":           "client.Secret[map[string]any]",
		"VirtInstanceUpdateAnswer.raw":              "client.Secret[map[string]any]",
		"SwiftCredentialsModel.auth_version":        "client.Secret[int]",
		"S3CredentialsModel.max_upload_parts":       "client.Secret[int]",
		"FTPCredentialsModel.port":                  "client.Secret[int]",
		"SFTPCredentialsModel.port":                 "client.Secret[int]",
		"SFTPCredentialsModel.private_key":          "client.Secret[int]",
		"S3CredentialsModel.signatures_v2":          "client.Secret[bool]",
		"S3CredentialsModel.skip_region":            "client.Secret[bool]",
	}),
	// a ZFS GUID is an unsigned 64-bit number, half of which do not fit the signed integer the description's "integer" becomes
	workarounds.NamedTypes(workarounds.About{Name: "truenas-guids", Service: "truenas", Bug: "zfs.resource.query writes a dataset's guid as a bare number, which is often larger than a signed 64-bit integer holds"}, "", map[string]string{
		"ZFSResourceEntry.guid": "uint64",
	}),
	workarounds.NamedTypes(workarounds.About{Name: "truenas-maybe-numbers", Service: "truenas", Bug: "a number the NAS leaves out or writes as null when it has none would read as 0, which is a real answer for these"}, "", map[string]string{
		"PoolScan.total_secs_left":                  "*int",
		"PoolExpand.total_secs_left":                "*int",
		"PoolDatasetQueryResultItem.snapshot_count": "*int",
	}),
	workarounds.NamedTypes(workarounds.About{Name: "truenas-sets", Service: "truenas", Bug: "a list marked as holding no repeats is written as a plain list in one answer and wrapped as a set in another"}, "github.com/katbyte/truenas-mcp/sdk/truenasrpc", map[string]string{ //nolint:gosec // G101: a rule for passwords is named here, and no password
		"SystemGeneralConfigAnswer.ui_httpsprotocols":            "truenasrpc.Set[$item]",
		"SystemGeneralUpdateAnswer.ui_httpsprotocols":            "truenasrpc.Set[$item]",
		"SystemSecurityConfigAnswer.password_complexity_ruleset": "truenasrpc.Set[$item]",
		"SystemSecurityUpdateAnswer.password_complexity_ruleset": "truenasrpc.Set[$item]",
	}),
}

// services is what truenas-mcp generates its client from.
var services = []config.Service{{
	Name:        "truenas",
	Package:     "truenas",
	Output:      "sdk/truenas",
	Document:    config.JSONRPCDocument,
	Naming:      config.OperationIDNaming,
	Grouped:     true,
	TypedUnions: true,
	// two groups whose names will not do as a package's: interface is a keyword, and truenas is the package of the types
	Packages: map[string]string{"Interface": "netinterface", "Truenas": "truenasservice"},
	// a moment is written {"$date": milliseconds}. A date written as text with no time zone, as a certificate's dates are, is in the NAS's own
	// zone, so it is left as text and not read as UTC
	Dates: config.Dates{Send: "$date", Read: []string{"2006-01-02T15:04:05Z07:00"}},
	// a method the read-only administrator may call is a read, but for the not reads. Reads are the methods that change nothing and name no role, as
	// anyone logged in may call them, or anyone at all
	ReadRole: "READONLY_ADMIN",
	Reads:    []string{"directoryservices.status", "auth.me", "core.get_jobs", "core.get_methods", "core.get_services", "privilege.roles", "system.version", "system.version_short", "core.ping", "system.boot_id", "system.advanced.login_banner", "truenas.managed_by_truecommand", "user.has_local_administrator_set_up"},
	NotReads: []string{"api_key.create", "api_key.update", "api_key.delete", "support.new_ticket", "support.attach_ticket"},
}}
