package definitions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ServiceFile is the service description inside a definitions directory; every
// other JSON file there is one group.
const ServiceFile = "Service.json"

// GroupFile is the file a group's definitions are in: <Name>.json, but for a group named Service (TrueNAS has a service
// namespace), whose file would be the service description's, and is Service.group.json instead.
func GroupFile(name string) string {
	if name+".json" == ServiceFile {
		return name + ".group.json"
	}

	return name + ".json"
}

// Load reads a definitions directory.
func Load(dir string) (*Service, error) {
	var svc Service
	if err := readJSON(filepath.Join(dir, ServiceFile), &svc); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == ServiceFile || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		var g Group
		if err := readJSON(filepath.Join(dir, e.Name()), &g); err != nil {
			return nil, err
		}
		if want := GroupFile(g.Name); e.Name() != want {
			return nil, fmt.Errorf("%s: holds group %q, which belongs in %s", filepath.Join(dir, e.Name()), g.Name, want)
		}
		svc.Groups = append(svc.Groups, g)
	}
	slices.SortFunc(svc.Groups, func(a, b Group) int { return strings.Compare(a.Name, b.Name) })

	return &svc, nil
}

// Save writes a definitions directory, removing group files that no longer
// have a group so a tag that disappears from the document disappears here.
func Save(svc *Service, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // checked-in source, world-readable is right
		return err
	}

	want := map[string]bool{ServiceFile: true}
	if err := writeJSON(filepath.Join(dir, ServiceFile), svc); err != nil {
		return err
	}
	for i := range svc.Groups {
		name := GroupFile(svc.Groups[i].Name)
		if want[name] {
			return fmt.Errorf("two groups are named %q", svc.Groups[i].Name)
		}
		want[name] = true
		if err := writeJSON(filepath.Join(dir, name), &svc.Groups[i]); err != nil {
			return err
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" && !want[e.Name()] {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}

	return nil
}

// Marshal renders a value the way the definition files hold it: indented,
// without HTML escaping (descriptions carry < and &), with a final newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func writeJSON(path string, v any) error {
	b, err := Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	return os.WriteFile(path, b, 0o644) //nolint:gosec // checked-in source, world-readable is right
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path) //nolint:gosec // a definitions directory from the service config
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist; run the importer first", path)
		}
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	return nil
}
