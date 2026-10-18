package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/katbyte/go-kt/clog"
	"github.com/sirupsen/logrus"
)

// A build from a checkout is stamped with how far past the tag it is and the commit, so the commit is not said twice; a release is stamped its tag
// alone, so its commit is added; a build stamped with no commit says the version as it is.
func TestTheVersionNamesItsCommitOnce(t *testing.T) {
	t.Parallel()

	const commit = "bfa4166"
	for _, c := range []struct{ version, commit, want string }{
		{"v0.1.0+6@gbfa4166", commit, "v0.1.0+6@gbfa4166"},
		{"v0.1.0+6@gbfa4166-dirty", commit, "v0.1.0+6@gbfa4166-dirty"},
		{"v0.1.0", commit, "v0.1.0 (commit bfa4166)"},
		{"ci", "unknown", "ci"},
		{"dev", "", "dev"},
		{"v0.1.1-0.20261018120000-bfa4166c0ffe", "", "v0.1.1-0.20261018120000-bfa4166c0ffe"},
	} {
		if got := versionOf(c.version, c.commit); got != c.want {
			t.Errorf("versionOf(%q, %q) = %q, want %q", c.version, c.commit, got, c.want)
		}
	}
}

// serve says the version it starts as before anything else, as an info line however little the log is set to show, so that a start that is then
// refused still says what refused it.
//
//nolint:paralleltest // the log and viper are the process's own; Make mutates them
func TestServeSaysItsVersionFirstWhateverTheLogShows(t *testing.T) {
	var logged bytes.Buffer
	was, level := clog.Log.Out, clog.Log.GetLevel()
	clog.Log.SetOutput(&logged)
	clog.Log.SetLevel(logrus.ErrorLevel)
	t.Cleanup(func() {
		clog.Log.SetOutput(was)
		clog.Log.SetLevel(level)
	})

	if _, err := run(t, "serve"); err == nil {
		t.Fatal("serve started with no NAS to serve")
	}
	if want := `level=info msg="truenas-mcp ` + versionLine() + ` starting"`; !strings.Contains(logged.String(), want) {
		t.Errorf("serve logged %q, want a line with %s", logged.String(), want)
	}
}
