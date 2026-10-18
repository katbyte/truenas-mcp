package tools

// answerLength is the most of any one piece of text an answer carries: what came from the NAS is cleaned of what does not show and cut to this.
const answerLength = 16 << 10

// handles are the answer fields, by JSON name, that a later call hands back or a person types in: a pool's or a dataset's name, a disk's serial
// number. They go back exactly as they came, so the registry leaves them uncleaned.
var handles = []string{"id", "name", "pool", "disk", "serial", "mountpoint", "encryption_root", "origin", "address", "hostname", "user", "dataset", "path", "source_datasets", "target_dataset", "snapshot", "last_snapshot", "naming_schema", "credential", "username", "service", "remote", "primary_group", "maproot_user", "mapall_user", "members", "vlan_parent", "under", "newer"}

// serverSaid are the answer fields that carry the NAS's own words for what is wrong. Such words can quote an address with a key in it, which was
// never the caller's to see, so an address under one of these names loses its sign-in, its query and what follows a #.
var serverSaid = []string{"text", "status_detail", "note", "error"}
