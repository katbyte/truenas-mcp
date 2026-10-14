// Package workarounds is how a repository fixes the bugs in the OpenAPI
// documents it vendors, one named workaround per bug, after Pandora's
// dataworkarounds. The workarounds themselves live with the documents, in the
// repository that vendors them; this package is the interface they implement,
// the helpers they share, and the test every one of them must pass (Verify).
//
// Every workaround patches the loaded document before it is normalised, and
// every one first checks that the bug it fixes is still there: when the
// document no longer has the problem (a refreshed spec declares the missing
// parameter, the duplicate operationId is gone) the workaround fails the
// import instead of silently doing nothing, so dead workarounds get deleted
// rather than accumulating. The applied names are recorded in the service's
// definitions.
//
// Only the shape of the document belongs in a workaround: what an operation
// takes and answers. Behaviour the document cannot express (Emby keying watch
// state by provider id, a filter it silently drops) stays in the hand-written
// code over the generated SDK, next to the live tests that found it.
package workarounds

import (
	"errors"
	"fmt"
	"strings"

	"github.com/katbyte/pandorest/openapi"
)

// Workaround fixes one bug in one service's document.
type Workaround interface {
	// Name identifies the workaround in logs and in Service.json; it starts
	// with its service's name and a dash.
	Name() string
	// Service is the config service name it applies to.
	Service() string
	// Bug says what is wrong with the document and how the server behaves.
	Bug() string
	// Apply patches the document, or returns an error when the bug it fixes
	// is not there (fixed upstream, or the operation is gone).
	Apply(spec *openapi.Spec) error
}

// Apply runs every workaround in all that is for a service, in order, logging
// each, and returns the names applied.
func Apply(all []Workaround, service string, spec *openapi.Spec, log func(string)) ([]string, error) {
	if log == nil {
		log = func(string) {}
	}
	applied := []string{}
	for _, w := range all {
		if w.Service() != service {
			continue
		}
		var notes []string
		var err error
		if noting, ok := w.(interface {
			ApplyNoting(*openapi.Spec, func(string)) error
		}); ok {
			// a default workaround says what it reached
			err = noting.ApplyNoting(spec, func(n string) { notes = append(notes, n) })
		} else {
			err = w.Apply(spec)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: workaround %s no longer applies, so remove it: %w\n  (it was for: %s)", service, w.Name(), err, w.Bug())
		}
		line := fmt.Sprintf("%s: applied workaround %s", service, w.Name())
		if len(notes) > 0 {
			line += ": " + strings.Join(notes, ", ")
		}
		log(line)
		applied = append(applied, w.Name())
	}

	return applied, nil
}

// Verify is the test a repository's workarounds must pass, against the
// documents it vendors: load reads a fresh copy of a service's document. No
// two workarounds share a name; each is named for its service and says what
// bug it fixes; the bug is in the document; applying it to the document it
// already fixed fails, the cheapest stand-in for a document fixed upstream, so
// it would notice the bug being gone; and on a document that has lost the 200
// responses it reads it returns rather than panics. Every problem found is in
// the error, one to a line.
//
// Each workaround meets its document as the ones before it in the list have left it, in order, which is how Apply applies them:
// a workaround may depend on a shape an earlier one declared (an enum on an answer the document said nothing of), so it is asked
// whether it applies to that document, and whether applying it again to the same fails. The no-panic check is on the raw document.
func Verify(all []Workaround, load func(service string) (*openapi.Spec, error)) error {
	var problems []error
	seen := map[string]bool{}
	sofar := map[string]*openapi.Spec{}
	for _, w := range all {
		if seen[w.Name()] {
			problems = append(problems, fmt.Errorf("two workarounds are named %s", w.Name()))
		}
		seen[w.Name()] = true

		if err := verifyOne(w, load, sofar); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", w.Name(), err))
		}
	}

	return errors.Join(problems...)
}

