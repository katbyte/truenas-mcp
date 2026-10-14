// Package pandorest generates typed Go Services from vendored OpenAPI documents.
// It is inspired by Pandora, the go-azure-sdk generator
// (https://github.com/hashicorp/pandora), and keeps its pipeline, scaled down
// to a handful of documents in one repository:
//
//	import    <dir>/<service>-openapi-<version>.json -> workarounds -> <dir>/<service>-<version>/*.json
//	generate  <dir>/<service>-<version> -> the service's package
//
// A service's document and definitions are named by the document's version,
// and the highest version present is the one imported and generated from.
//
//	diff      what a refreshed spec changes, against the checked-in definitions
//	check     the definitions match the spec and the package has every method
//	resolve   which document and definitions each service is at
//
// A repository that generates an SDK keeps what is its own - the documents,
// the services they describe and the workarounds for their bugs - and a main
// of a few lines that hands them to this package:
//
//	func main() {
//		pandorest.Main(pandorest.Config{Services: services, Workarounds: workarounds})
//	}
//
// which it runs from its root, as its make targets do:
//
//	go run ./sdk/pandorest import
//	go run ./sdk/pandorest generate -service jellyfin
//	go run ./sdk/pandorest diff
//	go run ./sdk/pandorest diff -old api-defs/emby-4.9.0.30 -new api-defs/emby-4.10.0.40
//
// The packages it writes send their requests through the client package, the
// base client they share. See the README for the design and how to refresh a
// spec.
package pandorest

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/katbyte/pandorest/config"
	"github.com/katbyte/pandorest/definitions"
	"github.com/katbyte/pandorest/differ"
	"github.com/katbyte/pandorest/generator"
	"github.com/katbyte/pandorest/importer"
	"github.com/katbyte/pandorest/importer/workarounds"
)

// Config is what a repository generates its Services from.
type Config struct {
	// Services are the APIs, in the order the commands process them.
	Services []config.Service
	// Workarounds are the fixes for the bugs in the services' documents, in
	// the order they are applied; each names the service it is for.
	Workarounds []workarounds.Workaround
}

// ErrChanges is what diff -exit-code returns when there are changes.
var ErrChanges = errors.New("the definitions differ")

const usage = `usage: pandorest <import|generate|diff|check|resolve> [flags]

  import    read each service's spec into its definitions directory, applying the workarounds
  generate  write the SDK packages from those definitions
  diff      report what the specs change against the checked-in definitions
  check     verify the definitions match the specs and the packages have every method, and count the methods the repository's own files name
  resolve   print each service's version, document and definitions

a service is its highest version alone, or every version it vendors when its config says where each goes (Outputs)

run "pandorest <command> -h" for a command's flags`

// The commands.
const (
	cmdImport   = "import"
	cmdGenerate = "generate"
	cmdDiff     = "diff"
	cmdCheck    = "check"
	cmdResolve  = "resolve"
)

// Main runs the command the process was started with, and exits 1 when it
// fails. It is the whole of a repository's generator main.
func Main(cfg Config) {
	if err := Run(cfg, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pandorest:", err)
		os.Exit(1) //nolint:revive // this is a repository's main, in all but name: it is what exits
	}
}

