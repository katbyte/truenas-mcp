package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/katbyte/go-kt/pointer"
	"github.com/katbyte/pandorest/client"
	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Making, changing and removing datasets and volumes.

// datasetChangeOut is what making or changing a dataset answers.
type datasetChangeOut struct {
	Dataset *datasetRow `json:"dataset,omitempty" jsonschema:"the dataset as it is now, read back; absent on a dry run of making one"`
	changed
}

// datasetAbsent checks the NAS has no dataset by this whole name yet.
func datasetAbsent(ctx context.Context, r *registry, name string) error {
	_, err := r.typed.PoolDataset.QueryOne(ctx, []any{[]any{"name", "=", name}}, truenas.PoolDatasetQueryOptions{Select: []any{"name"}, Extra: namesOnly})
	switch {
	case err == nil:
		return fmt.Errorf("the NAS already has a dataset %s: dataset_update changes it", name)
	case truenasrpc.IsNotFound(err):
		return nil
	}

	return fmt.Errorf("reading the dataset %s: %w", name, err)
}

// datasetFound reads one dataset whole, saying what to do when the NAS has none by that name.
func datasetFound(ctx context.Context, r *registry, name string) (*truenas.PoolDatasetQueryResultItem, error) {
	found, err := r.typed.PoolDataset.QueryOne(ctx, []any{[]any{"name", "=", name}}, truenas.PoolDatasetQueryOptions{Extra: flat})
	switch {
	case truenasrpc.IsNotFound(err):
		return nil, fmt.Errorf("the NAS has no dataset %q: dataset_list lists them, by their whole name", name)
	case err != nil:
		return nil, fmt.Errorf("reading the dataset %s: %w", name, err)
	case found.Answer == nil:
		return nil, fmt.Errorf("the NAS answered nothing for the dataset %s", name)
	}

	return found.Answer, nil
}

// datasetRead reads a dataset back after a change. One that cannot be read was still changed, so the note says so rather than the call failing.
func datasetRead(ctx context.Context, r *registry, name string, c *changed) *datasetRow {
	d, err := datasetFound(ctx, r, name)
	if err != nil {
		c.noted("it could not be read back afterwards: %v", err)

		return nil
	}
	row := datasetRowOf(*d)

	return &row
}

// datasetsUnder is the whole names of every dataset and volume under one, in order.
func datasetsUnder(ctx context.Context, r *registry, name string) ([]string, error) {
	found, err := r.typed.PoolDataset.Query(ctx, []any{[]any{"name", "^", name + "/"}}, truenas.PoolDatasetQueryOptions{Select: []any{"name"}, Extra: namesOnly, OrderBy: []string{"name"}})
	if err != nil {
		return nil, fmt.Errorf("listing the datasets under %s: %w", name, err)
	}
	names := make([]string, 0, len(found.Answer))
	for _, d := range found.Answer {
		names = append(names, d.Name)
	}

	return names, nil
}

// datasetCalled is a dataset's whole name as given, and the dataset it is in; a name with nothing it is in is a pool's own dataset.
func datasetCalled(given string) (name, in string) {
	name = strings.Trim(strings.TrimSpace(given), "/")
	if i := strings.LastIndex(name, "/"); i > 0 {
		in = name[:i]
	}

	return name, in
}

