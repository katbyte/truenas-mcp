package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/version"
	"github.com/sirupsen/logrus"
)

// versionLine is the version this binary says it is: the version, and the commit it was built from when the version does not already name it.
func versionLine() string { return versionOf(version.Version, version.GitCommit) }

// versionOf is a version and commit as one line. A build from a checkout is stamped v0.1.0+6@gbfa4166, six commits past the tag, which names its
// commit already; a release is stamped its tag alone, so its commit is added.
func versionOf(v, commit string) string {
	if commit == "" || commit == "unknown" || strings.Contains(v, commit) {
		return v
	}

	return fmt.Sprintf("%s (commit %s)", v, commit)
}

// announce writes one line to the log as an info line, whatever level the log is at: the log shows only warnings unless asked for more, and what
// a server started as is wanted in every log.
func announce(format string, args ...any) {
	entry := logrus.NewEntry(clog.Log)
	entry.Time, entry.Level, entry.Message = time.Now(), logrus.InfoLevel, fmt.Sprintf(format, args...)
	line, err := clog.Log.Formatter.Format(entry)
	if err != nil {
		return
	}

	_, _ = clog.Log.Out.Write(line)
}
