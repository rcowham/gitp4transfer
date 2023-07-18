// Tests for gitfilter

package main

import (
	"flag"
	"fmt"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

var debug bool = false
var logger *logrus.Logger

func init() {
	flag.BoolVar(&debug, "debug", false, "Set to have debug logging for tests.")
}

func createLogger() *logrus.Logger {
	if logger != nil {
		return logger
	}
	logger = logrus.New()
	logger.Level = logrus.InfoLevel
	if debug {
		logger.Level = logrus.DebugLevel
	}
	return logger
}

func RunFilterWithDump(t *testing.T, logger *logrus.Logger, input string, opts *GitFilterOptions) string {
	if opts == nil {
		opts = &GitFilterOptions{}
	}
	g := NewGitFilter(logger)
	g.testInput = input
	g.RunGitFilter(*opts)
	return fmt.Sprintf("%s%s", g.testBlobOutput.String(), g.testOutput.String())
}

// ------------------------------------------------------------------

func TestFilter(t *testing.T) {
	// Rename of a file on a branch where original file is deleted and a merge is created
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	baseData := `blob
mark :1
data %d
%s

reset refs/heads/main
commit refs/heads/main
mark :2
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt
`
	gitExport := fmt.Sprintf(baseData, 9, "contents")
	expected := fmt.Sprintf(baseData, 2, "1")

	output := RunFilterWithDump(t, logger, gitExport, nil)

	assert.Equal(t, strings.ReplaceAll(expected, "\n\n", "\n"), strings.ReplaceAll(output, "\n\n", "\n"))
}

func TestFilterBranch(t *testing.T) {
	// Filter with paths specified
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 2
1

blob
mark :2
data 2
2

blob
mark :3
data 2
3

reset refs/heads/main
commit refs/heads/main
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt
M 100644 :2 src/file2.txt

reset refs/heads/dev
commit refs/heads/dev
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
renamed
from :4
R src/file1.txt src/file3.txt

reset refs/heads/main
commit refs/heads/main
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
other
from :5
R src/file2.txt src/file4.txt

reset refs/heads/dev
commit refs/heads/dev
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
ren dir
from :6
R src targ

`

	opts := &GitFilterOptions{pathFilter: "file1.txt"}
	output := RunFilterWithDump(t, logger, gitExport, opts)
	expected := gitExport
	assert.Equal(t, strings.ReplaceAll(expected, "\n\n", "\n"), strings.ReplaceAll(output, "\n\n", "\n"))
}
