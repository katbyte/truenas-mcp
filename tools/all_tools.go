// Package tools defines the MCP tools truenas-mcp serves, split by what they act on (server.go, ...). Tools are named resource first (server_*,
// pool_*, dataset_*, audit_*) so they group by what they act on.
//
// Every tool is registered through add with a kind: read tools never change what the NAS holds, write tools do and are only registered with
// --enable-write, and delete tools remove what cannot be put back and are only registered with --enable-delete as well. A session reads until it is
// told it may do more: a NAS holds everything else's data, and a wrong change loses it. --toolsets picks the groups a session needs, and --allow-tools
// and --deny-tools narrow them further.
package tools

import (
	"errors"

	mcpregistry "github.com/katbyte/go-kt/mcp/registry"
	"github.com/katbyte/truenas-mcp/sdk/truenas/client"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options controls which tools are registered.
type Options struct {
	// EnableWrite registers the tools that change something. Off unless the operator opts in: with nothing said, a session can only read.
	EnableWrite bool
	// EnableDelete registers the tools that delete. A delete is a change, so it needs EnableWrite too, and is refused without it rather than quietly
	// registering nothing.
	EnableDelete bool
	// Toolsets, when set, limits registration to the named groups (see Toolsets). Core is always included, so a set can be asked for on its own.
	// Allow adds to them and Deny takes away.
	Toolsets []string
	// Allow asks for tools by name: exact names, a pattern with one "*" at either end (pool_*, *_list), or the essential preset. Beside Toolsets it
	// adds to them; on its own it is only the tools it names. It lets nothing past EnableWrite or EnableDelete.
	Allow []string
	// Deny takes matching tools out of whatever Toolsets and Allow asked for: it is how a toolset is narrowed.
	Deny []string
}

// Toolsets group the tools by the job someone is doing, so a client loads the part it needs and not all of them: every tool's definition is spent
// from a client's context before anyone asks a question.
//
// "all" is every tool, which is what the package registers when no toolset is asked for; the binary defaults to core instead (see
// cli.DefaultToolsets). --toolsets also takes a resource family, every tool with that prefix ("pool" for pool_*), read off the tools themselves.
//
// Every tool belongs to exactly one set (TestEveryToolIsInExactlyOneToolset proves it), and core comes along with whatever else is asked for.
var Toolsets = map[string][]string{
	// enough to know which NAS this is, find its pools and datasets, and see what it says is wrong: the base every other set assumes
	mcpregistry.Core: {
		"server_info", "pool_list", "pool_get", "dataset_list", "dataset_get", "alert_list",
	},
	// find what is wrong: each audit only reports, and changes nothing
	"audit": {
		"audit_all", "audit_pools", "audit_space", "audit_tasks", "audit_disks", "audit_system",
	},
	// the machine's disks, whatever pool they are in
	"disks": {
		"disk_list",
	},
	// what keeps the data safe: snapshots and the tasks that take them, copy data elsewhere, and check it
	"protection": {
		"snapshot_list", "snapshottask_list", "replication_list", "rsynctask_list", "cloudsync_list", "cloud_backup_list", "pool_scrub_list",
	},
	// what the NAS offers other machines: SMB and NFS shares, and iSCSI targets with what they offer
	"shares": {
		"sharing_smb_list", "sharing_nfs_list", "iscsi_target_list", "iscsi_extent_list",
	},
	// the services that serve the shares and everything else
	"services": {
		"service_list",
	},
	// the apps the NAS runs in containers
	"apps": {
		"app_list",
	},
	// the virtual machines it runs
	"vms": {
		"vm_list",
	},
	// how it is reached
	"networking": {
		"interface_list", "network_configuration_get",
	},
	// who may use it
	"accounts": {
		"user_list", "group_list",
	},
}

// ToolsetOrder is the order a listing shows the sets in: the base first.
var ToolsetOrder = []string{mcpregistry.Core, "audit", "disks", "protection", "shares", "services", "apps", "vms", "networking", "accounts"}

// EssentialTools is the preset --allow-tools essential asks for: enough to know which NAS this is, what it holds and what it says is wrong.
var EssentialTools = []string{
	"server_info", "pool_list", "dataset_list", "alert_list",
}

// The kinds a tool is added with, in go-kt's registry's terms: a read tool never changes what the NAS holds, a write tool does, and a delete tool
// removes what cannot be put back.
const (
	readTool = mcpregistry.Read
)

// registry is this application's tools and what they share. Which of them a session gets, and what each tells a client about itself, is go-kt's
// registry's work, so the allow and deny patterns are checked against every tool before any is registered.
type registry struct {
	// nas is the connection to the one TrueNAS this process works on, and typed the client generated from its description, which calls through it
	nas   *truenasrpc.Conn
	typed *client.Client
	opts  Options
	// tools holds every tool queued, made when the first is (see queued)
	tools *mcpregistry.Registry
	// errorLog is where a handler's panic is logged; nil is the process's log
	errorLog func(format string, args ...any)
}

// toolHints are what a write tool tells a client about itself beyond its kind: additive when it only ever adds, idempotent when calling it again
// with the same arguments changes nothing more. Every write tool has an entry, decided on its own; one missing is marked destructive, and fails
// TestEveryWriteToolIsHinted.
var toolHints = map[string]mcpregistry.Hints{}

// queued is every tool queued so far, in go-kt's registry, which is made when the first tool is.
func (r *registry) queued() *mcpregistry.Registry {
	if r.tools == nil {
		r.tools = mcpregistry.New(mcpregistry.Config{
			Toolsets:       Toolsets,
			Essential:      EssentialTools,
			Hints:          toolHints,
			LogError:       r.errorLog,
			CleanAnswers:   answerLength,
			LeaveAsIs:      handles,
			BlankAddresses: serverSaid,
		})
	}

	return r.tools
}

// add queues a typed tool. go-kt's registry sets its annotations from its kind and hints, turns a panic into an error, and sends an empty list as []
// rather than null, which a client would read as "not fetched".
func add[In, Out any](r *registry, kind mcpregistry.Kind, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcpregistry.Add(r.queued(), kind, t, h)
}

// queueTools queues every tool there is; which of them a session gets is decided after.
func queueTools(r *registry) {
	registerServerTools(r)
	registerPoolTools(r)
	registerDatasetTools(r)
	registerDiskTools(r)
	registerAlertTools(r)
	registerProtectionTools(r)
	registerSharingTools(r)
	registerServiceTools(r)
	registerAppTools(r)
	registerVMTools(r)
	registerNetworkTools(r)
	registerAccountTools(r)
	registerPoolAudits(r)
	registerSpaceAudits(r)
	registerTaskAudits(r)
	registerDiskAudits(r)
	registerSystemAudits(r)
	registerAllAudit(r)
}

// check refuses options that contradict each other.
func (o *Options) check() error {
	if o.EnableDelete && !o.EnableWrite {
		return errors.New("--enable-delete needs --enable-write (TRUENAS_ENABLE_WRITE=true) beside it: a delete is a change, and no change is made without that")
	}

	return nil
}

// selection is the options in go-kt's registry's terms.
func (o *Options) selection() mcpregistry.Selection {
	return mcpregistry.Selection{
		ReadOnly:     !o.EnableWrite,
		EnableDelete: o.EnableDelete,
		Toolsets:     o.Toolsets,
		Allow:        o.Allow,
		Deny:         o.Deny,
	}
}

// RegisterAll adds every tool the options permit to the MCP server and returns the names registered. A toolset or pattern that names no tool is
// refused, so a typo cannot quietly hide one. The NAS is not called: one that is down is something the tools report, not a reason to have none.
func RegisterAll(server *mcp.Server, nas *truenasrpc.Conn, opts Options) ([]string, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	typed, err := client.New(nas)
	if err != nil {
		return nil, err
	}
	r := &registry{nas: nas, typed: typed, opts: opts}
	queueTools(r)

	return r.queued().Register(server, opts.selection())
}

// ToolInfo describes a tool the options would register.
type ToolInfo = mcpregistry.Info

// described is every tool queued with no NAS behind it, for the answers that need none: registering a tool never calls the NAS, only calling it does.
func described(opts Options) *mcpregistry.Registry {
	r := &registry{opts: opts}
	queueTools(r)

	return r.queued()
}

// Describe lists the tools the options would register, for `truenas-mcp tools`. It needs no NAS, and makes the same choice RegisterAll does.
func Describe(opts Options) ([]ToolInfo, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}

	return described(opts).Describe(opts.selection())
}

// ToolsetNames lists the curated toolsets, for help output.
func ToolsetNames() []string {
	return mcpregistry.New(mcpregistry.Config{Toolsets: Toolsets}).ToolsetNames()
}

// FamilyNames lists the resource prefixes --toolsets takes, for help output.
func FamilyNames() []string {
	return described(Options{}).FamilyNames()
}
