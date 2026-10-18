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
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Taking, removing and going back to snapshots.

// snapshotCalled is a snapshot's whole name as given, split at its @, or an error saying how one is named.
func snapshotCalled(given string) (name, dataset string, err error) {
	name = strings.TrimSpace(given)
	dataset, short, ok := strings.Cut(name, "@")
	if !ok || dataset == "" || short == "" || strings.Contains(short, "@") {
		return "", "", fmt.Errorf("%q is no snapshot's whole name: one is named dataset@name, as snapshot_list answers it (tank/media@auto-2026-10-18_00-00)", given)
	}

	return name, dataset, nil
}

// snapshotThere reads a snapshot that has to be there, saying what to do when it is not.
func snapshotThere(ctx context.Context, r *registry, name, dataset string) (*truenas.PoolSnapshotQueryResultItem, error) {
	s, err := snapshotFound(ctx, r, name)
	switch {
	case err != nil:
		return nil, err
	case s == nil:
		return nil, fmt.Errorf("the NAS has no snapshot %s: snapshot_list lists the snapshots of %s", name, dataset)
	}

	return s, nil
}

// snapshotsNamed is which of these whole snapshot names the NAS has, among the snapshots of these datasets: asked for by name alone and dataset by
// dataset, which a NAS answers without reading every snapshot it has.
func snapshotsNamed(ctx context.Context, r *registry, datasets, names []string) ([]string, error) {
	if len(datasets) == 0 {
		return nil, nil
	}
	found, err := r.typed.PoolSnapshot.Query(ctx, []any{[]any{"dataset", "in", datasets}, []any{"name", "in", names}}, truenas.PoolSnapshotQueryOptions{Select: []any{"name"}, OrderBy: []string{"name"}})
	if err != nil {
		return nil, fmt.Errorf("looking for the snapshots %s: %w", inWords(names), err)
	}
	there := make([]string, 0, len(found.Answer))
	for _, s := range found.Answer {
		there = append(there, s.Name)
	}

	return there, nil
}