// verifyOne checks one workaround against its service's document as the workarounds before it left it, in sofar, loading it the
// first time.
func verifyOne(w Workaround, load func(service string) (*openapi.Spec, error), sofar map[string]*openapi.Spec) error {
	if w.Bug() == "" || !strings.HasPrefix(w.Name(), w.Service()+"-") {
		return fmt.Errorf("its name or its bug %q does not describe it: a name starts with the service and a dash, and a bug is a sentence", w.Bug())
	}

	spec := sofar[w.Service()]
	if spec == nil {
		loaded, err := load(w.Service())
		if err != nil {
			return fmt.Errorf("loading the %s document: %w", w.Service(), err)
		}
		spec = loaded
		sofar[w.Service()] = spec
	}
	if err := w.Apply(spec); err != nil {
		return fmt.Errorf("it does not apply to the vendored document as the workarounds before it leave it, so the bug is not there or the workaround is written wrong: %w", err)
	}
	if err := w.Apply(spec); err == nil {
		return errors.New("applying it again succeeded, so it would not notice the bug being fixed")
	}

	spec, err := load(w.Service())
	if err != nil {
		return fmt.Errorf("loading the %s document: %w", w.Service(), err)
	}
	for _, item := range spec.Paths {
		for _, m := range item.Methods() {
			delete(m.Operation.Responses, "200")
		}
	}

	return applyWithoutPanic(w, spec)
}

// applyWithoutPanic applies a workaround to a document with no 200 responses,
// where an error or a no-op are both right: the point is that it returns.
func applyWithoutPanic(w Workaround, spec *openapi.Spec) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("it panicked on a document with no 200 responses: %v", r)
		}
	}()
	_ = w.Apply(spec)

	return nil
}

// Operation finds an operation a workaround targets.
func Operation(spec *openapi.Spec, method, path string) (*openapi.Operation, error) {
	op := spec.Operation(method, path)
	if op == nil {
		return nil, fmt.Errorf("%s %s is not in the document", method, path)
	}

	return op, nil
}

// JSONResponse is an operation's 200 application/json media type, or the
// error a workaround returns when the document no longer declares one,
// rather than a nil pointer where a message is promised. what names the
// operation in that error.
func JSONResponse(op *openapi.Operation, what string) (*openapi.MediaType, error) {
	ok := op.Responses["200"]
	if ok == nil {
		return nil, fmt.Errorf("%s has no 200 response", what)
	}
	media := ok.Content["application/json"]
	if media == nil {
		return nil, fmt.Errorf("%s no longer answers JSON", what)
	}

	return media, nil
}

// Param is a parameter a workaround adds: In is openapi.InQuery, InHeader or
// InPath, and Type one of the openapi types. A header named Accept is how a
// caller is given the say over what one call asks for, for a server that
// chooses its answer by it: the option, when set, is the whole header.
type Param struct {
	Name        string
	In          string
	Type        string
	Description string
}

func (p Param) parameter() *openapi.Parameter {
	return &openapi.Parameter{Name: p.Name, In: p.In, Description: p.Description, Schema: &openapi.Schema{Type: p.Type}, Added: true}
}

// AddParameters adds undeclared parameters to operations, each target a
// method and a path ("GET /Items"), failing when any target already declares
// one of them.
func AddParameters(spec *openapi.Spec, targets []string, params ...Param) error {
	for _, target := range targets {
		method, path, _ := strings.Cut(target, " ")
		op, err := Operation(spec, method, path)
		if err != nil {
			return err
		}
		for _, p := range params {
			if op.Parameter(p.In, p.Name) != nil {
				return fmt.Errorf("%s already declares %s parameter %s", target, p.In, p.Name)
			}
			op.Parameters = append(op.Parameters, p.parameter())
		}
	}

	return nil
}

// Property finds a component schema's property.
func Property(spec *openapi.Spec, schema, name string) (*openapi.Schema, error) {
	s := spec.Components.Schemas[schema]
	if s == nil {
		return nil, fmt.Errorf("no schema %s", schema)
	}
	p := s.Properties[name]
	if p == nil {
		return nil, fmt.Errorf("schema %s has no property %s", schema, name)
	}

	return p, nil
}
