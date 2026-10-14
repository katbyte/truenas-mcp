package main

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// The order classes are met in decides which of two declarations of one method wins, so it is the order Python's own walk meets them in: all
// those of one depth before any deeper, where an elif, an except and a case are each a level deeper than the block they belong to. The order
// wanted is what Python printed for the same source.
func TestClassesAreMetInTheOrderPythonMeetsThem(t *testing.T) {
	t.Parallel()

	module, err := parseModule(`
class A:
    class B:
        class C: pass
if x:
    class D: pass
elif y:
    class E: pass
try:
    pass
except Exception:
    class F: pass
finally:
    class G: pass
for i in z:
    pass
else:
    class I: pass
match v:
    case 1:
        class J: pass
class H: pass
`)
	if err != nil {
		t.Fatal(err)
	}

	classes := classesOf(module)
	met := make([]string, 0, len(classes))
	for _, class := range classes {
		met = append(met, class.name)
	}
	if want := []string{"A", "H", "B", "D", "G", "I", "C", "E", "F", "J"}; !slices.Equal(met, want) {
		t.Errorf("the classes are met in the order %v; Python meets them in the order %v", met, want)
	}
}

// A model is read for what the secrets depend on, however the file around it is written: strings that hold quotes, braces and values of their
// own, lines carried on to the next, names Python keeps for itself only at the start of a statement, and code that is only stepped over.
func TestAModelIsReadHoweverTheFileAroundItIsWritten(t *testing.T) {
	t.Parallel()

	module, err := parseModule(`"""A docstring with "quotes", a brace { and a ''' inside."""
from typing import Annotated, Generic, TypeAlias

width = 10
banner = f"{'}'} {width:{width}.{width}} {{literal}} {settings["name"]!r:>10} \N{BULLET}"
raw = rb'\'' + b"\x00"
handler = lambda x=1, *a, **k: x
left = right = Secret[str]
Password: TypeAlias = Annotated[Secret[str], Field(description="a password")]
Later = "list[Credentials]"


class Credentials(Base, mixins.Keyed, Generic[T], metaclass=Meta):
    """What a login takes."""
    type: Literal["password"] = "password"
    match: int = 0
    case: str
    pass_: Annotated[Secret[str], Field(alias="pass")]
    api_key: Secret[str] = Field(default="", alias="apiKey")
    keys: list[Secret[str]] = \
        []

    def check(self):
        match self.type:
            case "password" if self.pass_:
                return {"a": 1, **self.extra}
        return [k for k in self.keys if k]


@single_argument_args("credentials")
class LoginArgs(Credentials): pass
`)
	if err != nil {
		t.Fatal(err)
	}

	m := &models{classes: map[string]*model{}, aliases: map[string]*expr{}, wrapped: map[string]string{}}
	m.read(module)

	if !slices.Equal(m.order, []string{"Credentials", "LoginArgs"}) {
		t.Errorf("the classes read are %v; want Credentials and LoginArgs", m.order)
	}
	if bases := m.classes["Credentials"].bases; !slices.Equal(bases, []string{"Base", "Keyed"}) {
		t.Errorf("Credentials is made from %v; want Base and Keyed, as a generic base and a metaclass are not bases Python names", bases)
	}
	if fields := m.classes["Credentials"].fields.names; !slices.Equal(fields, []string{"type", "match", "case", "pass", "apiKey", "keys"}) {
		t.Errorf("Credentials has the fields %v; want each by the name it is sent under, in the order written", fields)
	}
	if m.wrapped["LoginArgs"] != "args" {
		t.Errorf("LoginArgs is read as %q; want the model of one argument that holds all", m.wrapped["LoginArgs"])
	}
	if aliases := slices.Sorted(maps.Keys(m.aliases)); !slices.Equal(aliases, []string{"Later", "Password", "banner", "handler", "raw", "width"}) {
		t.Errorf("the names given to values are %v; a value given two names at once is not a name Python keeps", aliases)
	}

	for _, c := range []struct {
		of   string
		want []string
	}{
		{"Credentials", []string{".apiKey", ".keys[]", ".pass"}},
		{"Password", []string{""}},
		{"Later", []string{"[].apiKey", "[].keys[]", "[].pass"}},
		{"banner", nil},
	} {
		if got := m.secretsOf(&expr{kind: exprName, name: c.of}, nil).sorted(); !slices.Equal(got, c.want) {
			t.Errorf("the secrets in %s are at %v; want %v", c.of, got, c.want)
		}
	}
	if got := m.arguments("LoginArgs").sorted(); !slices.Equal(got, []string{"$[0].apiKey", "$[0].keys[]", "$[0].pass"}) {
		t.Errorf("the secrets in LoginArgs's one argument are at %v", got)
	}
}

// A file that is not Python as far as it is read is an error that says where, not a reading that leaves something out.
func TestWhatIsNotPythonIsAnErrorThatSaysWhere(t *testing.T) {
	t.Parallel()

	for src, want := range map[string]string{
		"x = 1\ny = \"never ends\n":            "line 2: a string that runs off the end of its line",
		"x = (1,\n2\n":                         "a bracket that is never closed",
		"x = 1\n  y = 2\n":                     "line 2: indented where no block was started",
		"class A:\n    x: = 3\n":               "line 2",
		"@decorated\nx = 1\n":                  "line 2: decorators with no class or function after them",
		"class A:\n    x: f\"{y\n":             "line 2",
		"x = 1 ? 2\n":                          "line 1: '?' is not Python",
		"class A:\nx = 1\n":                    "line 1: a block with nothing in it",
		"class A(Base):\n    x: list[int\n]\n": "",
	} {
		_, err := parseModule(src)
		switch {
		case want == "" && err != nil:
			t.Errorf("reading %q: %v", src, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("reading %q fails with %v; want %q", src, err, want)
		}
	}
}
