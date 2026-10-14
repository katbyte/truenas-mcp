package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// datasetRow is what a list says of a dataset: where it is, what it holds, and what limits it.
type datasetRow struct {
	Name           string  `json:"name"                     jsonschema:"the dataset's whole name, its pool first (tank/media/films), which dataset_get takes"`
	Type           string  `json:"type"                     jsonschema:"FILESYSTEM, or VOLUME for a block device such as an iSCSI disk"`
	Pool           string  `json:"pool"                     jsonschema:"the pool it is on"`
	UsedBytes      int64   `json:"used_bytes"               jsonschema:"what it and everything under it take, snapshots included, in bytes"`
	AvailableBytes int64   `json:"available_bytes"          jsonschema:"how much more it can take, in bytes: the pool's free space, or less under a quota"`
	QuotaBytes     int64   `json:"quota_bytes,omitempty"    jsonschema:"the most it and everything under it may take, in bytes; none when it has no quota"`
	RefquotaBytes  int64   `json:"refquota_bytes,omitempty" jsonschema:"the most it may take itself, snapshots and children aside, in bytes; none when it has no such quota"`
	Compression    string  `json:"compression"              jsonschema:"how what is written is compressed: LZ4, ZSTD, OFF"`
	CompressRatio  float64 `json:"compress_ratio"           jsonschema:"how much compression saves: 1.5 is a third smaller"`
	Encrypted      bool    `json:"encrypted"                jsonschema:"whether it is encrypted"`
	Locked         bool    `json:"locked"                   jsonschema:"whether it is encrypted and its key is not loaded, so nothing in it can be read: shares and tasks on it stop"`
	ReadOnly       bool    `json:"read_only"                jsonschema:"whether it can only be read"`
	Mountpoint     string  `json:"mountpoint,omitempty"     jsonschema:"where its files are, on the NAS; none for a volume, or a locked dataset"`
}

// datasetRowOf is a dataset as a list says of it.
func datasetRowOf(d truenas.PoolDatasetQueryResultItem) datasetRow {
	row := datasetRow{Name: d.Name, Type: d.Type, Pool: d.Pool, Encrypted: d.Encrypted, Locked: d.Locked, Mountpoint: d.Mountpoint}
	row.UsedBytes, row.AvailableBytes, row.QuotaBytes, row.RefquotaBytes = bytesOf(d.Used), bytesOf(d.Available), bytesOf(d.Quota), bytesOf(d.Refquota)
	row.Compression, row.ReadOnly = strings.ToUpper(property(d.Compression)), property(d.Readonly) == "on"
	row.CompressRatio, _ = strconv.ParseFloat(strings.TrimSuffix(property(d.Compressratio), "x"), 64)

	return row
}

// flat asks for a list of datasets as rows alone: each with no copy of everything under it, which the NAS otherwise nests in every row. Asked this way
// with no filter, a NAS goes no deeper than each pool's own dataset, so such a query always carries everyDataset at least.
var flat = map[string]any{"flat": true, "retrieve_children": false}

// everyDataset is a filter every dataset and volume passes, for a query that would otherwise have none.
var everyDataset = []any{"type", "in", []string{"FILESYSTEM", "VOLUME"}}

// namesOnly asks for the datasets' names and nothing else, so that counting them is cheap.
var namesOnly = map[string]any{"flat": true, "retrieve_children": false, "properties": []string{}, "retrieve_user_props": false}