func registerSnapshotChanges(r *registry) {
	type snapshotCreateIn struct {
		Dataset   string `json:"dataset"             jsonschema:"the dataset or volume to take it of, by its whole name (tank/media)"`
		Name      string `json:"name"                jsonschema:"what to call it, the part after the @: tank/media@before-upgrade is named before-upgrade"`
		Recursive bool   `json:"recursive,omitempty" jsonschema:"also take one of every dataset and volume under it, by the same name and at the same moment"`
		DryRun    bool   `json:"dry_run,omitempty"   jsonschema:"say what would be taken and take nothing"`
	}
	type snapshotCreateOut struct {
		Snapshot *snapshotRow `json:"snapshot,omitempty" jsonschema:"the snapshot as it is now, read back; absent on a dry run"`
		changed
	}
	add(r, writeTool, &mcp.Tool{
		Name:        "snapshot_create",
		Description: "Take a snapshot of a dataset or volume: what it holds at this moment, kept until the snapshot is removed. It takes no space at first, and holds on to what is changed or removed after. snapshot_rollback goes back to one. dry_run says what would be taken.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotCreateIn) (*mcp.CallToolResult, snapshotCreateOut, error) {
		dataset, short := strings.Trim(strings.TrimSpace(in.Dataset), "/"), strings.TrimSpace(in.Name)
		switch {
		case dataset == "":
			return nil, snapshotCreateOut{}, errors.New("say which dataset to take a snapshot of: dataset_list names them")
		case short == "" || strings.ContainsAny(short, "@/ "):
			return nil, snapshotCreateOut{}, errors.New("name is what to call the snapshot, the part after the @, with no @, / or space in it: before-upgrade")
		}
		name := dataset + "@" + short

		if err := datasetNamed(ctx, r, dataset); err != nil {
			return nil, snapshotCreateOut{}, err
		}
		if s, err := snapshotFound(ctx, r, name); err != nil || s != nil {
			if err == nil {
				err = fmt.Errorf("the NAS already has a snapshot %s: a new one takes a name of its own", name)
			}

			return nil, snapshotCreateOut{}, err
		}

		out := snapshotCreateOut{Changes: []string{}, DryRun: in.DryRun}
		if in.Recursive {
			out.did("would take the snapshot %s, and one of each dataset under it by the same name", "took the snapshot %s, and one of each dataset under it by the same name", name)
		} else {
			out.did("would take the snapshot %s", "took the snapshot %s", name)
		}
		if in.DryRun {
			return nil, out, nil
		}

		data, err := client.UnionOf(truenas.PoolSnapshotCreateWithName{Dataset: dataset, Name: short, Recursive: pointer.To(in.Recursive)})
		if err != nil {
			return nil, out, fmt.Errorf("the snapshot %s could not be written out, so none was taken: %w", name, err)
		}
		if _, err := r.typed.PoolSnapshot.Create(ctx, truenas.PoolSnapshotCreateData{Union: data}); err != nil {
			return nil, out, fmt.Errorf("the snapshot %s was not taken: %w", name, err)
		}
		switch s, err := snapshotFound(ctx, r, name); {
		case err != nil:
			out.noted("it could not be read back afterwards: %v", err)
		case s == nil:
			return nil, out, fmt.Errorf("the NAS answered that it took the snapshot %s, and has none by that name", name)
		default:
			row := snapshotRowOf(s)
			out.Snapshot = &row
		}

		return nil, out, nil
	})

	type snapshotDeleteIn struct {
		Snapshot  string `json:"snapshot"            jsonschema:"the snapshot by its whole name, dataset@name, as snapshot_list answers it"`
		Recursive bool   `json:"recursive,omitempty" jsonschema:"also the snapshots of the same name of every dataset and volume under its dataset"`
		Confirm   bool   `json:"confirm,omitempty"   jsonschema:"remove it: without this the answer says what would go and nothing is removed"`
	}
	type snapshotDeleteOut struct {
		Snapshot string   `json:"snapshot"        jsonschema:"the snapshot, by its whole name"`
		Under    []string `json:"under,omitempty" jsonschema:"the snapshots of the same name under it, which go with it"`
		removal
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "snapshot_delete",
		Description: "Remove a snapshot for good, freeing the space only it holds: what the dataset held when it was taken can no longer be gone back to or copied from it. A held snapshot is refused: a replication holds the last one it sent so that the next can follow on from it. Without confirm the answer says what would go and removes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotDeleteIn) (*mcp.CallToolResult, snapshotDeleteOut, error) {
		name, dataset, err := snapshotCalled(in.Snapshot)
		if err != nil {
			return nil, snapshotDeleteOut{}, err
		}
		s, err := snapshotThere(ctx, r, name, dataset)
		if err != nil {
			return nil, snapshotDeleteOut{}, err
		}
		row := snapshotRowOf(s)

		out := snapshotDeleteOut{Snapshot: name}
		if in.Recursive {
			datasets, err := datasetsUnder(ctx, r, dataset)
			if err != nil {
				return nil, out, err
			}
			wanted := make([]string, 0, len(datasets))
			for _, d := range datasets {
				wanted = append(wanted, d+"@"+row.Snapshot)
			}
			if out.Under, err = snapshotsNamed(ctx, r, datasets, wanted); err != nil {
				return nil, out, err
			}
		}
		out.goes("the snapshot %s, taken %s, and the %s only it holds", name, orNone(row.Created), sized(row.UsedBytes))
		if len(out.Under) > 0 {
			out.goes("%s of the same name under it: %s", several(len(out.Under), "snapshot"), inWords(out.Under))
		}
		if row.Held {
			return nil, out, fmt.Errorf("the snapshot %s is held, so the NAS will not remove it: a replication holds the last snapshot it sent, so that the next can follow on from it", name)
		}
		if !in.Confirm {
			out.Note = "nothing was removed: confirm removes it"

			return nil, out, nil
		}

		if _, err := r.typed.PoolSnapshot.Delete(ctx, name, truenas.PoolSnapshotDeleteOptions{Recursive: pointer.To(in.Recursive)}); err != nil {
			return nil, out, fmt.Errorf("the snapshot %s was not removed: %w", name, err)
		}
		switch s, err := snapshotFound(ctx, r, name); {
		case err != nil:
			out.Note = "it was removed, and could not be looked for afterwards: " + err.Error()
		case s != nil:
			return nil, out, fmt.Errorf("the NAS answered that the snapshot %s was removed, and still has it", name)
		default:
		}
		out.done()

		return nil, out, nil
	})

	type snapshotRollbackIn struct {
		Snapshot    string `json:"snapshot"               jsonschema:"the snapshot to go back to, by its whole name, dataset@name"`
		RemoveNewer bool   `json:"remove_newer,omitempty" jsonschema:"also remove the dataset's snapshots taken after it, which the NAS otherwise refuses to go back past"`
		Confirm     bool   `json:"confirm,omitempty"      jsonschema:"go back: without this the answer says what would be lost and nothing changes"`
	}
	type snapshotRollbackOut struct {
		Snapshot   string   `json:"snapshot"           jsonschema:"the snapshot, by its whole name"`
		Newer      []string `json:"newer,omitempty"    jsonschema:"the dataset's snapshots taken after it, which go with what was written since"`
		WouldGo    []string `json:"would_go,omitempty" jsonschema:"what going back loses; absent once it has gone back"`
		RolledBack bool     `json:"rolled_back"        jsonschema:"whether the dataset was put back: without confirm it is not"`
		Note       string   `json:"note,omitempty"     jsonschema:"anything more going back needs said, such as what confirm does"`
	}
	add(r, deleteTool, &mcp.Tool{
		Name:        "snapshot_rollback",
		Description: "Put a dataset back as it was when one of its snapshots was taken, for good: everything written to it since is lost, and so are its snapshots taken after it, which the NAS refuses to go back past unless remove_newer is given. The answer names them first. Without confirm the answer says what would be lost and changes nothing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in snapshotRollbackIn) (*mcp.CallToolResult, snapshotRollbackOut, error) {
		name, dataset, err := snapshotCalled(in.Snapshot)
		if err != nil {
			return nil, snapshotRollbackOut{}, err
		}
		s, err := snapshotThere(ctx, r, name, dataset)
		if err != nil {
			return nil, snapshotRollbackOut{}, err
		}
		txg, err := strconv.ParseInt(s.Createtxg, 10, 64)
		if err != nil {
			return nil, snapshotRollbackOut{}, fmt.Errorf("the NAS did not say when the snapshot %s was taken, so which snapshots are newer cannot be told: %q", name, s.Createtxg)
		}

		// the snapshots written after it, by name alone and from this dataset alone, which a NAS answers without reading every snapshot it has
		newer, err := r.typed.PoolSnapshot.Query(ctx, []any{[]any{"dataset", "=", dataset}}, truenas.PoolSnapshotQueryOptions{Select: []any{"name"}, OrderBy: []string{"name"}, Extra: map[string]any{"min_txg": txg + 1}})
		if err != nil {
			return nil, snapshotRollbackOut{}, fmt.Errorf("listing the snapshots of %s taken after %s, so nothing changed: %w", dataset, name, err)
		}
		out := snapshotRollbackOut{Snapshot: name}
		for _, n := range newer.Answer {
			out.Newer = append(out.Newer, n.Name)
		}

		row := snapshotRowOf(s)
		out.WouldGo = append(out.WouldGo, fmt.Sprintf("everything written to %s since %s, when the snapshot was taken", dataset, orNone(row.Created)))
		if len(out.Newer) > 0 && in.RemoveNewer {
			out.WouldGo = append(out.WouldGo, fmt.Sprintf("%s of %s taken after it: %s", several(len(out.Newer), "snapshot"), dataset, inWords(out.Newer)))
		}
		if !in.Confirm {
			out.Note = "nothing changed: confirm goes back"
			if len(out.Newer) > 0 && !in.RemoveNewer {
				out.Note = "nothing changed: newer snapshots were taken, so confirm with remove_newer goes back and removes them"
			}

			return nil, out, nil
		}
		if len(out.Newer) > 0 && !in.RemoveNewer {
			return nil, out, fmt.Errorf("the NAS will not go back past the snapshots of %s taken after %s: %s. remove_newer removes them as it goes back", dataset, name, inWords(out.Newer))
		}

		if _, err := r.typed.PoolSnapshot.Rollback(ctx, name, truenas.PoolSnapshotRollbackOptions{Recursive: pointer.To(in.RemoveNewer)}); err != nil {
			return nil, out, fmt.Errorf("%s was not put back to %s: %w", dataset, name, err)
		}
		out.RolledBack, out.WouldGo = true, nil

		return nil, out, nil
	})
}