func registerDatasetChanges(r *registry) {
	type datasetCreateIn struct {
		Dataset       string `json:"dataset"                  jsonschema:"the new dataset's whole name, its pool first (tank/media/films); the one it goes in has to be there already"`
		VolumeBytes   int64  `json:"volume_bytes,omitempty"   jsonschema:"make a volume of this many bytes, a disk to offer over iSCSI or to a virtual machine, rather than a dataset of files"`
		Sparse        bool   `json:"sparse,omitempty"         jsonschema:"for a volume, keep no space for it on the pool: it takes space as it is written, and writes to it fail if the pool fills first"`
		QuotaBytes    int64  `json:"quota_bytes,omitempty"    jsonschema:"the most it and everything under it may take, in bytes; no quota when not given"`
		RefquotaBytes int64  `json:"refquota_bytes,omitempty" jsonschema:"the most it may take itself, snapshots and children aside, in bytes; no such quota when not given"`
		Compression   string `json:"compression,omitempty"    jsonschema:"how what is written is compressed: LZ4, ZSTD, OFF and the like; what the dataset it goes in has when not given"`
		Comments      string `json:"comments,omitempty"       jsonschema:"what it is for, in words"`
		ReadOnly      bool   `json:"read_only,omitempty"      jsonschema:"make it so it can only be read"`
		DryRun        bool   `json:"dry_run,omitempty"        jsonschema:"say what would be made and make nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "dataset_create",
		Description: "Make a dataset, which holds files and is what shares and snapshot tasks are set on, or a volume: a disk of a set size to offer over iSCSI or to a virtual machine. It goes inside a pool or a dataset that is there already, named by its whole name, and has the settings of the one it is in unless given its own. dry_run says what would be made.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in datasetCreateIn) (*mcp.CallToolResult, datasetChangeOut, error) {
		name, parent := datasetCalled(in.Dataset)
		volume := in.VolumeBytes > 0
		switch {
		case parent == "":
			return nil, datasetChangeOut{}, errors.New("a dataset goes inside a pool or another dataset: give its whole name, the pool first (tank/media)")
		case in.VolumeBytes < 0 || in.QuotaBytes < 0 || in.RefquotaBytes < 0:
			return nil, datasetChangeOut{}, errors.New("a size is a number of bytes above 0")
		case volume && (in.QuotaBytes > 0 || in.RefquotaBytes > 0):
			return nil, datasetChangeOut{}, errors.New("a volume is as big as volume_bytes says, and takes no quota")
		case in.Sparse && !volume:
			return nil, datasetChangeOut{}, errors.New("sparse is for a volume: give volume_bytes, or leave sparse out for a dataset of files")
		}

		if err := datasetAbsent(ctx, r, name); err != nil {
			return nil, datasetChangeOut{}, err
		}
		if err := datasetNamed(ctx, r, parent); err != nil {
			return nil, datasetChangeOut{}, fmt.Errorf("%s would go in %s: %w", name, parent, err)
		}

		out := datasetChangeOut{Changes: []string{}, DryRun: in.DryRun}
		kind, with := "dataset", []string{}
		if volume {
			kind = "volume"
			with = append(with, "a size of "+sized(in.VolumeBytes))
			if in.Sparse {
				with = append(with, "no space kept for it")
			}
		}
		if in.QuotaBytes > 0 {
			with = append(with, "a quota of "+sized(in.QuotaBytes))
		}
		if in.RefquotaBytes > 0 {
			with = append(with, "a quota of its own of "+sized(in.RefquotaBytes))
		}
		compression := strings.ToUpper(strings.TrimSpace(in.Compression))
		if compression != "" {
			with = append(with, "compression "+compression)
		}
		if in.ReadOnly {
			with = append(with, "nothing but reading")
		}
		withWords := ""
		if len(with) > 0 {
			withWords = ", with " + strings.Join(with, ", ")
		}
		out.did("would make the %s %s in %s%s", "made the %s %s in %s%s", kind, name, parent, withWords)
		if in.DryRun {
			return nil, out, nil
		}

		readOnly := ""
		if in.ReadOnly {
			readOnly = "ON"
		}
		var record any = truenas.PoolDatasetCreateFilesystem{Name: name, Type: truenas.PoolDatasetCreateFilesystemTypeFILESYSTEM, Compression: truenas.PoolDatasetCompression(compression), Comments: in.Comments, Readonly: truenas.PoolDatasetCreateFilesystemReadonly(readOnly), Quota: positive(in.QuotaBytes), Refquota: positive(in.RefquotaBytes)}
		if volume {
			record = truenas.PoolDatasetCreateVolume{Name: name, Type: truenas.PoolDatasetCreateVolumeTypeVOLUME, Volsize: int(in.VolumeBytes), Sparse: pointer.To(in.Sparse), Compression: truenas.PoolDatasetCompression(compression), Comments: in.Comments, Readonly: truenas.PoolDatasetCreateVolumeReadonly(readOnly)}
		}
		data, err := client.UnionOf(record)
		if err != nil {
			return nil, out, fmt.Errorf("the dataset %s could not be written out, so nothing was made: %w", name, err)
		}
		if _, err := r.typed.PoolDataset.Create(ctx, truenas.PoolDatasetCreateData{Union: data}); err != nil {
			return nil, out, fmt.Errorf("the %s %s was not made: %w", kind, name, err)
		}
		out.Dataset = datasetRead(ctx, r, name, &out.changed)

		return nil, out, nil
	})

	type datasetUpdateIn struct {
		Dataset       string  `json:"dataset"                  jsonschema:"the dataset or volume, by its whole name (tank/media/films)"`
		QuotaBytes    *int64  `json:"quota_bytes,omitempty"    jsonschema:"the most it and everything under it may take, in bytes; 0 takes the quota away"`
		RefquotaBytes *int64  `json:"refquota_bytes,omitempty" jsonschema:"the most it may take itself, snapshots and children aside, in bytes; 0 takes the quota away"`
		VolumeBytes   int64   `json:"volume_bytes,omitempty"   jsonschema:"for a volume, the size to grow it to, in bytes; the NAS will not shrink one, which would lose what is at its end"`
		Compression   string  `json:"compression,omitempty"    jsonschema:"how what is written from now on is compressed: LZ4, ZSTD, OFF and the like; what is written already stays as it is"`
		Comments      *string `json:"comments,omitempty"       jsonschema:"what it is for, in words; empty takes them away"`
		ReadOnly      *bool   `json:"read_only,omitempty"      jsonschema:"whether it can only be read: shares, apps and tasks that write to it fail while it is"`
		DryRun        bool    `json:"dry_run,omitempty"        jsonschema:"say what would change and change nothing"`
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "dataset_update",
		Description: "Change a dataset's or volume's quotas, compression, comments or whether it can only be read, or grow a volume. Only what is given changes, and the answer says each setting it moved from what to what. A quota below what a dataset already holds stops every write to it. dry_run says what would change.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in datasetUpdateIn) (*mcp.CallToolResult, datasetChangeOut, error) {
		name, _ := datasetCalled(in.Dataset)
		d, err := datasetFound(ctx, r, name)
		if err != nil {
			return nil, datasetChangeOut{}, err
		}
		row := datasetRowOf(*d)
		volume := d.Type == "VOLUME"
		switch {
		case volume && (in.QuotaBytes != nil || in.RefquotaBytes != nil):
			return nil, datasetChangeOut{}, fmt.Errorf("%s is a volume, which is as big as it is made and takes no quota: volume_bytes grows it", name)
		case !volume && in.VolumeBytes != 0:
			return nil, datasetChangeOut{}, fmt.Errorf("%s is a dataset of files, which has no size of its own to grow: quota_bytes limits it", name)
		case in.VolumeBytes < 0 || pointer.From(in.QuotaBytes) < 0 || pointer.From(in.RefquotaBytes) < 0:
			return nil, datasetChangeOut{}, errors.New("a size is a number of bytes, 0 or above")
		case in.VolumeBytes > 0 && in.VolumeBytes < bytesOf(d.Volsize):
			return nil, datasetChangeOut{}, fmt.Errorf("%s is %s, and the NAS will not shrink a volume: what is written at its end would be lost", name, sized(bytesOf(d.Volsize)))
		}

		out := datasetChangeOut{Changes: []string{}, DryRun: in.DryRun}
		data := truenas.PoolDatasetUpdateData{}
		var moves settings
		if in.QuotaBytes != nil && *in.QuotaBytes != row.QuotaBytes {
			moves.move("the quota", sizedOrNone(row.QuotaBytes), sizedOrNone(*in.QuotaBytes))
			data.Quota = *in.QuotaBytes
		}
		if in.RefquotaBytes != nil && *in.RefquotaBytes != row.RefquotaBytes {
			moves.move("its own quota", sizedOrNone(row.RefquotaBytes), sizedOrNone(*in.RefquotaBytes))
			data.Refquota = *in.RefquotaBytes
		}
		if size := bytesOf(d.Volsize); in.VolumeBytes > 0 && in.VolumeBytes != size {
			moves.move("the size", sized(size), sized(in.VolumeBytes))
			data.Volsize = int(in.VolumeBytes)
		}
		if want := strings.ToUpper(strings.TrimSpace(in.Compression)); want != "" && want != row.Compression {
			moves.move("compression", row.Compression, want)
			data.Compression = truenas.PoolDatasetCompression(want)
		}
		var blank []string
		if was := commentsOf(d); in.Comments != nil && *in.Comments != was {
			moves.move("the comments", quotedOrNone(was), quotedOrNone(*in.Comments))
			data.Comments = *in.Comments
			if *in.Comments == "" {
				blank = append(blank, "comments")
			}
		}
		if in.ReadOnly != nil && *in.ReadOnly != row.ReadOnly {
			moves.move("read only", onOff(row.ReadOnly), onOff(*in.ReadOnly))
			data.Readonly = truenas.PoolDatasetUpdateDataReadonly(strings.ToUpper(onOff(*in.ReadOnly)))
		}
		moves.say(&out.changed)
		if in.DryRun || len(moves) == 0 {
			out.Dataset = &row

			return nil, out, nil
		}

		if len(moves) > len(blank) {
			if _, err := r.typed.PoolDataset.Update(ctx, name, data); err != nil {
				return nil, out, fmt.Errorf("%s was not changed: %w", name, err)
			}
		}
		if err := blankOut(ctx, r, "pool.dataset.update", name, blank); err != nil {
			return nil, out, fmt.Errorf("the comments on %s were not taken away: %w", name, err)
		}
		out.Dataset = datasetRead(ctx, r, name, &out.changed)

		return nil, out, nil
	})

	type datasetDeleteIn struct {
		Dataset   string `json:"dataset"             jsonschema:"the dataset or volume, by its whole name (tank/media/films)"`
		Recursive bool   `json:"recursive,omitempty" jsonschema:"also every dataset and volume under it, with all they hold: the NAS refuses to remove one with any under it otherwise"`
		Force     bool   `json:"force,omitempty"     jsonschema:"remove it even while something has its files open, which loses them what it had open"`
		Confirm   bool   `json:"confirm,omitempty"   jsonschema:"remove it: without this the answer says what would go and nothing is removed"`
	}
	type datasetDeleteOut struct {
		Dataset string   `json:"dataset"         jsonschema:"the dataset or volume, by its whole name"`
		Under   []string `json:"under,omitempty" jsonschema:"the datasets and volumes under it, which go with it"`
		removal
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "dataset_delete",
		Description: "Remove a dataset or volume and everything in it, for good: its files, its snapshots, and whatever the NAS has set on it, such as shares and tasks, which the NAS removes with it. One with datasets under it is refused unless recursive is given, and the answer names them first. A pool's own dataset goes only with the pool. Without confirm the answer says what would go and removes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in datasetDeleteIn) (*mcp.CallToolResult, datasetDeleteOut, error) {
		name, parent := datasetCalled(in.Dataset)
		if parent == "" {
			return nil, datasetDeleteOut{}, fmt.Errorf("%s is a pool's own dataset, which goes only with the pool", name)
		}
		d, err := datasetFound(ctx, r, name)
		if err != nil {
			return nil, datasetDeleteOut{}, err
		}
		under, err := datasetsUnder(ctx, r, name)
		if err != nil {
			return nil, datasetDeleteOut{}, err
		}
		attached, err := r.typed.PoolDataset.Attachments(ctx, name)
		if err != nil {
			return nil, datasetDeleteOut{}, fmt.Errorf("reading what is set on %s, so nothing was removed: %w", name, err)
		}
		withSnapshots := []string{name}
		if in.Recursive {
			withSnapshots = append(withSnapshots, under...)
		}
		snapshots, err := r.typed.PoolSnapshot.QueryCount(ctx, []any{[]any{"dataset", "in", withSnapshots}}, truenas.PoolSnapshotQueryOptions{})
		if err != nil {
			return nil, datasetDeleteOut{}, fmt.Errorf("counting the snapshots of %s, so nothing was removed: %w", name, err)
		}

		out := datasetDeleteOut{Dataset: name, Under: under}
		kind := "dataset"
		if d.Type == "VOLUME" {
			kind = "volume"
		}
		out.goes("the %s %s and all it holds, %s", kind, name, sized(bytesOf(d.Used)))
		if n := pointer.From(snapshots.Answer); n > 0 {
			out.goes("%s of it", several(n, "snapshot"))
		}
		if len(under) > 0 && in.Recursive {
			out.goes("%s under it, with all they hold: %s", several(len(under), "dataset or volume", "datasets and volumes"), inWords(under))
		}
		for _, a := range attached.Answer {
			out.goes("%s set on it, which the NAS removes with it: %s", a.Type, inWords(a.Attachments))
		}
		if !in.Confirm {
			out.Note = "nothing was removed: confirm removes it"
			if len(under) > 0 && !in.Recursive {
				out.Note = "nothing was removed: it has datasets under it, so confirm with recursive removes it and them"
			}

			return nil, out, nil
		}
		if len(under) > 0 && !in.Recursive {
			return nil, out, fmt.Errorf("%s has %s under it: %s. recursive removes them with it", name, several(len(under), "dataset or volume", "datasets and volumes"), inWords(under))
		}

		if _, err := r.typed.PoolDataset.Delete(ctx, name, truenas.PoolDatasetDeleteOptions{Recursive: pointer.To(in.Recursive), Force: pointer.To(in.Force)}); err != nil {
			return nil, out, fmt.Errorf("%s was not removed: %w", name, err)
		}
		switch err := datasetAbsent(ctx, r, name); {
		case err == nil:
		case strings.HasPrefix(err.Error(), "the NAS already has"):
			return nil, out, fmt.Errorf("the NAS answered that %s was removed, and still has it", name)
		default:
			out.Note = "it was removed, and could not be looked for afterwards: " + err.Error()
		}
		out.done()

		return nil, out, nil
	})
}

// positive is a size given as an argument, or nothing for none, so that a create leaves it out rather than sending 0.
func positive(n int64) any {
	if n <= 0 {
		return nil
	}

	return n
}

// quotedOrNone is a piece of text as a change says it: quoted, or none when it is empty.
func quotedOrNone(s string) string {
	if s == "" {
		return "none"
	}

	return strconv.Quote(s)
}