func registerDatasetTools(r *registry) {
	type datasetListIn struct {
		Pool  string `json:"pool,omitempty"  jsonschema:"only the datasets on this pool"`
		Under string `json:"under,omitempty" jsonschema:"only the datasets under this one, by its whole name; it is not listed itself"`
		pageIn
	}
	type datasetListOut struct {
		Datasets []datasetRow `json:"datasets"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "dataset_list",
		Description: "List the NAS's datasets and volumes by their whole name, on every pool or one, or under one dataset: each one's space, quotas, compression and whether it is encrypted and locked. The NAS pages the list itself, so a NAS of thousands is read a page at a time. dataset_get describes one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in datasetListIn) (*mcp.CallToolResult, datasetListOut, error) {
		filters := []any{everyDataset}
		if in.Pool != "" {
			filters = append(filters, []any{"pool", "=", in.Pool})
		}
		if in.Under != "" {
			filters = append(filters, []any{"name", "^", strings.TrimSuffix(in.Under, "/") + "/"})
		}

		// a NAS asked to count datasets counts every one whatever the filters, so the total is how many names pass them
		names, err := r.typed.PoolDataset.Query(ctx, filters, truenas.PoolDatasetQueryOptions{Extra: namesOnly, Select: []any{"name"}})
		if err != nil {
			return nil, datasetListOut{}, fmt.Errorf("counting the datasets: %w", err)
		}
		limit, offset := in.args()
		found, err := r.typed.PoolDataset.Query(ctx, filters, truenas.PoolDatasetQueryOptions{Extra: flat, OrderBy: []string{"name"}, Limit: limit, Offset: offset})
		if err != nil {
			return nil, datasetListOut{}, fmt.Errorf("listing the datasets: %w", err)
		}

		out := datasetListOut{Datasets: make([]datasetRow, 0, len(found.Answer))}
		for _, d := range found.Answer {
			out.Datasets = append(out.Datasets, datasetRowOf(d))
		}
		out.pageOut = paged(offset, len(out.Datasets), len(names.Answer))

		return nil, out, nil
	})

	type datasetGetIn struct {
		Dataset string `json:"dataset" jsonschema:"the dataset, by its whole name (tank/media/films)"`
	}
	type datasetGetOut struct {
		datasetRow
		SnapshotsBytes      int64  `json:"snapshots_bytes"                jsonschema:"what its snapshots alone hold on to, in bytes"`
		ChildrenBytes       int64  `json:"children_bytes"                 jsonschema:"what the datasets under it take, in bytes"`
		ReservationBytes    int64  `json:"reservation_bytes,omitempty"    jsonschema:"space kept for it and everything under it whether used or not, in bytes"`
		RefreservationBytes int64  `json:"refreservation_bytes,omitempty" jsonschema:"space kept for it alone whether used or not, in bytes: a thick volume keeps its whole size"`
		VolumeBytes         int64  `json:"volume_bytes,omitempty"         jsonschema:"for a volume, how big a disk it is, in bytes"`
		RecordSize          string `json:"record_size,omitempty"          jsonschema:"the largest block a file is written in"`
		Sync                string `json:"sync,omitempty"                 jsonschema:"STANDARD, ALWAYS, or DISABLED, which loses the last seconds of writes on a power cut"`
		Deduplication       string `json:"deduplication,omitempty"        jsonschema:"whether blocks written twice are kept once"`
		Atime               string `json:"atime,omitempty"                jsonschema:"whether reading a file notes when it was read"`
		EncryptionRoot      string `json:"encryption_root,omitempty"      jsonschema:"the dataset whose key unlocks this one"`
		KeyFormat           string `json:"key_format,omitempty"           jsonschema:"HEX or PASSPHRASE: a passphrase has to be given again after the NAS restarts"`
		Origin              string `json:"origin,omitempty"               jsonschema:"the snapshot it was cloned from, for a clone"`
		Comments            string `json:"comments,omitempty"             jsonschema:"what someone wrote about it"`
		Created             string `json:"created,omitempty"              jsonschema:"when it was made"`
	}
	add(r, readTool, &mcp.Tool{
		Name:        "dataset_get",
		Description: "Describe one dataset or volume: its space, down to what its snapshots and children hold, its quotas and reservations, and how it is written, encrypted and kept.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in datasetGetIn) (*mcp.CallToolResult, datasetGetOut, error) {
		found, err := r.typed.PoolDataset.QueryOne(ctx, []any{[]any{"name", "=", in.Dataset}}, truenas.PoolDatasetQueryOptions{Extra: flat})
		switch {
		case truenasrpc.IsNotFound(err):
			return nil, datasetGetOut{}, fmt.Errorf("the NAS has no dataset %q: dataset_list lists them, by their whole name", in.Dataset)
		case err != nil:
			return nil, datasetGetOut{}, fmt.Errorf("reading the dataset %s: %w", in.Dataset, err)
		}
		d := found.Answer
		if d == nil {
			return nil, datasetGetOut{}, fmt.Errorf("the NAS answered nothing for the dataset %s", in.Dataset)
		}

		out := datasetGetOut{datasetRow: datasetRowOf(*d), EncryptionRoot: d.EncryptionRoot, Origin: property(d.Origin), Comments: property(d.Comments)}
		out.SnapshotsBytes, out.ChildrenBytes, out.ReservationBytes, out.RefreservationBytes, out.VolumeBytes = bytesOf(d.Usedbysnapshots), bytesOf(d.Usedbychildren), bytesOf(d.Reservation), bytesOf(d.Refreservation), bytesOf(d.Volsize)
		out.RecordSize, out.Sync, out.Deduplication, out.Atime = property(d.Recordsize), strings.ToUpper(property(d.Sync)), strings.ToUpper(property(d.Deduplication)), strings.ToUpper(property(d.Atime))
		out.KeyFormat = strings.ToUpper(property(d.KeyFormat))
		if seconds, err := strconv.ParseInt(property(d.Creation), 10, 64); err == nil {
			out.Created = when(time.Unix(seconds, 0))
		}

		return nil, out, nil
	})
}