// Run runs one command: args are the command and its flags, stdout takes what
// it reports and stderr its log.
func Run(cfg Config, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd, args := args[0], args[1:]

	fs := flag.NewFlagSet("pandorest "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	services := fs.String("service", "", "comma-separated services to process (default all: "+strings.Join(config.Names(cfg.Services), ", ")+")")
	root := fs.String("root", ".", "repository root the config paths are relative to")
	version := fs.String("version", "", "one version of a service generated per version (default all of them)")
	quiet := fs.Bool("quiet", false, "do not log each workaround and warning")
	var oldDir, newDir *string
	var exitCode, uncalled *bool
	var callers *string
	if cmd == cmdCheck {
		uncalled = fs.Bool("uncalled", false, "list the generated methods no hand-written file names (tests do not count)")
		callers = fs.String("callers", ".", "the directory whose hand-written files count as naming a method, tests left out, relative to the root (default the whole repository)")
	}
	if cmd == cmdDiff {
		oldDir = fs.String("old", "", "definitions directory to compare from (default: the checked-in definitions)")
		newDir = fs.String("new", "", "definitions directory to compare to (default: a fresh import of the spec)")
		exitCode = fs.Bool("exit-code", false, "exit 1 when there are changes")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := func(msg string) { _, _ = fmt.Fprintln(stderr, "pandorest: "+msg) }
	if *quiet {
		log = func(string) {}
	}

	if cmd == cmdDiff && (*oldDir != "") != (*newDir != "") {
		return errors.New("diff: pass both -old and -new, or neither")
	}
	if cmd == cmdDiff && *oldDir != "" {
		differs, err := diffDirs(*oldDir, *newDir, stdout)
		if err == nil && differs && *exitCode {
			return ErrChanges
		}

		return err
	}

	selected, err := config.Select(cfg.Services, *services)
	if err != nil {
		return err
	}
	changed := false
	for _, selected := range selected {
		versions, err := selected.In(*root).ResolveAll()
		if err != nil {
			return err
		}
		if *version != "" {
			versions = slices.DeleteFunc(versions, func(s config.Service) bool { return s.Version != *version })
			if len(versions) == 0 {
				return fmt.Errorf("%s: no version %s is vendored", selected.Name, *version)
			}
		}
		for _, svc := range versions {
			listing := io.Discard
			if uncalled != nil && *uncalled {
				listing = stdout
			}
			if err := runOne(cmd, svc, cfg, log, stdout, &changed, listing, callers); err != nil {
				return err
			}
		}
	}
	if cmd == cmdDiff && changed && *exitCode {
		return ErrChanges
	}

	return nil
}

// runOne runs one command on one service, resolved to one version.
// listing is where check lists the methods no hand-written file names: stdout when asked, else nowhere.
func runOne(cmd string, svc config.Service, cfg Config, log func(string), stdout io.Writer, changed *bool, listing io.Writer, callers *string) error {
	var err error
	switch cmd {
	case cmdImport:
		err = importService(svc, cfg.Workarounds, log, stdout)
	case cmdGenerate:
		err = generateService(svc, stdout)
	case cmdDiff:
		var differs bool
		differs, err = diffService(svc, cfg.Workarounds, log, stdout)
		*changed = *changed || differs
	case cmdCheck:
		err = checkService(svc, cfg.Workarounds, log, stdout, listing, *callers)
	case cmdResolve:
		_, _ = fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", svc.Name, svc.Version, svc.Spec, svc.Definitions)
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}

	return err
}

func importService(svc config.Service, all []workarounds.Workaround, log func(string), stdout io.Writer) error {
	defs, err := importer.Import(svc, all, log)
	if err != nil {
		return err
	}
	if err := definitions.Save(defs, svc.Path(svc.Definitions)); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s: %d operations, %d models, %d constants, %d workarounds -> %s\n", svc.Name, len(defs.Operations()), len(defs.Models()), len(defs.Constants()), len(defs.Workarounds), svc.Path(svc.Definitions))

	return nil
}

func generateService(svc config.Service, stdout io.Writer) error {
	defs, err := definitions.Load(svc.Path(svc.Definitions))
	if err != nil {
		return err
	}
	importPath, err := importPath(svc)
	if err != nil {
		return err
	}
	n, err := generator.Generate(defs, generator.Options{Dir: svc.Path(svc.Output), Definitions: svc.Definitions, Version: svc.Version, ClientImport: cmp.Or(svc.ClientImport, config.DefaultClient), Client: svc.Client, ClientService: svc.ClientService, Credential: svc.Credential, NewDoc: svc.NewDoc, Notes: svc.Notes, Grouped: svc.Grouped, Packages: svc.Packages, ImportPath: importPath, InlineSingleOptions: svc.InlineSingleOptions, ClientPackage: svc.ClientPackage})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s: %d files -> %s\n", svc.Name, n, svc.Path(svc.Output))

	return nil
}

// importPath is the import path of a grouped service's Output, which its
// packages import one another by: Output under the module path, from the
// config (Module) or the repository's go.mod.
func importPath(svc config.Service) (string, error) {
	if !svc.Grouped {
		return "", nil
	}
	if svc.Module != "" {
		return path.Join(svc.Module, filepath.ToSlash(svc.Output)), nil
	}
	src, err := os.ReadFile(svc.Path("go.mod")) //nolint:gosec // the repository root, from the -root flag
	if err != nil {
		return "", fmt.Errorf("%s: the grouped layout needs the import path of %s, and neither the config (Module) nor go.mod says it: %w", svc.Name, svc.Output, err)
	}
	for line := range strings.SplitSeq(string(src), "\n") {
		if module, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return path.Join(strings.TrimSpace(module), filepath.ToSlash(svc.Output)), nil
		}
	}

	return "", fmt.Errorf("%s: the grouped layout needs the import path of %s, and go.mod names no module: set Module", svc.Name, svc.Output)
}

func diffService(svc config.Service, all []workarounds.Workaround, log func(string), stdout io.Writer) (bool, error) {
	checkedIn, err := definitions.Load(svc.Path(svc.Definitions))
	if err != nil {
		return false, err
	}
	fresh, err := importer.Import(svc, all, log)
	if err != nil {
		return false, err
	}
	report := differ.Diff(checkedIn, fresh)
	_, _ = fmt.Fprint(stdout, report.String())

	return !report.Empty(), nil
}

func diffDirs(oldDir, newDir string, stdout io.Writer) (bool, error) {
	older, err := definitions.Load(oldDir)
	if err != nil {
		return false, err
	}
	newer, err := definitions.Load(newDir)
	if err != nil {
		return false, err
	}
	report := differ.Diff(older, newer)
	_, _ = fmt.Fprint(stdout, report.String())

	return !report.Empty(), nil
}

func checkService(svc config.Service, all []workarounds.Workaround, log func(string), stdout, listing io.Writer, callers string) error {
	checkedIn, err := definitions.Load(svc.Path(svc.Definitions))
	if err != nil {
		return err
	}
	fresh, err := importer.Import(svc, all, log)
	if err != nil {
		return err
	}
	if report := differ.Diff(checkedIn, fresh); !report.Empty() {
		return fmt.Errorf("%s: %s no longer matches %s; import and generate again:\n%s", svc.Name, svc.Definitions, svc.Spec, report.String())
	}
	n, err := generator.Check(checkedIn, svc.Path(svc.Output))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s: %d operations, every one has a method in %s\n", svc.Name, n, svc.Path(svc.Output))

	// what the repository's own code reaches, for a reader deciding what the live tests have not proven
	uncalled, total, err := generator.Uncalled(checkedIn, svc.Path(callers), svc.Path(svc.Output))
	if err != nil {
		return err
	}
	where := "in the repository"
	if callers != "." {
		where = "under " + callers
	}
	_, _ = fmt.Fprintf(stdout, "%s: %d of %d methods are named by a hand-written file %s\n", svc.Name, total-len(uncalled), total, where)
	if len(uncalled) > 0 {
		_, _ = fmt.Fprintf(listing, "  never named: %s\n", strings.Join(uncalled, ", "))
	}

	return nil
}
