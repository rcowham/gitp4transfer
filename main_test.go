// Tests for gitp4transfer

package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rcowham/gitp4transfer/config"
	"github.com/rcowham/gitp4transfer/journal"
	node "github.com/rcowham/gitp4transfer/node"
	libfastimport "github.com/rcowham/go-libgitfastimport"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

var debug bool = false
var logger *logrus.Logger

func init() {
	flag.BoolVar(&debug, "debug", false, "Set to have debug logging for tests.")
}

func runCmd(cmdLine string) (string, error) {
	logger.Debugf("runCmd: %s", cmdLine)
	cmd := exec.Command("/bin/bash", "-c", cmdLine)
	stdout, err := cmd.CombinedOutput()
	logger.Debugf("result: %s", string(stdout))
	return string(stdout), err
}

func createGitRepo(t *testing.T) string {
	d := t.TempDir()
	os.Chdir(d)
	runCmd("git init -b main")
	return d
}

func unzipBuf(data string) string {
	gz, err := gzip.NewReader(bytes.NewReader([]byte(data)))
	if err != nil {
		log.Fatal(err)
	}
	buf, err := io.ReadAll(gz)
	if err != nil {
		log.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		log.Fatal(err)
	}
	return string(buf)
}

type P4Test struct {
	startDir   string
	p4d        string
	port       string
	testRoot   string
	serverRoot string
	clientRoot string
}

func MakeP4Test(startDir string) *P4Test {
	var err error
	p4t := &P4Test{}
	p4t.startDir = startDir
	if err != nil {
		panic(err)
	}
	p4t.testRoot = filepath.Join(p4t.startDir, "testroot")
	p4t.serverRoot = filepath.Join(p4t.testRoot, "server")
	p4t.clientRoot = filepath.Join(p4t.testRoot, "client")
	p4t.ensureDirectories()
	p4t.p4d = "p4d"
	p4t.port = fmt.Sprintf("rsh:%s -r \"%s\" -L log -vserver=3 -i", p4t.p4d, p4t.serverRoot)
	os.Chdir(p4t.clientRoot)
	p4config := filepath.Join(p4t.startDir, os.Getenv("P4CONFIG"))
	writeToFile(p4config, fmt.Sprintf("P4PORT=%s", p4t.port))
	return p4t
}

func (p4t *P4Test) ensureDirectories() {
	for _, d := range []string{p4t.serverRoot, p4t.clientRoot} {
		err := os.MkdirAll(d, 0777)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create %s: %v", d, err)
		}
	}
}

// func (p4t *P4Test) cleanupTestTree() {
// 	os.Chdir(p4t.startDir)
// 	err := os.RemoveAll(p4t.testRoot)
// 	if err != nil {
// 		fmt.Fprintf(os.Stderr, "Failed to remove %s: %v", p4t.startDir, err)
// 	}
// }

func appendToFile(fname, contents string) {
	f, err := os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprint(f, contents)
	if err != nil {
		panic(err)
	}
}

// func writeToTempFile(contents string) string {
// 	f, err := os.CreateTemp("", "*.txt")
// 	if err != nil {
// 		panic(err)
// 	}
// 	defer f.Close()
// 	fmt.Fprint(f, contents)
// 	if err != nil {
// 		fmt.Println(err)
// 	}
// 	return f.Name()
// }

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

func parseFilelog(text string) map[string]string {
	result := make(map[string]string, 0) // Indexed by filename
	k := ""
	v := ""
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if hasPrefix(line, "//") {
			if k != "" {
				result[k] = v
			}
			k = line
			v = ""
		} else {
			v = v + line + "\n"
		}
	}
	if k != "" {
		result[k] = v
	}
	return result
}

// Make comparison differences easier to view
func compareFilelog(t *testing.T, expected, actual string) {
	eResults := parseFilelog(expected)
	aResults := parseFilelog(actual)
	for k, v := range eResults {
		assert.Regexp(t, v, aResults[k], "Different for: %s", k)
		// Check we aren't missing a line
		eLines := strings.Split(v, "\n")
		aLines := strings.Split(aResults[k], "\n")
		assert.Equal(t, len(eLines), len(aLines), "Different no of lines for: %s", k)
	}
	for k := range eResults {
		if _, ok := aResults[k]; !ok {
			assert.Fail(t, fmt.Sprintf("Missing key '%s' in '%s'", k, actual))
		}
	}
	for k := range aResults {
		if _, ok := eResults[k]; !ok {
			assert.Fail(t, fmt.Sprintf("Unexpected key '%s' in '%s'", k, expected))
		}
	}
}

// func TestCompareFilelog(t *testing.T) {
// 	logger := createLogger()
// 	logger.Debugf("======== Test: %s", t.Name())
// 	compareFilelog(t, `//file1
// t.*`, `//file1
// test`)

// 	compareFilelog(t, `//file2
// t*`, `//file1
// test`)
// }

func runTransferWithDump(t *testing.T, logger *logrus.Logger, output string, opts *GitParserOptions) string {
	p4t := MakeP4Test(t.TempDir())
	if opts != nil {
		opts.archiveRoot = p4t.serverRoot
		if opts.config != nil && opts.config.ImportDepot == "" {
			opts.config.ImportDepot = "import"
		}
		if opts.config != nil && opts.config.DefaultBranch == "" {
			opts.config.DefaultBranch = "main"
		}
	} else {
		opts = &GitParserOptions{archiveRoot: p4t.serverRoot,
			config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}
	g.testInput = output
	user := getGitUser(t)
	commitChan := g.GitParse(nil)
	commits := make([]GitCommit, 0)
	// just read all commits and test them
	for c := range commitChan {
		commits = append(commits, c)
	}

	buf := new(bytes.Buffer)
	os.Chdir(p4t.serverRoot)
	logger.Debugf("P4D serverRoot: %s", p4t.serverRoot)

	j := journal.Journal{}
	j.SetWriter(buf)
	j.WriteHeader(opts.config.ImportDepot, opts.caseInsensitive)

	for _, c := range commits {
		j.WriteChange(c.commit.Mark, user, c.commit.Msg, int(c.commit.Author.Time.Unix()))
		for _, f := range c.files {
			opts.archiveRoot = p4t.serverRoot
			f.CreateArchiveFile(nil, opts, g.blobFileMatcher, c.commit.Mark)
			f.WriteJournal(&j, &c)
		}
	}

	jnl := filepath.Join(p4t.serverRoot, "jnl.0")
	writeToFile(jnl, buf.String())
	runCmd("p4d -r . -jr jnl.0")
	runCmd("p4d -r . -J journal -xu")
	runCmd("p4 storage -R")
	runCmd("p4 storage -r")
	runCmd("p4 storage -w")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, "Phase 1 of the storage upgrade has finished.\n", result)

	return p4t.serverRoot
}

func getGitUser(t *testing.T) string {
	output, err := runCmd("git config user.email")
	if err != nil {
		t.Errorf("ERROR: Failed to git config '%s': %v\n", output, err)
	}
	return getUserFromEmail(output)
}

func runTransfer(t *testing.T, logger *logrus.Logger) string {
	// fast-export with rename detection implemented
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	return runTransferWithDump(t, logger, output, nil)
}

func runTransferOpts(t *testing.T, logger *logrus.Logger, opts *GitParserOptions) string {
	// fast-export with rename detection implemented
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	return runTransferWithDump(t, logger, output, opts)
}

// ------------------------------------------------------------------
// Set of tests for validation logic, detecting delets of renames etc

type testFile struct {
	name          string
	srcName       string
	action        GitAction
	isDirtyRename bool
}

var gcMark = 10 // Unique Mark - see newGCMark

func newTestGitFile(gf *GitFile) *GitFile {
	gitFileID += 1
	gf.ID = gitFileID
	return gf
}

func newGCMark() int {
	gcMark += 10
	return gcMark
}

func newValidatedCommit(g *GitP4Transfer, tf []testFile) *GitCommit {
	branch := "main"
	gc := &GitCommit{commit: &libfastimport.CmdCommit{Mark: newGCMark()}, branch: branch, files: make([]*GitFile, 0)}
	g.commits[gc.commit.Mark] = gc
	for _, f := range tf {
		gf := newTestGitFile(&GitFile{name: f.name, srcName: f.srcName, action: f.action,
			isDirtyRename: f.isDirtyRename, blob: &GitBlob{}})
		if gf.action == modify {
			g.blobFileMatcher.addGitFile(gf)
		}
		gc.files = append(gc.files, gf)
	}
	g.ValidateCommit(gc)
	return gc
}

func newValidatedBranchCommit(g *GitP4Transfer, branch string, tf []testFile) *GitCommit {
	gc := &GitCommit{commit: &libfastimport.CmdCommit{Mark: newGCMark()}, branch: branch, files: make([]*GitFile, 0)}
	gc.commit.From = fmt.Sprintf(":%d", gc.commit.Mark-10)
	g.commits[gc.commit.Mark] = gc
	for _, f := range tf {
		gf := newTestGitFile(&GitFile{name: f.name, srcName: f.srcName, action: f.action,
			isDirtyRename: f.isDirtyRename, blob: &GitBlob{}})
		if gf.action == modify {
			g.blobFileMatcher.addGitFile(gf)
		}
		gc.files = append(gc.files, gf)
	}
	g.ValidateCommit(gc)
	return gc
}

func TestCommitValidAddDelete(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Simple add
	gc := newValidatedCommit(g, []testFile{{name: "file.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file.txt", nfiles[0])

	// Now delete the added file in new commit
	gc = newValidatedCommit(g, []testFile{{name: "file.txt", srcName: "", action: delete}})
	assert.Equal(t, 1, len(gc.files))
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 0, len(nfiles))
}

func TestCommitValidAddRename(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add file and rename it
	gc := newValidatedCommit(g, []testFile{{name: "file.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file.txt", nfiles[0])

	gc = newValidatedCommit(g, []testFile{{name: "file2.txt", srcName: "file.txt", action: rename}})
	assert.Equal(t, 1, len(gc.files))
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file2.txt", nfiles[0])

	// Rename again - dirty this time
	gc = newValidatedCommit(g, []testFile{{name: "file3.txt", srcName: "file2.txt", action: rename},
		{name: "file3.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	assert.True(t, gc.files[0].isDirtyRename)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file3.txt", nfiles[0])
}

func TestCommitValidDirRename(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add dir
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify}})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename dir
	gc = newValidatedCommit(g, []testFile{{name: "targ", srcName: "src", action: rename}})
	assert.Equal(t, 2, len(gc.files))
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "targ/file1.txt", nfiles[0])
	assert.Equal(t, "targ/file2.txt", nfiles[1])

	// Rename dir again - should be ignored
	gc = newValidatedCommit(g, []testFile{{name: "targ", srcName: "src", action: rename}})
	assert.Equal(t, 0, len(gc.files))
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "targ/file1.txt", nfiles[0])
	assert.Equal(t, "targ/file2.txt", nfiles[1])

	// Delete dir
	gc = newValidatedCommit(g, []testFile{{name: "targ", srcName: "", action: delete}})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "targ/file1.txt", gc.files[0].name)
	assert.Equal(t, "targ/file2.txt", gc.files[1].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 0, len(nfiles))

	// Delete dir again - should be ignored
	gc = newValidatedCommit(g, []testFile{{name: "targ", srcName: "", action: delete}})
	assert.Equal(t, 0, len(gc.files))
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 0, len(nfiles))
}

func TestCommitValidPseudoRename(t *testing.T) {
	// More complex scenarios
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])

	// Pseudo rename we modify the supposedly deleted file
	gc = newValidatedCommit(g, []testFile{{name: "file2.txt", srcName: "file1.txt", action: rename},
		{name: "file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.Equal(t, "file1.txt", gc.files[1].name)
	assert.True(t, gc.files[0].isPseudoRename)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])
	assert.Equal(t, "file2.txt", nfiles[1])

	// Modification of a deleted file
	gc = newValidatedCommit(g, []testFile{{name: "file2.txt", srcName: "", action: delete},
		{name: "file2.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.Equal(t, modify, gc.files[0].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])
	assert.Equal(t, "file2.txt", nfiles[1])

	// Delete of a modified file
	gc = newValidatedCommit(g, []testFile{{name: "file2.txt", srcName: "", action: modify},
		{name: "file2.txt", srcName: "", action: delete}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.Equal(t, delete, gc.files[0].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])
}

func TestCommitValidRenameOfModify(t *testing.T) {
	// Rename a modified file - another sequence for dirty rename
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])

	// Modify file and rename
	gc = newValidatedCommit(g, []testFile{{name: "file1.txt", srcName: "", action: modify},
		{name: "file2.txt", srcName: "file1.txt", action: rename}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.True(t, gc.files[0].isDirtyRename)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file2.txt", nfiles[0])
}

func TestCommitValidRenameOfModifySameCommit(t *testing.T) {
	// Rename a modified file - another sequence for dirty rename
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Same commit - we modify a file and rename it
	gc := newValidatedCommit(g, []testFile{{name: "file1.txt", srcName: "", action: modify},
		{name: "file2.txt", srcName: "file1.txt", action: rename}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.True(t, gc.files[0].isDirtyRename)
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file2.txt", nfiles[0])
}

func TestCommitValidRenameDirOfModify(t *testing.T) {
	// Rename a modified file - another sequence for dirty rename
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	// Modify and rename file
	gc = newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "targ", srcName: "src", action: rename}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "targ/file1.txt", gc.files[0].name)
	assert.True(t, gc.files[0].isDirtyRename)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "targ/file1.txt", nfiles[0])
}

func TestCommitValidRenameDirOfModifySameCommit(t *testing.T) {
	// Rename a modified file - another sequence for dirty rename
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Modify and rename file in same commit
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "targ", srcName: "src", action: rename}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "targ/file1.txt", gc.files[0].name)
	assert.False(t, gc.files[0].isDirtyRename)
	assert.Equal(t, modify, gc.files[0].action)
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "targ/file1.txt", nfiles[0])
}

func TestCommitValidDeleteDirAndModify(t *testing.T) {
	// Modify a file deleted by dir delete
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	// Delete and Modify file
	gc = newValidatedCommit(g, []testFile{{name: "src", srcName: "", action: delete},
		{name: "src/file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "src/file1.txt", gc.files[0].name)
	assert.Equal(t, modify, gc.files[0].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
}

func TestCommitValidDeleteDirAndModifySameCommit(t *testing.T) {
	// Modify a file deleted by dir delete
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Delete and Modify file
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src", srcName: "", action: delete}})
	assert.Equal(t, 0, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 0, len(nfiles))
}

func TestCommitValidRenameBecomes2Edits(t *testing.T) {
	// A rename which is both dirty and pseudo - so ultimately not a rename!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])

	// Combine pseudo and dirty
	gc = newValidatedCommit(g, []testFile{{name: "file2.txt", srcName: "file1.txt", action: rename},
		{name: "file1.txt", srcName: "", action: modify},
		{name: "file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.Equal(t, "file1.txt", gc.files[1].name)
	assert.Equal(t, modify, gc.files[0].action)
	assert.Equal(t, modify, gc.files[1].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])
	assert.Equal(t, "file2.txt", nfiles[1])

	// Combine pseudo and dirty - other way around!
	gc = newValidatedCommit(g, []testFile{{name: "file2.txt", srcName: "file1.txt", action: rename},
		{name: "file2.txt", srcName: "", action: modify},
		{name: "file1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "file2.txt", gc.files[0].name)
	assert.Equal(t, "file1.txt", gc.files[1].name)
	assert.Equal(t, modify, gc.files[0].action)
	assert.Equal(t, modify, gc.files[1].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])
	assert.Equal(t, "file2.txt", nfiles[1])
}

func TestCommitValidRenameCascade(t *testing.T) {
	// Multiple renames B->A, C->B etc
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "file1.txt", srcName: "", action: modify},
		{name: "file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "file1.txt", nfiles[0])
	assert.Equal(t, "file2.txt", nfiles[1])

	// Renames - must be in correct order (inverse will be joined into 1)
	gc = newValidatedCommit(g, []testFile{{name: "file3.txt", srcName: "file2.txt", action: rename},
		{name: "file2.txt", srcName: "file1.txt", action: rename},
	})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "file3.txt", gc.files[0].name)
	assert.Equal(t, "file2.txt", gc.files[1].name)
	// assert.Equal(t, modify, gc.files[0].action)
	// assert.Equal(t, modify, gc.files[1].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "file3.txt", nfiles[0])
	assert.Equal(t, "file2.txt", nfiles[1])
}

func TestCommitValidDoubleRename(t *testing.T) {
	// More complex scenarios
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify}})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename directory and then override rename of one file
	gc = newValidatedCommit(g, []testFile{{name: "targ", srcName: "src", action: rename},
		{name: "targ/file3.txt", srcName: "targ/file1.txt", action: rename}})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "targ/file2.txt", gc.files[0].name)
	assert.Equal(t, "targ/file3.txt", gc.files[1].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "targ/file2.txt", nfiles[0])
	assert.Equal(t, "targ/file3.txt", nfiles[1])
}

func TestCommitValidDoubleRename2(t *testing.T) {
	// Rename a file and override with directory rename
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify}})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename directory and then override rename of one file
	gc = newValidatedCommit(g, []testFile{{name: "src/file3.txt", srcName: "src/file1.txt", action: rename},
		{name: "targ", srcName: "src", action: rename}})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "targ/file2.txt", gc.files[1].name)
	assert.Equal(t, "targ/file3.txt", gc.files[0].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "targ/file3.txt", nfiles[0])
	assert.Equal(t, "targ/file2.txt", nfiles[1])
}

func TestCommitValidDoubleRenameAndDelete(t *testing.T) {
	// Rename a file and override with directory rename
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify}})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename file then whole directory and then delete soruce dir
	gc = newValidatedCommit(g, []testFile{{name: "src/file3.txt", srcName: "src/file1.txt", action: rename},
		{name: "targ", srcName: "src", action: rename},
		{name: "src", srcName: "", action: delete},
	})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "targ/file3.txt", gc.files[0].name)
	assert.Equal(t, "targ/file2.txt", gc.files[1].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "targ/file3.txt", nfiles[0])
	assert.Equal(t, "targ/file2.txt", nfiles[1])
}

func TestCommitValidDeleteRenamedDir(t *testing.T) {
	// Rename a dir and override with directory delete of target
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify},
		{name: "targ/file3.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 3, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 3, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])
	assert.Equal(t, "targ/file3.txt", nfiles[2])

	// Rename dir then delete target - result is no files!
	gc = newValidatedCommit(g, []testFile{
		{name: "targ", srcName: "src", action: rename},
		{name: "targ", srcName: "", action: delete},
	})
	assert.Equal(t, 3, len(gc.files))
	assert.Equal(t, "src/file1.txt", gc.files[0].name)
	assert.Equal(t, "src/file2.txt", gc.files[1].name)
	assert.Equal(t, "targ/file3.txt", gc.files[2].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 0, len(nfiles))
}

func TestCommitValidDeleteFollowedByRenameDir(t *testing.T) {
	// Delete source file and then rename dir
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename dir then delete target - result is no files!
	gc = newValidatedCommit(g, []testFile{
		{name: "src/file1.txt", srcName: "", action: delete},
		{name: "targ", srcName: "src", action: rename},
	})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "src/file1.txt", gc.files[0].name)
	assert.Equal(t, "targ/file2.txt", gc.files[1].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "targ/file2.txt", nfiles[0])
}

func TestCommitValidDirRenameRenameBackDelete(t *testing.T) {
	// Complex scenario of rename and deletes in same commit!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename dir, modify, rename back, delete and modify!
	gc = newValidatedCommit(g, []testFile{
		{name: "temp", srcName: "src", action: rename},
		{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src", srcName: "temp", action: rename},
		{name: "temp", srcName: "", action: delete},
		{name: "src/file1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "src/file1.txt", gc.files[0].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])
}

func TestCommitValidDirRenameRenameBackDelete2(t *testing.T) {
	// Complex scenario of rename and deletes in same commit!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename dir, modify, rename back, delete and modify!
	gc = newValidatedCommit(g, []testFile{
		{name: "temp", srcName: "src", action: rename},
		{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file1.txt", srcName: "temp/file1.txt", action: rename},
		{name: "temp", srcName: "", action: delete},
	})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "src/file2.txt", gc.files[0].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, delete, gc.files[0].action)
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
}

func TestCommitValidDirRenameDelete(t *testing.T) {
	// Complex scenario of dir rename, modify of file and dir delete in same commit!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename dir, modify, delete and modify!
	gc = newValidatedCommit(g, []testFile{
		{name: "temp", srcName: "src", action: rename},
		{name: "src/file1.txt", srcName: "", action: modify},
		{name: "temp", srcName: "", action: delete},
	})
	assert.Equal(t, 2, len(gc.files))
	assert.Equal(t, "src/file2.txt", gc.files[0].name)
	assert.Equal(t, "src/file1.txt", gc.files[1].name)
	assert.Equal(t, delete, gc.files[0].action)
	assert.Equal(t, modify, gc.files[1].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
}

func TestCommitValidDirRenameTwice(t *testing.T) {
	// Complex scenario of multiple renames
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify},
		{name: "src/file2.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 2, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 2, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "src/file2.txt", nfiles[1])

	// Rename dir, modify, rename again and modify!
	gc = newValidatedCommit(g, []testFile{
		{name: "temp", srcName: "src", action: rename},
		{name: "src/file1.txt", srcName: "", action: modify},
		{name: "targ", srcName: "temp", action: rename},
		{name: "targ/file1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 3, len(gc.files))
	for _, gf := range gc.files {
		logger.Debugf("File: %s", gf.name)
	}
	assert.Equal(t, "targ/file1.txt", gc.files[0].name)
	assert.Equal(t, "targ/file2.txt", gc.files[1].name)
	assert.Equal(t, "src/file1.txt", gc.files[2].name)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 3, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	assert.Equal(t, "targ/file1.txt", nfiles[1])
	assert.Equal(t, "targ/file2.txt", nfiles[2])
}

func TestCommitValidCaseInsensitiveRename(t *testing.T) {
	// Rename a file just changing case
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}, caseInsensitive: true}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	// Rename just changing case
	gc = newValidatedCommit(g, []testFile{
		{name: "src/FILE1.txt", srcName: "src/file1.txt", action: rename},
	})
	assert.Equal(t, 0, len(gc.files))
}

func TestCommitValidCaseInsensitiveDirtyRename(t *testing.T) {
	// Rename a file just changing case
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}, caseInsensitive: true}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	// Rename just changing case
	gc = newValidatedCommit(g, []testFile{
		{name: "src/FILE1.txt", srcName: "src/file1.txt", action: rename, isDirtyRename: true},
	})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "src/FILE1.txt", gc.files[0].name)
	assert.Equal(t, modify, gc.files[0].action)
}

func TestCommitValidCaseInsensitiveDirDeleteModify(t *testing.T) {
	// Dir delete followed by case insensitive modify
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}, caseInsensitive: true}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{{name: "src/file1.txt", srcName: "", action: modify}})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	// Delete old dir and modify to new name
	gc = newValidatedCommit(g, []testFile{
		{name: "src", srcName: "", action: delete},
		{name: "SRC/file1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "SRC/file1.txt", gc.files[0].name)
	assert.Equal(t, modify, gc.files[0].action)
}

func TestCommitValidCaseInsensitiveDirDeleteMultiple(t *testing.T) {
	// Dir delete of multiple files only differing in case
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}, caseInsensitive: true}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{
		{name: "src/file1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	gc = newValidatedBranchCommit(g, "dev", []testFile{
		{name: "src/FILE1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 1, len(gc.files))
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])
	nfiles = g.filesOnBranch["dev"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	// Delete old dir
	gc = newValidatedCommit(g, []testFile{
		{name: "src", srcName: "", action: delete},
	})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "src/file1.txt", gc.files[0].name)
	assert.Equal(t, delete, gc.files[0].action)
}

func TestCommitValidCaseInsensitiveDirDelete(t *testing.T) {
	// Dir delete of modify with different case
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	opts := &GitParserOptions{config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}, caseInsensitive: true}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatalf("Failed to create GitP4Transfer")
	}

	// Add
	gc := newValidatedCommit(g, []testFile{
		{name: "src/file1.txt", srcName: "", action: modify},
	})
	assert.Equal(t, 1, len(gc.files))
	nfiles := g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 1, len(nfiles))
	assert.Equal(t, "src/file1.txt", nfiles[0])

	gc = newValidatedCommit(g, []testFile{
		{name: "src/file1.txt", srcName: "", action: modify},
		{name: "SRC", srcName: "", action: delete},
	})
	assert.Equal(t, 1, len(gc.files))
	assert.Equal(t, "src/file1.txt", gc.files[0].name)
	assert.Equal(t, delete, gc.files[0].action)
	nfiles = g.filesOnBranch["main"].GetFiles("")
	assert.Equal(t, 0, len(nfiles))
}

// ------------------------------------------------------------------

func TestAdd(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")

	// fast-export with rename detection implemented
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Export file:\n%s", output)

	p4t := MakeP4Test(t.TempDir())
	os.Chdir(p4t.serverRoot)
	logger.Debugf("P4D serverRoot: %s", p4t.serverRoot)

	opts := &GitParserOptions{archiveRoot: p4t.serverRoot,
		config: &config.Config{ImportDepot: "import", DefaultBranch: "main"}}
	g, err := NewGitP4Transfer(logger, opts)
	if err != nil {
		logger.Fatal("Failed to create object")
	}
	g.testInput = output
	commitChan := g.GitParse(nil)
	commits := make([]GitCommit, 0)
	// just read all commits and test them
	for c := range commitChan {
		commits = append(commits, c)
	}
	assert.Equal(t, 1, len(commits))
	c := commits[0]
	assert.Equal(t, 2, c.commit.Mark)
	assert.Equal(t, "refs/heads/main", c.commit.Ref)
	assert.Equal(t, 1, len(c.files))
	f := c.files[0]
	assert.Equal(t, modify, f.action)
	assert.Equal(t, src, f.name)

	buf := new(bytes.Buffer)
	j := journal.Journal{}
	j.SetWriter(buf)
	j.WriteHeader(opts.config.ImportDepot, opts.caseInsensitive)
	c = commits[0]
	j.WriteChange(c.commit.Mark, defaultP4user, c.commit.Msg, int(c.commit.Author.Time.Unix()))
	f = c.files[0]
	j.WriteRev(f.p4.depotFile, f.p4.rev, f.p4.p4action, f.baseFileType, c.commit.Mark,
		f.p4.depotFile, c.commit.Mark, int(c.commit.Author.Time.Unix()))
	dt := c.commit.Author.Time.Unix()
	expectedJournal := fmt.Sprintf(`@pv@ 0 @db.depot@ @import@ 0 @subdir@ @import/...@ 
@pv@ 3 @db.domain@ @import@ 100 @@ @@ @@ @@ @git-user@ 0 0 0 1 @Created by git-user@ 
@pv@ 3 @db.user@ @git-user@ @git-user@@git-client@ @@ 0 0 @git-user@ @@ 0 @@ 0 
@pv@ 0 @db.view@ @git-client@ 0 0 @//git-client/...@ @//import/...@ 
@pv@ 3 @db.domain@ @git-client@ 99 @@ @/ws@ @@ @@ @git-user@ 0 0 0 1 @Created by git-user@ 
@pv@ 0 @db.desc@ 2 @initial
@ 
@pv@ 0 @db.change@ 2 2 @git-client@ @git-user@ %d 1 @initial
@ 
@rv@ 0 @db.counters@ @change@ 2 
@pv@ 3 @db.rev@ @//import/main/src.txt@ 1 3 0 2 %d %d 00000000000000000000000000000000 @//import/main/src.txt@ @1.2@ 3 
@pv@ 0 @db.revcx@ 2 @//import/main/src.txt@ 1 0 
`, dt, dt, dt)
	assert.Equal(t, expectedJournal, buf.String())

	jnl := filepath.Join(p4t.serverRoot, "jnl.0")
	writeToFile(jnl, expectedJournal)
	opts.archiveRoot = p4t.serverRoot
	f.CreateArchiveFile(nil, opts, g.blobFileMatcher, c.commit.Mark)
	runCmd("p4d -r . -jr jnl.0")
	runCmd("p4d -r . -J journal -xu")
	runCmd("p4 storage -r")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, "Phase 1 of the storage upgrade has finished.\n", result)
	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)
	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "", result)
	result, err = runCmd("p4 changes")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `Change 2 on .* by git\-user@git\-client`, result)
	result, err = runCmd("p4 changes //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `Change 2 on .* by git\-user@git\-client`, result)
	contents, err := runCmd("p4 print -q //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, contents)
}

func TestAddEdit(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	srcContents2 := "contents\nappended\n"
	writeToFile(src, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	user := getGitUser(t)

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#2 - edit change 4 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/main/src.txt#2")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents2, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*/1.4.gz`, result)

	result, err = runCmd("p4 changes")
	assert.Equal(t, nil, err)
	assert.Regexp(t, fmt.Sprintf(`Change 4 on .* by %s@git\-client`, user), result)
	assert.Regexp(t, fmt.Sprintf(`Change 2 on .* by %s@git\-client`, user), result)
	result, err = runCmd("p4 changes //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, fmt.Sprintf(`Change 4 on .* by %s@git\-client`, user), result)
	assert.Regexp(t, fmt.Sprintf(`Change 2 on .* by %s@git\-client`, user), result)
	result, err = runCmd("p4 storage //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrRev 1.2`, result)
	assert.Regexp(t, `lbrRev 1.4`, result)
}

func TestMaxCommits(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	srcContents2 := "contents\nappended\n"
	writeToFile(src, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	user := getGitUser(t)

	r := runTransferOpts(t, logger, &GitParserOptions{
		maxCommits: 1,
		config:     &config.Config{ImportDepot: "import", DefaultBranch: "main"},
	})
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*/1.2.gz`, result)

	result, err = runCmd("p4 changes")
	assert.Equal(t, nil, err)
	assert.NotRegexp(t, fmt.Sprintf(`Change 4 on .* by %s@git\-client`, user), result)
	assert.Regexp(t, fmt.Sprintf(`Change 2 on .* by %s@git\-client`, user), result)
}

func TestCaseInsensitive(t *testing.T) { // Check case sensitive flag set
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "SRC.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")

	r := runTransferOpts(t, logger, &GitParserOptions{
		caseInsensitive: true,
		config:          &config.Config{ImportDepot: "IMPORT", DefaultBranch: "main"},
	})
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//IMPORT/main/SRC.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 print -q //IMPORT/main/SRC.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //IMPORT/main/SRC.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*/import/main/src.txt,d/1.2.gz`, result)

	files, err := ioutil.ReadDir(r)
	if err != nil {
		log.Fatal(err)
	}
	foundUpper := false
	foundLower := false
	for _, f := range files {
		if f.Name() == "IMPORT" {
			foundUpper = true
		} else if f.Name() == "import" {
			foundLower = true
		}
	}
	assert.False(t, foundUpper)
	assert.True(t, foundLower)
}

func TestCaseSensitive(t *testing.T) { // Test when case sensitive is specified
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "SRC.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")

	r := runTransferOpts(t, logger, &GitParserOptions{
		caseInsensitive: false,
		config:          &config.Config{ImportDepot: "IMPORT", DefaultBranch: "main"},
	})
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//IMPORT/main/SRC.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 print -q //IMPORT/main/SRC.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	files, err := ioutil.ReadDir(r)
	if err != nil {
		log.Fatal(err)
	}
	foundUpper := false
	foundLower := false
	for _, f := range files {
		if f.Name() == "IMPORT" {
			foundUpper = true
		} else if f.Name() == "import" {
			foundLower = true
		}
	}
	assert.True(t, foundUpper)
	assert.False(t, foundLower)

}

func TestAddSameFile(t *testing.T) {
	// Ensure single archive in git
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	file1 := "file1.txt"
	file2 := "file2.txt"
	contents1 := "contents\n"
	writeToFile(file1, contents1)
	writeToFile(file2, contents1)
	runCmd("git add .")
	runCmd("git commit -m initial")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/file1.txt#1 - add change 2 (text+C)
//import/main/file2.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents1, result)

	result, err = runCmd("p4 print -q //import/main/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents1, result)

	result, err = runCmd("p4 fstat -Ob //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*file1.txt,d/1.2.gz`, result)

	result, err = runCmd("p4 fstat -Ob //import/main/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*file1.txt,d/1.2.gz`, result)
}

func TestAddBinary(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("gzip " + src)
	runCmd("git add .")
	runCmd("git commit -m initial")

	runTransfer(t, logger)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt.gz#1 - add change 2 (binary+F)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt.gz#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType binary\+F`, result)
	assert.Regexp(t, `lbrType binary\+F`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2$`, result)
}

func TestAddBinaryAndFileType(t *testing.T) {
	// Add a binary file and set the filetype to be binary, although file will be auto-detected as binary+F
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("gzip " + src)
	runCmd("git add .")
	runCmd("git commit -m initial")

	c := &config.Config{
		ImportDepot: "import", DefaultBranch: "main",
		ReTypeMaps: make([]config.RegexpTypeMap, 0),
	}
	rePath := regexp.MustCompile("//.*.gz$") // Go version of typemap
	c.ReTypeMaps = append(c.ReTypeMaps, config.RegexpTypeMap{Filetype: journal.Binary, RePath: rePath})

	opts := &GitParserOptions{config: c}

	runTransferOpts(t, logger, opts)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt.gz#1 - add change 2 (binary+F)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt.gz#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType binary\+F`, result)
	assert.Regexp(t, `lbrType binary\+F`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2$`, result)
}

func TestAddCRLF1(t *testing.T) {
	// Test where CRLF should be converted to LF - but only for text files!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	file1 := "file1.txt"
	srcContents1 := "contents\n"
	contents2 := "contents1\r\ncontents2\r\ncontents3\n"
	writeToFile(src, srcContents1)
	writeToFile(file1, contents2)
	runCmd("gzip " + src)
	runCmd("git add .")
	runCmd("git commit -m initial")

	runTransfer(t, logger)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/file1.txt#1 - add change 3 (text+C)
//import/main/src.txt.gz#1 - add change 3 (binary+F)
`, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt.gz#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType binary\+F`, result)
	assert.Regexp(t, `lbrType binary\+F`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.3$`, result)

	result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents2, result)
}

func TestAddCRLF2(t *testing.T) {
	// Test where CRLF should be converted to LF - but only for text files!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	file1 := "file1.txt"
	file2 := "file2.dat"
	file3 := "file3.dat"
	srcContents1 := "contents\n"
	contents1 := "contents1\r\ncontents2\r\ncontents3\n"
	contents2 := "contents1a\r\ncontents2a\r\ncontents3a\n"
	contents3 := "contents1b\r\ncontents2b\r\ncontents3b\n"
	writeToFile(src, srcContents1)
	writeToFile(file1, contents1)
	writeToFile(file2, contents2)
	runCmd("gzip " + src)
	runCmd("git add .")
	runCmd("git commit -m initial")
	writeToFile(file2, contents3)
	runCmd("git add .")
	runCmd("git commit -m modified")
	writeToFile(file3, contents3)
	runCmd("git add .")
	runCmd("git commit -m added2")
	runCmd(fmt.Sprintf("rm %s", file3))
	runCmd("git add .")
	runCmd("git commit -m deleted")

	c := &config.Config{
		ImportDepot: "import", DefaultBranch: "main",
		ReTypeMaps: make([]config.RegexpTypeMap, 0),
	}
	rePath := regexp.MustCompile("//.*.txt$") // Go version of typemap
	c.ReTypeMaps = append(c.ReTypeMaps, config.RegexpTypeMap{Filetype: journal.CText, RePath: rePath})
	rePath = regexp.MustCompile("//.*.dat$") // Go version of typemap
	c.ReTypeMaps = append(c.ReTypeMaps, config.RegexpTypeMap{Filetype: journal.Binary, RePath: rePath})

	opts := &GitParserOptions{config: c, convertCRLF: true}
	runTransferOpts(t, logger, opts)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/file1.txt#1 - add change 4 (text+C)
//import/main/file2.dat#2 - edit change 6 (binary)
//import/main/file3.dat#2 - delete change 8 (binary)
//import/main/src.txt.gz#1 - add change 4 (binary+F)
`, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, strings.ReplaceAll(contents1, "\r\n", "\n"), result)

	result, err = runCmd("p4 print -q //import/main/file2.dat#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents2, result)
}

func TestAddEmpty(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	writeToFile(src, "")
	runCmd("git add .")
	runCmd("git commit -m initial")

	runTransfer(t, logger)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)

	result, err = runCmd("p4 fstat -Ol //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `fileSize 0`, result)
}

func TestAddWildcard(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "src@wild.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	srcContents2 := "contents\nappended\n"
	writeToFile(src, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	user := getGitUser(t)

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src%40wild.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/main/src%40wild.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src%40wild.txt#2 - edit change 4 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/main/src%40wild.txt#2")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents2, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src%40wild.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*/1.4.gz`, result)

	result, err = runCmd("p4 changes")
	assert.Equal(t, nil, err)
	assert.Regexp(t, fmt.Sprintf(`Change 4 on .* by %s@git\-client`, user), result)
	assert.Regexp(t, fmt.Sprintf(`Change 2 on .* by %s@git\-client`, user), result)
	result, err = runCmd("p4 changes //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, fmt.Sprintf(`Change 4 on .* by %s@git\-client`, user), result)
	assert.Regexp(t, fmt.Sprintf(`Change 2 on .* by %s@git\-client`, user), result)
	result, err = runCmd("p4 storage //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrRev 1.2`, result)
	assert.Regexp(t, `lbrRev 1.4`, result)
}

func TestAllWildcards(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	file1 := "wild@.txt"
	file2 := "wild%.txt"
	file3 := "wild#.txt"
	file4 := "wild*.txt"
	file5 := "wild%@#*.txt"
	files := []string{file1, file2, file3, file4, file5}
	contents1 := "contents\n"
	for _, f := range files {
		writeToFile(f, contents1)
	}
	runCmd("git add .")
	runCmd("git commit -m initial")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/wild%23.txt#1 - add change 2 (text+C)
//import/main/wild%25%40%23%2A.txt#1 - add change 2 (text+C)
//import/main/wild%25.txt#1 - add change 2 (text+C)
//import/main/wild%2A.txt#1 - add change 2 (text+C)
//import/main/wild%40.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	for _, f := range files {
		result, err = runCmd(fmt.Sprintf("p4 print -q //import/main/%s#1", journal.ReplaceWildcards(f)))
		assert.Equal(t, nil, err)
		assert.Equal(t, contents1, result)
	}

}

func TestDeleteFile(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("rm " + src)
	runCmd("git add .")
	runCmd("git commit -m deleted")

	runTransfer(t, logger)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#2 - delete change 3 (text+C)\n", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text`, result)
	assert.NotRegexp(t, `lbrType text`, result)
	assert.NotRegexp(t, `(?m)lbrPath `, result)

}

func TestDeleteAdd(t *testing.T) {
	// Delete a file and add back again
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("rm " + src)
	runCmd("git add .")
	runCmd("git commit -m deleted")
	srcContents2 := "contents2\n"
	writeToFile(src, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m added-back")

	runTransfer(t, logger)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#3 - add change 5 (text+C)\n", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.NotRegexp(t, `lbrType text`, result)
	assert.NotRegexp(t, `(?m)lbrPath `, result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.5.gz$`, result)

}

func TestRename(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m deleted")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#1")
	assert.Regexp(t, `headType text\+C`, result)
	assert.Equal(t, nil, err)

	result, err = runCmd("p4 files //...@3")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src.txt#2 - delete change 3 (text+C)
//import/main/targ.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 fstat -Ob //import/main/targ.txt#1")
	assert.Regexp(t, `headType text\+C`, result)
	assert.Equal(t, nil, err)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/targ.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)
}

func TestRename2(t *testing.T) {
	// Rename of a file with 2 revs
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	srcContents2 := "contents2\n"
	writeToFile(src, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m edited")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m deleted")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 fstat -Ob //import/main/src.txt#1")
	assert.Regexp(t, `headType text\+C`, result)
	assert.Equal(t, nil, err)

	result, err = runCmd("p4 files //...@4")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#2 - edit change 4 (text+C)\n", result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src.txt#3 - delete change 5 (text+C)
//import/main/targ.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/targ.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.4.gz$`, result)
}

func TestRenameOnBranch(t *testing.T) {
	// Rename of a file on a branch
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	file1 := "file1.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	contents2 := "contents2\n"
	writeToFile(file1, contents2)
	runCmd("git add .")
	runCmd("git commit -m 'a file changed on dev'")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m renamed")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/file1.txt#1 - add change 4 (text+C)
//import/dev/src.txt#1 - delete change 5 (text+C)
//import/dev/targ.txt#1 - add change 5 (text+C)
//import/main/src.txt#1 - add change 2 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog -i //import/dev/file1.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file1.txt`, result)
	assert.Regexp(t, `... #1 change 4 add on \S+ by \S+ \S+`, result)

	result, err = runCmd("p4 filelog -i //import/dev/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/src.txt
... #1 change 5 delete on .* by .* \S+ 'renamed '
... ... delete from //import/main/src.txt#1`, result)

	result, err = runCmd("p4 filelog -i //import/dev/targ.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `... #1 change 2 add on \S+ by \S+ \S+`, result)
	assert.Regexp(t, `... ... delete into //import/dev/src.txt#1`, result)
	assert.Regexp(t, `... ... branch into //import/dev/targ.txt#1`, result)

	result, err = runCmd("p4 filelog -i //import/main/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `... #1 change 2 add on \S+ by \S+ \S+`, result)
	assert.Regexp(t, `... ... delete into //import/dev/src.txt#1`, result)
	assert.Regexp(t, `... ... branch into //import/dev/targ.txt#1`, result)
}

// func TestAddOfMergedFile(t *testing.T) {
// 	// Add a file on branch which is merged in from main
// 	logger := createLogger()
// 	logger.Debugf("======== Test: %s", t.Name())

// 	d := createGitRepo(t)
// 	os.Chdir(d)
// 	logger.Debugf("Git repo: %s", d)

// 	file1 := "file1.txt"
// 	contents1 := "contents\n"
// 	file2 := "file2.txt"
// 	file3 := "file3.txt"
// 	writeToFile(file1, contents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m initial")
// 	runCmd("git switch -c dev")
// 	contents11 := "contents11\n"
// 	writeToFile(file1, contents11)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'a file changed on dev'")
// 	runCmd("git switch main")
// 	runCmd("git switch -c dev2")
// 	contents3 := "contents3\n"
// 	writeToFile(file3, contents3)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'a file changed on dev2'")
// 	contents21 := "contents21\n"
// 	writeToFile(file2, contents21)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'add file2'")
// 	runCmd("git switch main")
// 	runCmd("git merge dev2 --no-ff")
// 	runCmd("git switch dev")
// 	runCmd("git merge main --no-ff")
// 	runCmd("git log --graph --abbrev-commit --oneline")

// 	r := runTransfer(t, logger)
// 	logger.Debugf("Server root: %s", r)

// 	result, err := runCmd("p4 verify -qu //...")
// 	assert.Equal(t, "", result)
// 	assert.Equal(t, "<nil>", fmt.Sprint(err))

// 	result, err = runCmd("p4 files //...")
// 	assert.Equal(t, nil, err)
// 	assert.Equal(t, `//import/dev/file1.txt#1 - add change 4 (text+C)
// //import/dev/file2.txt#1 - delete change 5 (text+C)
// //import/dev/file3.txt#1 - add change 5 (text+C)
// //import/main/file2.txt#1 - add change 2 (text+C)
// `,
// 		result)

// 	result, err = runCmd("p4 filelog -i //import/dev/file1.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `//import/dev/file1.txt`, result)
// 	assert.Regexp(t, `... #1 change 4 add on \S+ by \S+ \S+`, result)

// 	result, err = runCmd("p4 filelog -i //import/dev/file2.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 5 delete on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... delete from //import/main/file2.txt#1`, result)

// 	result, err = runCmd("p4 filelog -i //import/dev/file3.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 2 add on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... delete into //import/dev/file2.txt#1`, result)
// 	assert.Regexp(t, `... ... branch into //import/dev/file3.txt#1`, result)

// 	result, err = runCmd("p4 filelog -i //import/main/file2.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 2 add on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... delete into //import/dev/file2.txt#1`, result)
// 	assert.Regexp(t, `... ... branch into //import/dev/file3.txt#1`, result)
// }

func TestRenameRename(t *testing.T) {
	// Rename of a file done twice
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	targ2 := "targ2.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	srcContents2 := "contents2\n"
	writeToFile(src, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m edited")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m renamed")
	runCmd(fmt.Sprintf("mv %s %s", targ, targ2))
	runCmd("git add .")
	runCmd("git commit -m renamed-again")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 files //...@4")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/src.txt#2 - edit change 4 (text+C)\n", result)

	result, err = runCmd("p4 files //...@5")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src.txt#3 - delete change 5 (text+C)
//import/main/targ.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src.txt#3 - delete change 5 (text+C)
//import/main/targ.txt#2 - delete change 6 (text+C)
//import/main/targ2.txt#1 - add change 6 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/targ.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.4.gz$`, result)

	result, err = runCmd("p4 fstat -Ob //import/main/targ2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.4.gz$`, result)
}

func TestRenameBack(t *testing.T) {
	// Rename of a file and renamed back
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m renamed")
	runCmd(fmt.Sprintf("mv %s %s", targ, src))
	runCmd("git add .")
	runCmd("git commit -m renamed-back")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src.txt#3 - add change 4 (text+C)
//import/main/targ.txt#2 - delete change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //import/main/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/main/src.txt
... #3 change 4 add on .* by \S+ \S+ 'renamed-back '
... ... branch from //import/main/targ.txt#1
... #2 change 3 delete on .* by \S+ \S+ 'renamed '
... #1 change 2 add on .* by \S+ \S+ 'initial '
... ... branch into //import/main/targ.txt#1
`, result)

	result, err = runCmd("p4 filelog //import/main/targ.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/main/targ.txt
... #2 change 4 delete on \S+ by \S+ \S+ 'renamed-back '
... #1 change 3 add on \S+ by \S+ \S+ 'renamed '
... ... branch into //import/main/src.txt#3
... ... branch from //import/main/src.txt#1
`, result)
}

func TestRenameOnBranchOriginalDeleted(t *testing.T) {
	// Rename of a file on a branch where original file is deleted and a merge is created
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 9
contents

blob
mark :2
data 10
contents2

blob
mark :3
data 10
contents3

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt
M 100644 :2 src/file2.txt

reset refs/heads/dev
commit refs/heads/dev
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
added-one
from :3
M 100644 :3 src/file2.txt

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
delet-two
from :3
D src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
renamed00
from :4
merge :5
R src/file1.txt targ/file1.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#1 - delete change 6 (text+C)
//import/dev/src/file2.txt#1 - add change 4 (text+C)
//import/dev/targ/file1.txt#1 - add change 6 (text+C)
//import/main/src/file1.txt#2 - delete change 5 (text+C)
//import/main/src/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //import/...")
	reExpected := `//import/dev/src/file1.txt
... #1 change 6 delete on \S+ by \S+ \S+ 'renamed00 '
... ... delete from //import/main/src/file1.txt#1
//import/dev/src/file2.txt
... #1 change 4 add on \S+ by \S+ \S+ 'added-one '
... ... branch from //import/main/src/file2.txt#1
//import/dev/targ/file1.txt
... #1 change 6 add on \S+ by \S+ \S+ 'renamed00 '
... ... branch from //import/main/src/file1.txt#1
//import/main/src/file1.txt
... #2 change 5 delete on \S+ by \S+ \S+ 'delet-two '
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file1.txt#1
... ... branch into //import/dev/targ/file1.txt#1
//import/main/src/file2.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... edit into //import/dev/src/file2.txt#1
`

	assert.Equal(t, nil, err)
	compareFilelog(t, reExpected, result)
	assert.Regexp(t, reExpected, result)

}

func TestDirtyRename(t *testing.T) {
	// Rename of a file where target has new contents
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 9
contents

blob
mark :2
data 10
contents2

blob
mark :3
data 10
contents3

reset refs/heads/main
commit refs/heads/main
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 file1.txt

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 5
edit
from :4
M 100644 :2 file1.txt

reset refs/heads/main
commit refs/heads/main
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
rename
from :5
R file1.txt file2.txt
M 100644 :3 file2.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/file1.txt#3 - delete change 6 (text+C)
//import/main/file2.txt#1 - add change 6 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //import/...")
	reExpected := `//import/main/file1.txt
... #3 change 6 delete on \S+ by \S+ \S+ 'rename '
... #2 change 5 edit on \S+ by \S+ \S+ 'edit '
... ... branch into //import/main/file2.txt#1
... #1 change 4 add on \S+ by \S+ \S+ 'initial '
//import/main/file2.txt
... #1 change 6 add on \S+ by \S+ \S+ 'rename '
... ... branch from //import/main/file1.txt#2
`

	assert.Equal(t, nil, err)
	compareFilelog(t, reExpected, result)
	assert.Regexp(t, reExpected, result)

	result, err = runCmd("p4 print -q //import/main/file2.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents3\n", result)
}

func TestDirtyRenameOnBranch(t *testing.T) {
	// Rename of a file on a branch where target has new contents
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 9
contents

blob
mark :2
data 10
contents2

blob
mark :3
data 10
contents3

reset refs/heads/main
commit refs/heads/main
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 file1.txt

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 5
edit
from :4
M 100644 :2 file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
rename
from :5
R file1.txt file2.txt
M 100644 :3 file2.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/file1.txt#1 - delete change 6 (text+C)
//import/dev/file2.txt#1 - add change 6 (text+C)
//import/main/file1.txt#2 - edit change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //import/...")
	reExpected := `//import/dev/file1.txt
... #1 change 6 delete on \S+ by \S+ \S+ 'rename '
... ... delete from //import/main/file1.txt#1,#2
//import/dev/file2.txt
... #1 change 6 add on \S+ by \S+ \S+ 'rename '
... ... branch from //import/main/file1.txt#2
//import/main/file1.txt
... #2 change 5 edit on \S+ by \S+ \S+ 'edit '
... ... delete into //import/dev/file1.txt#1
... ... branch into //import/dev/file2.txt#1
... #1 change 4 add on \S+ by \S+ \S+ 'initial '
`

	assert.Equal(t, nil, err)
	compareFilelog(t, reExpected, result)
	assert.Regexp(t, reExpected, result)

	result, err = runCmd("p4 print -q //import/dev/file2.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents3\n", result)
}

func TestRenameOnBranchWithEdit(t *testing.T) {
	// Rename of a file on a branch with edited contents so R and M records, then merge back to main
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	file1 := "file1.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	contents2 := "contents2\n"
	writeToFile(file1, contents2)
	runCmd("git add .")
	runCmd("git commit -m 'a file changed on dev'")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	appendToFile(targ, "1") // Small extra char - should still be a rename
	runCmd("git add .")
	runCmd("git commit -m renamed")
	runCmd("git switch main")
	runCmd("git merge --no-ff dev")
	runCmd("git commit -m \"merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/file1.txt#1 - add change 4 (text+C)
//import/dev/src.txt#1 - delete change 6 (text+C)
//import/dev/targ.txt#1 - add change 6 (text+C)
//import/main/file1.txt#1 - add change 7 (text+C)
//import/main/src.txt#2 - delete change 7 (text+C)
//import/main/targ.txt#1 - add change 7 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //import/...")
	reExpected := `//import/dev/file1.txt
... #1 change 4 add on \S+ by \S+ \S+ 'a file changed on dev '
... ... edit into //import/main/file1.txt#1
//import/dev/src.txt
... #1 change 6 delete on \S+ by \S+ \S+ 'renamed '
... ... delete into //import/main/src.txt#2
... ... delete from //import/main/src.txt#1
//import/dev/targ.txt
... #1 change 6 add on \S+ by \S+ \S+ 'renamed '
... ... branch from //import/main/src.txt#1
... ... branch into //import/main/targ.txt#1
//import/main/file1.txt
... #1 change 7 add on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... branch from //import/dev/file1.txt#1
//import/main/src.txt
... #2 change 7 delete on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... delete from //import/dev/src.txt#1
... #1 change 2 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src.txt#1
... ... branch into //import/dev/targ.txt#1
//import/main/targ.txt
... #1 change 7 add on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... branch from //import/dev/targ.txt#1
`

	assert.Equal(t, nil, err)
	compareFilelog(t, reExpected, result)
	assert.Regexp(t, reExpected, result)

}

// WIP - not provoking quite the right behaviour for now
// func TestMergeRenameOfBranchWithEdit(t *testing.T) {
// 	// Rename of a file on a branch with edited contents so R and M records, then merge back to main
// 	debug = true
// 	logger := createLogger()
// 	logger.Debugf("======== Test: %s", t.Name())

// 	d := createGitRepo(t)
// 	os.Chdir(d)
// 	logger.Debugf("Git repo: %s", d)

// 	src := "src.txt"
// 	targ := "targ.txt"
// 	file1 := "file1.txt"
// 	srcContents1 := "contents\n"
// 	writeToFile(src, srcContents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m initial")
// 	runCmd("git switch -c dev")
// 	writeToFile(file1, srcContents1+"1")
// 	runCmd("git add .")
// 	runCmd("git commit -m 'a file changed on dev'")
// 	runCmd(fmt.Sprintf("mv %s %s", src, targ))
// 	appendToFile(targ, "1") // Small extra char - should still be a rename
// 	runCmd("git add .")
// 	runCmd("git commit -m renamed")
// 	// Delete file on dev and make it same contents as the renamed one.
// 	runCmd(fmt.Sprintf("git rm %s", file1))
// 	runCmd("git add .")
// 	runCmd("git commit -m deleted")
// 	runCmd("git switch main")
// 	runCmd("git merge --no-ff dev")
// 	runCmd("git commit -m \"merged edited change\"")
// 	runCmd("git log --graph --abbrev-commit --oneline")

// 	r := runTransfer(t, logger)
// 	logger.Debugf("Server root: %s", r)

// 	result, err := runCmd("p4 verify -qu //...")
// 	assert.Equal(t, "", result)
// 	assert.Equal(t, "<nil>", fmt.Sprint(err))

// 	result, err = runCmd("p4 files //...")
// 	assert.Equal(t, nil, err)
// 	assert.Equal(t, `//import/dev/file1.txt#1 - add change 4 (text+C)
// //import/dev/src.txt#1 - delete change 6 (text+C)
// //import/dev/targ.txt#1 - add change 6 (text+C)
// //import/main/file1.txt#1 - add change 7 (text+C)
// //import/main/src.txt#2 - delete change 7 (text+C)
// //import/main/targ.txt#1 - add change 7 (text+C)
// `,
// 		result)

// 	result, err = runCmd("p4 filelog //...")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `//import/dev/file1.txt`, result)

// 	result, err = runCmd("p4 filelog //import/dev/file1.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `//import/dev/file1.txt`, result)
// 	assert.Regexp(t, `... #1 change 4 add on \S+ by \S+ \S+`, result)

// 	result, err = runCmd("p4 filelog //import/dev/src.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 6 delete on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... delete from //import/main/src.txt#1`, result)

// 	result, err = runCmd("p4 filelog //import/dev/targ.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 6 add on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... branch from //import/main/src.txt#1`, result)

// 	result, err = runCmd("p4 filelog //import/main/src.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 2 add on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... delete into //import/dev/src.txt#1`, result)
// 	assert.Regexp(t, `... ... branch into //import/dev/targ.txt#1`, result)

// 	result, err = runCmd("p4 filelog //import/main/targ.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `... #1 change 7 add on \S+ by \S+ \S+`, result)
// 	assert.Regexp(t, `... ... branch from //import/dev/targ.txt#1`, result)
// }

func TestBranchOfDeletedFile(t *testing.T) {
	// Rename of a file on a branch with edited contents so R and M records, then merge back to main
	debug = true
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	runCmd("git switch main")
	contents2 := "contents2\n"
	writeToFile(src, contents2)
	runCmd("git add .")
	runCmd("git commit -m 'a file changed on main'")
	runCmd("git rm " + src)
	runCmd("git add .")
	runCmd("git commit -m deleted")
	runCmd("git switch dev")
	runCmd("git merge --no-ff main")
	writeToFile(src, contents2)
	runCmd("git commit -m \"merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "M 100644 :3 src.txt")
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src.txt#1 - add change 6 (text+C)
//import/main/src.txt#3 - delete change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/src.txt
... #1 change 6 add on \S+ by \S+@git-client \S+ 'Merge branch 'main' into dev '
... ... branch from //import/main/src.txt#2
//import/main/src.txt
... #3 change 5 delete on \S+ by \S+@git-client \S+ 'deleted '
... #2 change 4 edit on \S+ by \S+@git-client \S+ 'a file changed on main '
... ... edit into //import/dev/src.txt#1
... #1 change 2 add on \S+ by \S+@git-client \S+ 'initial '`,
		result)
}

func TestRenameDir(t *testing.T) {
	// Git rename of a dir consisting of multiple files - expand to constituent parts
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ")
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#1 - add change 2 (text+C)
//import/main/src/file2.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 fstat -Ob //import/main/src/file.txt#1")
	assert.Regexp(t, `headType text\+C`, result)
	assert.Equal(t, nil, err)

	result, err = runCmd("p4 files //...@3")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#2 - delete change 3 (text+C)
//import/main/src/file2.txt#2 - delete change 3 (text+C)
//import/main/targ/file.txt#1 - add change 3 (text+C)
//import/main/targ/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 fstat -Ob //import/main/targ/file.txt#1")
	assert.Regexp(t, `headType text\+C`, result)
	assert.Equal(t, nil, err)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/targ/file.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src/file.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)
}

func TestRenameDirWithDelete(t *testing.T) {
	// Similar to TestRenameDir but with a deleted file that should not be renamed.
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("rm " + src1)
	runCmd("git add .")
	runCmd("git commit -m deleted")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved-dir")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ")
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#1 - add change 2 (text+C)
//import/main/src/file2.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#2 - delete change 3 (text+C)
//import/main/src/file2.txt#2 - delete change 4 (text+C)
//import/main/targ/file2.txt#1 - add change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/targ/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src/file.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)
}

func TestDoubleRename(t *testing.T) {
	// Similar to TestRenameDir but with the rename of a file just renamed as part of the directory.
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("rm " + src1)
	runCmd("git add .")
	runCmd("git commit -m deleted")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved-dir")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ")
	newLines = append(newLines, "R targ/file2.txt targ/file3.txt")
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#1 - add change 2 (text+C)
//import/main/src/file2.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#2 - delete change 3 (text+C)
//import/main/src/file2.txt#2 - delete change 4 (text+C)
//import/main/targ/file3.txt#1 - add change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/targ/file3.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/src/file.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)
}

func TestPseudoRename(t *testing.T) {
	// Rename of a file where the same file is also added as source
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	srcContents2 := "contents2\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("rm " + src1)
	runCmd("git add .")
	runCmd("git commit -m deleted")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved-dir")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ") // Rename dir
	// newLines = append(newLines, "R targ/file2.txt targ/file3.txt") // Double rename of file
	newLines = append(newLines, "M 100644 :2 src/file2.txt") // Add back source file
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#2 - delete change 4 (text+C)
//import/main/src/file2.txt#2 - edit change 5 (text+C)
//import/main/targ/file2.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/main/src/file.txt
... #2 change 4 delete on \S+ by \S+ \S+ 'deleted '
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
//import/main/src/file2.txt
... #2 change 5 edit on \S+ by \S+ \S+ 'moved-dir '
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... branch into //import/main/targ/file2.txt#1
//import/main/targ/file2.txt
... #1 change 5 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file2.txt#1
`,
		result)
}

func TestPseudoRenameMerge(t *testing.T) {
	// Rename of a file where the same file is also added as source then merged
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	srcContents2 := "contents2\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved-dir")
	runCmd("git switch main")
	runCmd("git merge --no-ff dev")
	runCmd("git commit -m \"merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ") // Rename dir
	// newLines = append(newLines, "R targ/file2.txt targ/file3.txt") // Double rename of file
	newLines = append(newLines, "M 100644 :2 src/file2.txt") // Add back source file
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file.txt#1 - delete change 4 (text+C)
//import/dev/src/file2.txt#1 - delete change 4 (text+C)
//import/dev/targ/file.txt#1 - add change 4 (text+C)
//import/dev/targ/file2.txt#1 - add change 4 (text+C)
//import/main/src/file.txt#2 - delete change 5 (text+C)
//import/main/src/file2.txt#2 - edit change 5 (text+C)
//import/main/targ/file.txt#1 - add change 5 (text+C)
//import/main/targ/file2.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file.txt
... #1 change 4 delete on \S+ by \S+ \S+ 'moved-dir '
... ... delete into //import/main/src/file.txt#2
... ... delete from //import/main/src/file.txt#1
//import/dev/src/file2.txt
... #1 change 4 delete on \S+ by \S+ \S+ 'moved-dir '
... ... delete from //import/main/src/file2.txt#1
//import/dev/targ/file.txt
... #1 change 4 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file.txt#1
... ... branch into //import/main/targ/file.txt#1
//import/dev/targ/file2.txt
... #1 change 4 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file2.txt#1
... ... branch into //import/main/targ/file2.txt#1
//import/main/src/file.txt
... #2 change 5 delete on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... delete from //import/dev/src/file.txt#1
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file.txt#1
... ... branch into //import/dev/targ/file.txt#1
//import/main/src/file2.txt
... #2 change 5 edit on \S+ by \S+ \S+ 'Merge branch 'dev' '
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file2.txt#1
... ... branch into //import/dev/targ/file2.txt#1
//import/main/targ/file.txt
... #1 change 5 add on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... branch from //import/dev/targ/file.txt#1
//import/main/targ/file2.txt
... #1 change 5 add on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... branch from //import/dev/targ/file2.txt#1
`

	// assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestPseudoRenameBranch(t *testing.T) {
	// Rename of a file where the same file is also added as source on a branch
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	srcContents2 := "contents2\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved-dir")
	runCmd("git switch main")
	runCmd("git merge --no-ff dev")
	runCmd("git commit -m \"merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ") // Rename dir
	// newLines = append(newLines, "R targ/file2.txt targ/file3.txt") // Double rename of file
	newLines = append(newLines, "M 100644 :2 src/file2.txt") // Add back source file
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file.txt#1 - delete change 4 (text+C)
//import/dev/src/file2.txt#1 - delete change 4 (text+C)
//import/dev/targ/file.txt#1 - add change 4 (text+C)
//import/dev/targ/file2.txt#1 - add change 4 (text+C)
//import/main/src/file.txt#2 - delete change 5 (text+C)
//import/main/src/file2.txt#2 - edit change 5 (text+C)
//import/main/targ/file.txt#1 - add change 5 (text+C)
//import/main/targ/file2.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/main/src/file.txt
... #2 change 5 delete on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... delete from //import/dev/src/file.txt#1
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file.txt#1
... ... branch into //import/dev/targ/file.txt#1
//import/main/src/file2.txt
... #2 change 5 edit on \S+ by \S+ \S+ 'Merge branch 'dev' '
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file2.txt#1
... ... branch into //import/dev/targ/file2.txt#1
//import/main/targ/file.txt
... #1 change 5 add on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... branch from //import/dev/targ/file.txt#1
//import/main/targ/file2.txt
... #1 change 5 add on \S+ by \S+ \S+ 'Merge branch 'dev' '
... ... branch from //import/dev/targ/file2.txt#1
`,
		result)
}

func TestRenameBranchWithEdit(t *testing.T) {
	// Rename of a file where the same file is also edited
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	srcContents2 := "contents2\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	runCmd("git mv src targ")
	runCmd("git add .")
	runCmd("git commit -m moved-dir")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src targ") // Rename dir
	// newLines = append(newLines, "R targ/file2.txt targ/file3.txt") // Double rename of file
	newLines = append(newLines, "M 100644 :2 src/file2.txt") // Add back source file
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file.txt#1 - delete change 4 (text+C)
//import/dev/src/file2.txt#2 - edit change 4 (text+C)
//import/dev/targ/file.txt#1 - add change 4 (text+C)
//import/dev/targ/file2.txt#1 - add change 4 (text+C)
//import/main/src/file.txt#1 - add change 3 (text+C)
//import/main/src/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/src/file.txt
... #1 change 4 delete on \S+ by \S+ \S+ 'moved-dir '
... ... delete from //import/main/src/file.txt#1
//import/dev/src/file2.txt
... #2 change 4 edit on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file2.txt#1
//import/dev/targ/file.txt
... #1 change 4 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file.txt#1
//import/dev/targ/file2.txt
... #1 change 4 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file2.txt#1
//import/main/src/file.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file.txt#1
... ... branch into //import/dev/targ/file.txt#1
//import/main/src/file2.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... edit into //import/dev/src/file2.txt#2
... ... branch into //import/dev/targ/file2.txt#1
`,
		result)
}

func TestDoubleRenameOnBranch(t *testing.T) {
	// Same file is renamed to 2 different targets
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 9
contents

blob
mark :2
data 10
contents2

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file.txt
M 100644 :2 src/file2.txt

commit refs/heads/dev
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
added-one
from :3
M 100644 :2 src/file1.txt

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
added-two
from :3
M 100644 :2 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
moved-dir
from :4
merge :5
R src/file2.txt src/file3.txt
R src targ
D src
`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file.txt#1 - delete change 6 (text+C)
//import/dev/src/file1.txt#2 - delete change 6 (text+C)
//import/dev/src/file2.txt#1 - delete change 6 (text+C)
//import/dev/targ/file.txt#1 - add change 6 (text+C)
//import/dev/targ/file1.txt#1 - add change 6 (text+C)
//import/dev/targ/file3.txt#1 - add change 6 (text+C)
//import/main/src/file.txt#1 - add change 3 (text+C)
//import/main/src/file1.txt#1 - add change 5 (text+C)
//import/main/src/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file.txt
... #1 change 6 delete on \S+ by \S+ \S+ 'moved-dir '
... ... delete from //import/main/src/file.txt#1
//import/dev/src/file1.txt
... #2 change 6 delete on \S+ by \S+ \S+ 'moved-dir '
... #1 change 4 add on \S+ by \S+ \S+ 'added-one '
... ... branch into //import/dev/targ/file1.txt#1
//import/dev/src/file2.txt
... #1 change 6 delete on \S+ by \S+ \S+ 'moved-dir '
... ... delete from //import/main/src/file2.txt#1
//import/dev/targ/file.txt
... #1 change 6 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file.txt#1
//import/dev/targ/file1.txt
... #1 change 6 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/dev/src/file1.txt#1
//import/dev/targ/file3.txt
... #1 change 6 add on \S+ by \S+ \S+ 'moved-dir '
... ... branch from //import/main/src/file2.txt#1
//import/main/src/file.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file.txt#1
... ... branch into //import/dev/targ/file.txt#1
//import/main/src/file1.txt
... #1 change 5 add on \S+ by \S+ \S+ 'added-two '
//import/main/src/file2.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file2.txt#1
... ... branch into //import/dev/targ/file3.txt#1
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestDeleteDirWithModifyOnBranch(t *testing.T) {
	// File is deleted on branch and modified in same commit - should only be 1 action
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 9
contents

blob
mark :2
data 10
contents2

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt
M 100644 :2 src/file2.txt

commit refs/heads/dev
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
added-one
from :3
D src
M 100644 :2 src/file1.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#1 - add change 4 (text+C)
//import/dev/src/file2.txt#1 - delete change 4 (text+C)
//import/main/src/file1.txt#1 - add change 3 (text+C)
//import/main/src/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file1.txt
... #1 change 4 add on \S+ by \S+ \S+ 'added-one '
... ... branch from //import/main/src/file1.txt#1
//import/dev/src/file2.txt
... #1 change 4 delete on \S+ by \S+ \S+ 'added-one '
//import/main/src/file1.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
... ... edit into //import/dev/src/file1.txt#1
//import/main/src/file2.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestDeleteDirWithModifyAndMerge(t *testing.T) {
	// File is deleted on branch and modified in same commit with a merge - should only be 1 action
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

blob
mark :4
data 11
contents04

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
05devmrg
from :5
M 100644 :3 src/file2.txt

reset refs/heads/main
commit refs/heads/main
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
07main
from :5
M 100644 :2 src/file2.txt

reset refs/heads/dev
commit refs/heads/dev
mark :8
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
05devmrg
from :6
merge :7
D src
M 100644 :3 src/file1.txt
`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#1 - add change 8 (text+C)
//import/dev/src/file2.txt#2 - delete change 8 (text+C)
//import/main/src/file1.txt#1 - add change 5 (text+C)
//import/main/src/file2.txt#1 - add change 7 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file1.txt
... #1 change 8 add on \S+ by \S+ \S+ '05devmrg '
... ... branch from //import/main/src/file1.txt#1
//import/dev/src/file2.txt
... #2 change 8 delete on \S+ by \S+ \S+ '05devmrg '
... #1 change 6 add on \S+ by \S+ \S+ '05devmrg '
//import/main/src/file1.txt
... #1 change 5 add on \S+ by \S+ \S+ 'initial '
... ... edit into //import/dev/src/file1.txt#1
//import/main/src/file2.txt
... #1 change 7 add on \S+ by \S+ \S+ '07main '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestRenameRenameBackWithMerge(t *testing.T) {
	// Similar to TestCommitValidDirRenameRenameBackDelete2
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

blob
mark :4
data 11
contents04

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
06dev
from :5
M 100644 :3 src/file2.txt

reset refs/heads/main
commit refs/heads/main
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
07main
from :5
D src/file1.txt

reset refs/heads/main
commit refs/heads/main
mark :8
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
08main
from :7
M 100644 :2 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :9
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
05devmrg
from :6
merge :8
R src temp
M 100644 :4 src/file1.txt
R temp/file1.txt src/file1.txt
D temp

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file2.txt#2 - delete change 9 (text+C)
//import/main/src/file1.txt#3 - add change 8 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file2.txt
... #2 change 9 delete on \S+ by \S+ \S+ '05devmrg '
... #1 change 6 add on \S+ by \S+ \S+ '06dev '
//import/main/src/file1.txt
... #3 change 8 add on \S+ by \S+ \S+ '08main '
... #2 change 7 delete on \S+ by \S+ \S+ '07main '
... #1 change 5 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestRenameOfNonExistantSource(t *testing.T) {
	// Do a rename where source file doesn't exist
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

blob
mark :4
data 11
contents04

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/main
commit refs/heads/main
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
main02
from :5
M 100644 :1 src/file2.txt

reset refs/heads/dev1
commit refs/heads/dev1
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
06dev
from :5
M 100644 :3 src/file2.txt

reset refs/heads/dev2
commit refs/heads/dev2
mark :8
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
07main
from :7
M 100644 :4 src/file3.txt

reset refs/heads/dev2
commit refs/heads/dev2
mark :9
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
08main
from :8
merge :6
R src targ
R src/file1b.txt targ/file1.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev1/src/file2.txt#1 - add change 7 (text+C)
//import/dev2/src/file1.txt#1 - delete change 9 (text+C)
//import/dev2/src/file2.txt#1 - delete change 9 (text+C)
//import/dev2/src/file3.txt#2 - delete change 9 (text+C)
//import/dev2/targ/file1.txt#1 - add change 9 (text+C)
//import/dev2/targ/file2.txt#1 - add change 9 (text+C)
//import/dev2/targ/file3.txt#1 - add change 9 (text+C)
//import/main/src/file1.txt#1 - add change 5 (text+C)
//import/main/src/file2.txt#1 - add change 6 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev1/src/file2.txt
... #1 change 7 add on \S+ by \S+ \S+ '06dev '
... ... branch from //import/main/src/file2.txt#1
//import/dev2/src/file1.txt
... #1 change 9 delete on \S+ by \S+ \S+ '08main '
... ... delete from //import/main/src/file1.txt#1
//import/dev2/src/file2.txt
... #1 change 9 delete on \S+ by \S+ \S+ '08main '
... ... delete from //import/main/src/file2.txt#1
//import/dev2/src/file3.txt
... #2 change 9 delete on \S+ by \S+ \S+ '08main '
... #1 change 8 add on \S+ by \S+ \S+ '07main '
... ... branch into //import/dev2/targ/file3.txt#1
//import/dev2/targ/file1.txt
... #1 change 9 add on \S+ by \S+ \S+ '08main '
... ... branch from //import/main/src/file1.txt#1
//import/dev2/targ/file2.txt
... #1 change 9 add on \S+ by \S+ \S+ '08main '
... ... branch from //import/main/src/file2.txt#1
//import/dev2/targ/file3.txt
... #1 change 9 add on \S+ by \S+ \S+ '08main '
... ... branch from //import/dev2/src/file3.txt#1
//import/main/src/file1.txt
... #1 change 5 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev2/src/file1.txt#1
... ... branch into //import/dev2/targ/file1.txt#1
//import/main/src/file2.txt
... #1 change 6 add on \S+ by \S+ \S+ 'main02 '
... ... edit into //import/dev1/src/file2.txt#1
... ... delete into //import/dev2/src/file2.txt#1
... ... branch into //import/dev2/targ/file2.txt#1
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestDirRenameTwice(t *testing.T) {
	// Similar to TestCommitValidDirRenameTwice
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	// {name: "temp", srcName: "src", action: rename},
	// {name: "src/file1.txt", srcName: "", action: modify},
	// {name: "targ", srcName: "temp", action: rename},
	// {name: "targ/file1.txt", srcName: "", action: modify},

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

blob
mark :4
data 11
contents04

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
06dev
from :5
M 100644 :2 src/file2.txt

reset refs/heads/main
commit refs/heads/main
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 7
08main
from :5
M 100644 :2 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :8
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
05devmrg
from :6
merge :7
R src temp
M 100644 :3 src/file1.txt
R temp/file1.txt targ/file1.txt
M 100644 :3 targ/file1.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#1 - add change 8 (text+C)
//import/dev/src/file2.txt#2 - delete change 8 (text+C)
//import/dev/targ/file1.txt#1 - add change 8 (text+C)
//import/dev/temp/file2.txt#1 - add change 8 (text+C)
//import/main/src/file1.txt#2 - edit change 7 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file1.txt
... #1 change 8 add on \S+ by \S+ \S+ '05devmrg '
... ... edit into //import/dev/targ/file1.txt#1
... ... branch from //import/main/src/file1.txt#2
//import/dev/src/file2.txt
... #2 change 8 delete on \S+ by \S+ \S+ '05devmrg '
... #1 change 6 add on \S+ by \S+ \S+ '06dev '
... ... branch into //import/dev/temp/file2.txt#1
//import/dev/targ/file1.txt
... #1 change 8 add on \S+ by \S+ \S+ '05devmrg '
... ... branch from //import/dev/src/file1.txt#1
//import/dev/temp/file2.txt
... #1 change 8 add on \S+ by \S+ \S+ '05devmrg '
... ... branch from //import/dev/src/file2.txt#1
//import/main/src/file1.txt
... #2 change 7 edit on \S+ by \S+ \S+ '08main '
... ... edit into //import/dev/src/file1.txt#1
... #1 change 5 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestMergeRenameEditFromBranch(t *testing.T) {
	// Same file is renamed and then edited and merged back
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

blob
mark :4
data 11
contents04

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
01devedit
from :5
R src/file.txt src/file2.txt
M 100644 :2 src/file2.txt

reset refs/heads/main
commit refs/heads/main
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 11
02mainedit
from :5
merge :6
R src/file.txt src/file2.txt

reset refs/heads/dev
commit refs/heads/dev
mark :8
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
03devren
from :6
R src/file2.txt src/file3.txt

commit refs/heads/dev
mark :9
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
04devedit
from :8
M 100644 :4 src/file3.txt

reset refs/heads/main
commit refs/heads/main
mark :10
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
05mainmrg
from :7
merge :9
R src/file2.txt src/file3.txt
M 100644 :4 src/file3.txt
`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file.txt#1 - delete change 6 (text+C)
//import/dev/src/file2.txt#2 - delete change 8 (text+C)
//import/dev/src/file3.txt#2 - edit change 9 (text+C)
//import/main/src/file.txt#2 - delete change 7 (text+C)
//import/main/src/file2.txt#2 - delete change 10 (text+C)
//import/main/src/file3.txt#1 - add change 10 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file.txt
... #1 change 6 delete on \S+ by \S+ \S+ '01devedit '
... ... delete into //import/main/src/file.txt#2
... ... delete from //import/main/src/file.txt#1
//import/dev/src/file2.txt
... #2 change 8 delete on \S+ by \S+ \S+ '03devren '
... ... delete into //import/main/src/file2.txt#2
... #1 change 6 add on \S+ by \S+ \S+ '01devedit '
... ... branch into //import/dev/src/file3.txt#1
... ... branch from //import/main/src/file.txt#1
... ... branch into //import/main/src/file2.txt#1
//import/dev/src/file3.txt
... #2 change 9 edit on \S+ by \S+ \S+ '04devedit '
... ... branch into //import/main/src/file3.txt#1
... #1 change 8 add on \S+ by \S+ \S+ '03devren '
... ... branch from //import/dev/src/file2.txt#1
//import/main/src/file.txt
... #2 change 7 delete on \S+ by \S+ \S+ '02mainedit '
... ... delete from //import/dev/src/file.txt#1
... #1 change 5 add on \S+ by \S+ \S+ 'initial '
... ... delete into //import/dev/src/file.txt#1
... ... branch into //import/dev/src/file2.txt#1
//import/main/src/file2.txt
... #2 change 10 delete on \S+ by \S+ \S+ '05mainmrg '
... ... delete from //import/dev/src/file2.txt#1,#2
... #1 change 7 add on \S+ by \S+ \S+ '02mainedit '
... ... branch from //import/dev/src/file2.txt#1
//import/main/src/file3.txt
... #1 change 10 add on \S+ by \S+ \S+ '05mainmrg '
... ... branch from //import/dev/src/file3.txt#1,#2
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)

	result, err = runCmd("p4 print -q //import/main/src/file3.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents04\n", result)
}

func TestRenameMergeOnBranchOfDeletedFile(t *testing.T) {
	// File is renamed on branch where original has been deleted
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

blob
mark :4
data 11
contents04

reset refs/heads/main
commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :6
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
01devedit
from :5
M 100644 :2 src/file1.txt

reset refs/heads/dev2
commit refs/heads/dev2
mark :7
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
03devren
from :5
R src/file1.txt targ/file1.txt
M 100644 :3 src/file1.txt

reset refs/heads/dev2
commit refs/heads/dev2
mark :8
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
03devren
from :7
D targ/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :9
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
03devren
from :6
merge :8
R src/file1.txt targ/file1.txt
M 100644 :3 targ/file1.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#2 - delete change 9 (text+C)
//import/dev/targ/file1.txt#1 - add change 9 (text+C)
//import/dev2/src/file1.txt#2 - edit change 7 (text+C)
//import/dev2/targ/file1.txt#2 - delete change 8 (text+C)
//import/main/src/file1.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file1.txt
... #2 change 9 delete on \S+ by \S+ \S+ '03devren '
... ... delete from //import/dev2/src/file1.txt#1,#2
... #1 change 6 add on \S+ by \S+ \S+ '01devedit '
... ... branch from //import/main/src/file1.txt#1
//import/dev/targ/file1.txt
... #1 change 9 add on \S+ by \S+ \S+ '03devren '
... ... branch from //import/dev2/targ/file1.txt#1,#2
//import/dev2/src/file1.txt
... #2 change 7 edit on \S+ by \S+ \S+ '03devren '
... ... delete into //import/dev/src/file1.txt#2
... ... branch from //import/main/src/file1.txt#1
//import/dev2/targ/file1.txt
... #2 change 8 delete on \S+ by \S+ \S+ '03devren '
... ... branch into //import/dev/targ/file1.txt#1
... #1 change 7 add on \S+ by \S+ \S+ '03devren '
... ... branch from //import/main/src/file1.txt#1
//import/main/src/file1.txt
... #1 change 5 add on \S+ by \S+ \S+ 'initial '
... ... edit into //import/dev/src/file1.txt#1
... ... edit into //import/dev2/src/file1.txt#2
... ... branch into //import/dev2/targ/file1.txt#1
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)

	result, err = runCmd("p4 print -q //import/dev/targ/file1.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents03\n", result)
}

func TestMultipleModifySameCommit(t *testing.T) {
	// File is modified multiple times in same commit
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
dev01
from :3
M 100644 :2 src/file2.txt
D src/file2.txt

reset refs/heads/dev
commit refs/heads/dev
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
dev02
from :4
M 100644 :2 src/file2.txt
M 100644 :2 src/file2.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file2.txt#1 - add change 5 (text+C)
//import/main/src/file1.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file2.txt
... #1 change 5 add on \S+ by \S+ \S+ 'dev02 '
//import/main/src/file1.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)

	result, err = runCmd("p4 print -q //import/dev/src/file2.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents02\n", result)
}

func TestDirDeleteOverridesModifySameCommit(t *testing.T) {
	// Delete of modify in same commit
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

reset refs/heads/dev
commit refs/heads/dev
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
dev01
from :3
M 100644 :2 src/file2.txt
D src

reset refs/heads/dev
commit refs/heads/dev
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 6
dev02
from :4
M 100644 :2 src/file2.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#1 - delete change 4 (text+C)
//import/dev/src/file2.txt#1 - add change 5 (text+C)
//import/main/src/file1.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file1.txt
... #1 change 4 delete on \S+ by \S+ \S+ 'dev01 '
//import/dev/src/file2.txt
... #1 change 5 add on \S+ by \S+ \S+ 'dev02 '
//import/main/src/file1.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)

}

func TestDeleteOfRenamedDir(t *testing.T) {
	// Dir renamed and target deleted
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt
M 100644 :2 src/file2.txt
M 100644 :2 targ/file3.txt

reset refs/heads/dev
commit refs/heads/dev
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
01devdele
from :3
R src targ
D targ

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src/file1.txt#1 - delete change 4 (text+C)
//import/dev/src/file2.txt#1 - delete change 4 (text+C)
//import/dev/targ/file3.txt#1 - delete change 4 (text+C)
//import/main/src/file1.txt#1 - add change 3 (text+C)
//import/main/src/file2.txt#1 - add change 3 (text+C)
//import/main/targ/file3.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/dev/src/file1.txt
... #1 change 4 delete on \S+ by \S+ \S+ '01devdele '
//import/dev/src/file2.txt
... #1 change 4 delete on \S+ by \S+ \S+ '01devdele '
//import/dev/targ/file3.txt
... #1 change 4 delete on \S+ by \S+ \S+ '01devdele '
//import/main/src/file1.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
//import/main/src/file2.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
//import/main/targ/file3.txt
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestRenameBecomes2Edits(t *testing.T) {
	// Dir renamed and target deleted
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

blob
mark :3
data 11
contents03

reset refs/heads/main
commit refs/heads/main
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 file1.txt

commit refs/heads/main
mark :5
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 10
01renedit
from :4
R file1.txt file2.txt
M 100644 :1 file1.txt
M 100644 :3 file2.txt

`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/file1.txt#2 - edit change 5 (text+C)
//import/main/file2.txt#1 - add change 5 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/main/file1.txt
... #2 change 5 edit on \S+ by \S+ \S+ '01renedit '
... #1 change 4 add on \S+ by \S+ \S+ 'initial '
//import/main/file2.txt
... #1 change 5 add on \S+ by \S+ \S+ '01renedit '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)

	result, err = runCmd("p4 print -q //import/main/file1.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents01\n", result)

	result, err = runCmd("p4 print -q //import/main/file2.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, "contents03\n", result)
}

func TestModifyOfDeletedFile(t *testing.T) {
	// Same file is both deleted and modified in same changelist
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	gitExport := `blob
mark :1
data 11
contents01

blob
mark :2
data 11
contents02

reset refs/heads/main
commit refs/heads/main
mark :3
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 8
initial
M 100644 :1 src/file1.txt

commit refs/heads/main
mark :4
author Robert Cowham <rcowham@perforce.com> 1680784555 +0100
committer Robert Cowham <rcowham@perforce.com> 1680784555 +0100
data 9
01moddel
from :3
D src/file1.txt
M 100644 :2 src/file1.txt
`

	r := runTransferWithDump(t, logger, gitExport, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file1.txt#2 - edit change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	reExpected := `//import/main/src/file1.txt
... #2 change 4 edit on \S+ by \S+ \S+ '01moddel '
... #1 change 3 add on \S+ by \S+ \S+ 'initial '
`
	assert.Regexp(t, reExpected, result)
	compareFilelog(t, reExpected, result)
}

func TestDeleteDir(t *testing.T) {
	// Git rename of a dir consisting of multiple files - expand to constituent parts
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src1 := filepath.Join("src", "file.txt")
	src2 := filepath.Join("src", "file2.txt")
	srcContents1 := "contents\n"
	runCmd("mkdir src")
	writeToFile(src1, srcContents1)
	writeToFile(src2, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git rm -r src")
	runCmd("git add .")
	runCmd("git commit -m deleted")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "D src")
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#1 - add change 2 (text+C)
//import/main/src/file2.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 fstat -Ob //import/main/src/file.txt#1")
	assert.Regexp(t, `headType text\+C`, result)
	assert.Equal(t, nil, err)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/file.txt#2 - delete change 3 (text+C)
//import/main/src/file2.txt#2 - delete change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/src/file.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.NotRegexp(t, `lbrType`, result)
	assert.NotRegexp(t, `lbrFile`, result)
	assert.NotRegexp(t, `(?m)lbrPath`, result)
}

func TestRenameFileDeleteDir(t *testing.T) {
	// Git rename of a dir where a file has been renamed with a modification!
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	d1 := filepath.Join("src", "Animatic", "Animation")
	src1 := filepath.Join(d1, "Bow01.txt")
	srcContents1 := "contents\n"
	err := os.MkdirAll(d1, 0777)
	if err != nil {
		t.Fatalf("Failed to mkdir %v", err)
	}
	writeToFile(src1, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git rm -r src")
	runCmd("git add .")
	runCmd("git commit -m deleted")

	// fast-export with rename detection implemented - to tweak to directory rename
	output, err := runCmd("git fast-export --all -M")
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Output: %s", output)
	lines := strings.Split(output, "\n")
	logger.Debugf("Len: %d", len(lines))
	newLines := lines[:len(lines)-4]
	logger.Debugf("Len new: %d", len(newLines))
	newLines = append(newLines, "R src/Animatic/Animation/Bow01.txt src/AnimBow/Animation/Bow01.txt")
	newLines = append(newLines, "D src/Animatic")
	newLines = append(newLines, "M 100644 :1 src/AnimBow/Animation/Bow01.txt")
	newLines = append(newLines, "")
	newOutput := strings.Join(newLines, "\n")
	logger.Debugf("Changed output: %s", newOutput)

	r := runTransferWithDump(t, logger, newOutput, nil)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/Animatic/Animation/Bow01.txt#1 - add change 2 (text+C)
`, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/src/Animatic/Animation/Bow01.txt#2 - delete change 3 (text+C)
//import/main/src/AnimBow/Animation/Bow01.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/main/src/Animatic/Animation/Bow01.txt
... #2 change 3 delete on \S+ by \S+ \S+ 'deleted '
... #1 change 2 add on \S+ by \S+ \S+ 'initial '
... ... branch into //import/main/src/AnimBow/Animation/Bow01.txt#1
//import/main/src/AnimBow/Animation/Bow01.txt
... #1 change 3 add on \S+ by \S+ \S+ 'deleted '
... ... branch from //import/main/src/Animatic/Animation/Bow01.txt#1
`,
		result)

}

func TestBranch(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	contents1 := "contents\n"
	writeToFile(file1, contents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	contents2 := "contents\nchanged dev"
	writeToFile(file1, contents2)
	runCmd("git add .")
	runCmd("git commit -m 'changed on dev'")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/file1.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/file1.txt#1 - add change 4 (text+C)
//import/main/file1.txt#1 - add change 2 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents1, result)

	result, err = runCmd("p4 print -q //import/dev/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents2, result)

	result, err = runCmd("p4 fstat -Ob //import/dev/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/dev/file1.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.4.gz$`, result)

	result, err = runCmd("p4 filelog //import/dev/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file1.txt`, result)
	assert.Regexp(t, `... #1 change 4 add on .* by .*@git-client`, result)
	assert.Regexp(t, `... ... branch from //import/main/file1.txt#1`, result)

}

// TestTag - ensure tags are ignored as branch names
func TestTag(t *testing.T) {
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	contents1 := "contents\n"
	writeToFile(file1, contents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git tag v1.0")
	contents2 := "contents\nchanged"
	writeToFile(file1, contents2)
	runCmd("git add .")
	runCmd("git commit -m 'changed on main'")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/main/file1.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/main/file1.txt#2 - edit change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 fstat -Ob //import/main/file1.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/file1.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.4.gz$`, result)

}

func TestNode(t *testing.T) {
	// logger := createLogger()
	n := &node.Node{Name: ""}
	n.AddFile("file.txt")
	assert.Equal(t, 1, len(n.Children))
	assert.Equal(t, "file.txt", n.Children[0].Name)
	f := n.GetFiles("")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	f = n.GetFiles("file.txt")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	fname := "src/file2.txt"
	n.AddFile(fname)
	assert.Equal(t, 2, len(n.Children))
	assert.Equal(t, "src", n.Children[1].Name)
	assert.Equal(t, false, n.Children[1].IsFile)
	assert.Equal(t, 1, len(n.Children[1].Children))
	assert.Equal(t, fname, n.Children[1].Children[0].Path)

	f = n.GetFiles("src")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "src/file2.txt", f[0])

	n.AddFile(fname) // IF adding pre-existing file then no change
	assert.Equal(t, 2, len(n.Children))
	assert.Equal(t, "src", n.Children[1].Name)
	assert.Equal(t, false, n.Children[1].IsFile)
	assert.Equal(t, 1, len(n.Children[1].Children))

	fname = "src/file3.txt"
	n.AddFile(fname)
	assert.Equal(t, 2, len(n.Children))
	assert.Equal(t, "src", n.Children[1].Name)
	assert.Equal(t, false, n.Children[1].IsFile)
	assert.Equal(t, 2, len(n.Children[1].Children))
	assert.Equal(t, "file2.txt", n.Children[1].Children[0].Name)
	assert.Equal(t, "file3.txt", n.Children[1].Children[1].Name)

	f = n.GetFiles("src")
	assert.Equal(t, 2, len(f))
	assert.Equal(t, "src/file2.txt", f[0])
	assert.Equal(t, "src/file3.txt", f[1])

	assert.True(t, n.FindFile("src/file2.txt"))
	assert.False(t, n.FindFile("src/file99.txt"))
	assert.False(t, n.FindFile("file99.txt"))

	n.DeleteFile("src/file2.txt")
	f = n.GetFiles("src")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "src/file3.txt", f[0])

	n.DeleteFile("src/file3.txt")
	f = n.GetFiles("src")
	assert.Equal(t, 0, len(f))
}

func TestNodeInsensitive(t *testing.T) {
	// logger := createLogger()
	n := &node.Node{Name: "", CaseInsensitive: true}
	n.AddFile("file.txt")
	assert.Equal(t, 1, len(n.Children))
	assert.Equal(t, "file.txt", n.Children[0].Name)
	f := n.GetFiles("")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	f = n.GetFiles("file.txt")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	n.AddFile("FILE.txt")
	assert.Equal(t, 1, len(n.Children))
	assert.Equal(t, "file.txt", n.Children[0].Name)
	f = n.GetFiles("")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	fname := "src/file2.txt"
	n.AddFile(fname)
	assert.Equal(t, 2, len(n.Children))
	assert.Equal(t, "src", n.Children[1].Name)
	assert.Equal(t, false, n.Children[1].IsFile)
	assert.Equal(t, 1, len(n.Children[1].Children))
	assert.Equal(t, fname, n.Children[1].Children[0].Path)

	f = n.GetFiles("src")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "src/file2.txt", f[0])

	n.AddFile(fname) // IF adding pre-existing file then no change
	assert.Equal(t, 2, len(n.Children))
	assert.Equal(t, "src", n.Children[1].Name)
	assert.Equal(t, false, n.Children[1].IsFile)
	assert.Equal(t, 1, len(n.Children[1].Children))

	fname = "src/file3.txt"
	n.AddFile(fname)
	assert.Equal(t, 2, len(n.Children))
	assert.Equal(t, "src", n.Children[1].Name)
	assert.Equal(t, false, n.Children[1].IsFile)
	assert.Equal(t, 2, len(n.Children[1].Children))
	assert.Equal(t, "file2.txt", n.Children[1].Children[0].Name)
	assert.Equal(t, "file3.txt", n.Children[1].Children[1].Name)

	f = n.GetFiles("src")
	assert.Equal(t, 2, len(f))
	assert.Equal(t, "src/file2.txt", f[0])
	assert.Equal(t, "src/file3.txt", f[1])

	assert.True(t, n.FindFile("src/file2.txt"))
	assert.False(t, n.FindFile("src/file99.txt"))
	assert.False(t, n.FindFile("file99.txt"))

	n.DeleteFile("src/file2.txt")
	f = n.GetFiles("src")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "src/file3.txt", f[0])

	n.DeleteFile("src/file3.txt")
	f = n.GetFiles("src")
	assert.Equal(t, 0, len(f))
}

func TestBigNode(t *testing.T) {
	n := &node.Node{Name: ""}
	files := `Env/Assets/ArtEnv/Cookies/cookie.png
Env/Assets/ArtEnv/Cookies/cookie.png.meta
Env/Assets/Art/Structure/Universal.meta
Env/Assets/Art/Structure/Universal/Bunker.meta`

	for _, f := range strings.Split(files, "\n") {
		n.AddFile(f)
	}
	assert.True(t, n.FindFile("Env/Assets/Art/Structure/Universal/Bunker.meta"))
	assert.True(t, n.FindFile("Env/Assets/Art/Structure/Universal.meta"))
	assert.False(t, n.FindFile("src/file99.txt"))
	assert.False(t, n.FindFile("file99.txt"))
	f := n.GetFiles("Env/Assets/Art/Structure")
	assert.Equal(t, 2, len(f))
	assert.Equal(t, "Env/Assets/Art/Structure/Universal.meta", f[0])
	assert.Equal(t, "Env/Assets/Art/Structure/Universal/Bunker.meta", f[1])

	f = n.GetFiles("")
	assert.Equal(t, 4, len(f))
	assert.Equal(t, "Env/Assets/ArtEnv/Cookies/cookie.png", f[0])
	assert.Equal(t, "Env/Assets/ArtEnv/Cookies/cookie.png.meta", f[1])

}

func TestBigNode2(t *testing.T) {
	n := &node.Node{Name: ""}
	files := `Games/Content/Heroes/Weapons/Weapons/X.txt
Games/Content/Heroes/Weapons/Others/A.uasset
Games/Content/Heroes/Weapons/Others/B.uasset`

	for _, f := range strings.Split(files, "\n") {
		n.AddFile(f)
	}
	assert.True(t, n.FindFile("Games/Content/Heroes/Weapons/Others/A.uasset"))
	f := n.GetFiles("")
	assert.Equal(t, 3, len(f))
	f = n.GetFiles("Games/Content")
	assert.Equal(t, 3, len(f))

	f = n.GetFiles("Games/Content/Heroes/Weapons/Weapons")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "Games/Content/Heroes/Weapons/Weapons/X.txt", f[0])
}

// func TestBranchMerge(t *testing.T) {
// 	logger := createLogger()

// 	d := createGitRepo(t)
// 	os.Chdir(d)
// 	logger.Debugf("Git repo: %s", d)

// 	file1 := "file1.txt"
// 	contents1 := "contents\n"
// 	writeToFile(file1, contents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m initial")
// 	runCmd("git switch -c dev")
// 	contents2 := "contents\nchanged dev"
// 	writeToFile(file1, contents2)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'changed on dev'")
// 	runCmd("git switch main")
// 	runCmd("git merge dev")

// 	r := runTransfer(t, logger)
// 	logger.Debugf("Server root: %s", r)

// 	result, err := runCmd("p4 files //...@2")
// 	assert.Equal(t, nil, err)
// 	assert.Equal(t, "//import/main/file1.txt#1 - add change 2 (text+C)\n", result)

// 	result, err = runCmd("p4 files //...")
// 	assert.Equal(t, nil, err)
// 	assert.Equal(t, `//import/dev/file1.txt#1 - add change 4 (text+C)
// //import/main/file1.txt#1 - add change 2 (text+C)
// `,
// 		result)

// 	result, err = runCmd("p4 verify -qu //...")
// 	assert.Equal(t, "", result)
// 	assert.Equal(t, "<nil>", fmt.Sprint(err))

// 	result, err = runCmd("p4 fstat -Ob //import/dev/file1.txt#1")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `headType text\+C`, result)
// 	assert.Regexp(t, `lbrType text\+C`, result)
// 	assert.Regexp(t, `lbrFile //import/dev/file1.txt`, result)
// 	assert.Regexp(t, `(?m)lbrPath .*/1.4.gz$`, result)

// 	result, err = runCmd("p4 filelog //import/dev/file1.txt#1")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `//import/dev/file1.txt`, result)
// 	assert.Regexp(t, `... #1 change 4 add on .* by .*@git-client`, result)
// 	// assert.Regexp(t, `... #1 change 4 add on .* by .*@git-client (text+C) 'changed on dev '`, result)
// 	assert.Regexp(t, `... ... branch from //import/main/file1.txt#1`, result)

// }

func TestBranch2(t *testing.T) {
	// Multiple branches
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	file2 := "file2.txt"
	contents1 := ""
	writeToFile(file1, contents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	writeToFile(file2, contents1)
	runCmd("git add .")
	runCmd("git commit -m 'changed on dev'")
	runCmd("git switch -c dev2")
	writeToFile(file2, ".")
	runCmd("git add .")
	runCmd("git commit -m 'changed on dev2'")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/file2.txt#1 - add change 3 (text+C)
//import/dev2/file2.txt#1 - add change 5 (text+C)
//import/main/file1.txt#1 - add change 2 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents1, result)

	result, err = runCmd("p4 print -q //import/dev/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents1, result)

	result, err = runCmd("p4 fstat -Ob //import/dev/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/file1.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)

	result, err = runCmd("p4 filelog //import/dev/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file2.txt`, result)
	assert.Regexp(t, `... #1 change 3 add on .* by .*@git-client`, result)
	// assert.Regexp(t, `... #1 change 4 add on .* by .*@git-client (text+C) 'changed on dev '`, result)
	assert.Regexp(t, `... ... edit into //import/dev2/file2.txt#1`, result)

}

func TestBranchRename(t *testing.T) {
	// Branch and add new file which is then renamed
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	file2 := "file2.txt"
	file3 := "file3.txt"
	contents1 := "1"
	contents2 := "new"
	writeToFile(file1, contents1)
	writeToFile(file2, contents2)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	appendToFile(file1, contents1)
	runCmd("git add .")
	runCmd("git commit -m 'first on dev'")
	runCmd(fmt.Sprintf("mv %s %s", file2, file3))
	runCmd("git add .")
	runCmd("git commit -m 'renamed on dev'")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/file1.txt#1 - add change 5 (text+C)
//import/dev/file2.txt#1 - delete change 6 (text+C)
//import/dev/file3.txt#1 - add change 6 (text+C)
//import/main/file1.txt#1 - add change 3 (text+C)
//import/main/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents1, result)

	result, err = runCmd("p4 print -q //import/dev/file3.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents2, result)

	result, err = runCmd("p4 fstat -Ob //import/dev/file3.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/main/file2.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.3.gz$`, result)

	result, err = runCmd("p4 filelog //import/dev/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file2.txt`, result)
	assert.Regexp(t, `... #1 change 6 delete on .* by .*@git-client.*
... ... delete from //import/main/file2.txt#1`, result)

	result, err = runCmd("p4 filelog //import/dev/file3.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file3.txt`, result)
	assert.Regexp(t, `... #1 change 6 add on .* by .*@git-client.*
... ... branch from //import/main/file2.txt#1`, result)

}

func TestBranchMerge(t *testing.T) {
	// Merge branches
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	file2 := "file2.txt"
	contents1 := "Contents\n"
	contents2 := "Test content2\n"
	writeToFile(file1, contents1)
	runCmd("git add .")
	runCmd("git commit -m \"1: first change\"")
	runCmd("git checkout -b branch1")
	runCmd("git add .")
	bcontents1 := "branch change\n"
	appendToFile(file1, bcontents1)
	runCmd("git add .")
	runCmd("git commit -m \"2: branch edit change\"")
	runCmd("git checkout main")
	writeToFile(file2, contents2)
	runCmd("git add .")
	runCmd("git commit -m \"3: new file on main\"")
	runCmd("git merge --no-edit branch1")
	runCmd("git commit -m \"4: merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/branch1/file1.txt#1 - add change 6 (text+C)
//import/main/file1.txt#2 - edit change 7 (text+C)
//import/main/file2.txt#1 - add change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//import/branch1/file1.txt
... #1 change 6 add on .* by .*@git-client \(text\+C\).*
... ... edit into //import/main/file1.txt#2
... ... branch from //import/main/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
... #2 change 7 edit on .* by .*@git-client \(text\+C\).*
... ... branch from //import/branch1/file1.txt#1
... #1 change 2 add on .* by .*@git-client \(text\+C\).*
... ... edit into //import/branch1/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
... #1 change 4 add on .* by .*@git-client \(text\+C\).*`, result)

	result, err = runCmd("p4 print -q //import/main/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents2, result)

	bcontents2 := fmt.Sprintf("%s%s", contents1, bcontents1)
	result, err = runCmd("p4 print -q //import/branch1/file1.txt")
	assert.Equal(t, nil, err)
	assert.Equal(t, bcontents2, result)

	result, err = runCmd("p4 print -q //import/main/file1.txt#2")
	assert.Equal(t, nil, err)
	assert.Equal(t, bcontents2, result)

	result, err = runCmd("p4 fstat -Ob //import/main/file1.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/branch1/file1.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.6.gz$`, result)

}

func TestBranchDelete(t *testing.T) {
	// Merge branches with deleted files
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	file2 := "file2.txt"
	contents1 := "Contents\n"
	contents2 := "Test content2\n"
	writeToFile(file1, contents1)
	writeToFile(file2, contents2)
	runCmd("git add .")
	runCmd("git commit -m \"1: first change\"")
	runCmd("git checkout -b branch1")
	runCmd("git add .")
	bcontents1 := "branch change\n"
	appendToFile(file1, bcontents1)
	runCmd("git add .")
	runCmd("git commit -m \"2: branch edit change\"")
	runCmd("git rm " + file1)
	runCmd("git add .")
	runCmd("git commit -m \"3: delete file\"")
	runCmd("git checkout main")
	runCmd("git merge --no-ff branch1")
	runCmd("git commit -m \"4: merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/branch1/file1.txt#2 - delete change 6 (text+C)
//import/main/file1.txt#2 - delete change 7 (text+C)
//import/main/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//import/branch1/file1.txt
... #2 change 6 delete on .* by .*@git-client \(text\+C\).*
... ... delete into //import/main/file1.txt#2
... #1 change 5 add on .* by .*@git-client \(text\+C\).*
... ... branch from //import/main/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
... #2 change 7 delete on .* by .*@git-client \(text\+C\).*
... ... delete from //import/branch1/file1.txt#2
... #1 change 3 add on .* by .*@git-client \(text\+C\).*
... ... edit into //import/branch1/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
... #1 change 3 add on .* by .*@git-client \(text\+C\).*`, result)

	// result, err = runCmd("p4 print -q //import/main/file2.txt#1")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, contents2, result)

	// bcontents2 := fmt.Sprintf("%s%s", contents1, bcontents1)
	// result, err = runCmd("p4 print -q //import/branch1/file1.txt")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, bcontents2, result)

	// result, err = runCmd("p4 print -q //import/main/file1.txt#2")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, bcontents2, result)

	// result, err = runCmd("p4 fstat -Ob //import/main/file1.txt#2")
	// assert.Equal(t, nil, err)
	// assert.Regexp(t, `headType text\+C`, result)
	// assert.Regexp(t, `lbrType text\+C`, result)
	// assert.Regexp(t, `lbrFile //import/branch1/file1.txt`, result)
	// assert.Regexp(t, `(?m)lbrPath .*/1.6.gz$`, result)

}

func TestBranchDeleteFiletype(t *testing.T) {
	// Merge branches with deleted files
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	file2 := "file2.txt"
	contents1 := "Contents\n"
	contents2 := "Test content2\n"
	writeToFile(file1, contents1)
	writeToFile(file2, contents2)
	runCmd("git add .")
	runCmd("git commit -m \"1: first change\"")
	runCmd("git checkout -b branch1")
	runCmd("git add .")
	bcontents1 := "branch change\n"
	appendToFile(file1, bcontents1)
	runCmd("git add .")
	runCmd("git commit -m \"2: branch edit change\"")
	runCmd("git rm " + file1)
	runCmd("git add .")
	runCmd("git commit -m \"3: delete file\"")
	runCmd("git checkout main")
	runCmd("git merge --no-ff branch1")
	runCmd("git commit -m \"4: merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	c := &config.Config{
		ImportDepot: "import", DefaultBranch: "main",
		ReTypeMaps: make([]config.RegexpTypeMap, 0),
	}
	rePath := regexp.MustCompile("//.*.txt$") // Go version of typemap
	c.ReTypeMaps = append(c.ReTypeMaps, config.RegexpTypeMap{Filetype: journal.Binary, RePath: rePath})

	opts := &GitParserOptions{config: c, convertCRLF: true}
	r := runTransferOpts(t, logger, opts)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/branch1/file1.txt#2 - delete change 6 (binary)
//import/main/file1.txt#2 - delete change 7 (binary)
//import/main/file2.txt#1 - add change 3 (binary)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//import/branch1/file1.txt
... #2 change 6 delete on .* by .*@git-client \(binary\).*
... ... delete into //import/main/file1.txt#2
... #1 change 5 add on .* by .*@git-client \(binary\).*
... ... branch from //import/main/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
... #2 change 7 delete on .* by .*@git-client \(binary\).*
... ... delete from //import/branch1/file1.txt#2
... #1 change 3 add on .* by .*@git-client \(binary\).*
... ... edit into //import/branch1/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
... #1 change 3 add on .* by .*@git-client \(binary\).*`, result)

}

func TestBranchDelete2(t *testing.T) {
	// Merge branches with deleted files
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.txt"
	file2 := "file2.txt"
	contents1 := "Contents\n"
	contents2 := "Test content2\n"
	writeToFile(file1, contents1)
	writeToFile(file2, contents2)
	runCmd("git add .")
	runCmd("git commit -m \"1: first change\"")
	runCmd("git checkout -b branch1")
	runCmd("git rm " + file1)
	runCmd("git add .")
	runCmd("git commit -m \"2: branch delete change\"")
	runCmd("git checkout main")
	runCmd("git merge --no-ff branch1")
	runCmd("git commit -m \"4: merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/branch1/file1.txt#1 - delete change 4 (text+C)
//import/main/file1.txt#2 - delete change 5 (text+C)
//import/main/file2.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//import/branch1/file1.txt
... #1 change 4 delete on .* by .*@git-client \(text\+C\).*
... ... delete into //import/main/file1.txt#2`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
... #2 change 5 delete on .* by .*@git-client \(text\+C\).*
... ... delete from //import/branch1/file1.txt#1
... #1 change 3 add on .* by .*@git-client \(text\+C\).*`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
... #1 change 3 add on .* by .*@git-client \(text\+C\).*`, result)

}

func TestBranchMergeCompressed(t *testing.T) {
	// Merge branches with a file already compressed
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	file1 := "file1.png"
	file2 := "file2.png"
	file3 := "file3.png"
	contents1 := "Contents\n"
	contents2 := "Test content2\n"
	contents3 := "Test content3\n"
	writeToFile(file1, contents1)
	runCmd(fmt.Sprintf("gzip %s", file1))
	runCmd(fmt.Sprintf("mv %s.gz %s", file1, file1))
	runCmd("git add .")
	runCmd("git commit -m \"1: first change\"")
	runCmd("git checkout -b branch1")
	runCmd("git add .")
	bcontents1 := "branch change\n"
	writeToFile(file1, contents1+bcontents1)
	runCmd(fmt.Sprintf("gzip %s", file1))
	runCmd(fmt.Sprintf("mv %s.gz %s", file1, file1))
	runCmd("git add .")
	runCmd("git commit -m \"2: branch edit change\"")
	writeToFile(file3, contents3)
	runCmd(fmt.Sprintf("gzip %s", file3))
	runCmd(fmt.Sprintf("mv %s.gz %s", file3, file3))
	runCmd("git add .")
	runCmd("git commit -m \"3: branch add file\"")
	runCmd("git checkout main")
	writeToFile(file2, contents2)
	runCmd(fmt.Sprintf("gzip %s", file2))
	runCmd(fmt.Sprintf("mv %s.gz %s", file2, file2))
	runCmd("git add .")
	runCmd("git commit -m \"4: new file on main\"")
	runCmd("git merge --no-edit branch1")
	runCmd("git commit -m \"5: merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/branch1/file1.png#1 - add change 6 (binary+F)
//import/branch1/file3.png#1 - add change 8 (binary+F)
//import/main/file1.png#2 - edit change 9 (binary+F)
//import/main/file2.png#1 - add change 4 (binary+F)
//import/main/file3.png#1 - add change 9 (binary+F)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//import/branch1/file1.png
... #1 change 6 add on .* by .*@git-client \(binary\+F\).*
... ... edit into //import/main/file1.png#2
... ... branch from //import/main/file1.png#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.png
... #2 change 9 edit on .* by .*@git-client \(binary\+F\).*
... ... branch from //import/branch1/file1.png#1
... #1 change 2 add on .* by .*@git-client \(binary\+F\).*
... ... edit into //import/branch1/file1.png#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.png
... #1 change 4 add on .* by .*@git-client \(binary\+F\).*`, result)

	result, err = runCmd("p4 print -q //import/main/file2.png#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, contents2, unzipBuf(result))

	bcontents2 := fmt.Sprintf("%s%s", contents1, bcontents1)
	result, err = runCmd("p4 print -q //import/branch1/file1.png")
	assert.Equal(t, nil, err)
	assert.Equal(t, bcontents2, unzipBuf(result))

	result, err = runCmd("p4 print -q //import/main/file1.png#2")
	assert.Equal(t, nil, err)
	assert.Equal(t, bcontents2, unzipBuf(result))

	result, err = runCmd("p4 fstat -Ob //import/main/file1.png#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType binary\+F`, result)
	assert.Regexp(t, `lbrType binary\+F`, result)
	assert.Regexp(t, `lbrFile //import/branch1/file1.png`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.6$`, result)

}

func TestBranchMergeRename(t *testing.T) {
	// Merge branches with where a file has been renamed on the branch
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m renamed")
	runCmd("git switch main")
	runCmd("git merge --no-ff dev")
	runCmd("git commit -m \"merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	r := runTransfer(t, logger)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//import/dev/src.txt#1 - delete change 3 (text+C)
//import/dev/targ.txt#1 - add change 3 (text+C)
//import/main/src.txt#2 - delete change 4 (text+C)
//import/main/targ.txt#1 - add change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//import/dev/src.txt
... #1 change 3 delete on .* by .*@git-client \(text\+C\).*
... ... delete into //import/main/src.txt#2
... ... delete from //import/main/src.txt#1`, result)

	assert.Regexp(t, `(?m)//import/dev/targ.txt
... #1 change 3 add on .* by .*@git-client \(text\+C\).*
... ... branch from //import/main/src.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/src.txt
... #2 change 4 delete on .* by .*@git-client \(text\+C\).*
... ... delete from //import/dev/src.txt#1
... #1 change 2 add on .* by .*@git-client \(text\+C\).*
... ... delete into //import/dev/src.txt#1
... ... branch into //import/dev/targ.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/targ.txt
... #1 change 4 add on .* by .*@git-client \(text\+C\).*
... ... branch from //import/dev/targ.txt#1`, result)

}

func TestBranchNameMapping(t *testing.T) {
	// Merge branches with where a file has been renamed on the branch
	logger := createLogger()
	logger.Debugf("======== Test: %s", t.Name())

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)

	src := "src.txt"
	targ := "targ.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")
	runCmd("git switch -c dev")
	runCmd(fmt.Sprintf("mv %s %s", src, targ))
	runCmd("git add .")
	runCmd("git commit -m renamed")
	runCmd("git switch main")
	runCmd("git merge --no-ff dev")
	runCmd("git commit -m \"merged change\"")
	runCmd("git log --graph --abbrev-commit --oneline")

	mainMap := config.BranchMapping{Name: "mai.*", Prefix: "trunk/"}
	devMap := config.BranchMapping{Name: "dev.*", Prefix: "branches/"}
	opts := &GitParserOptions{
		config: &config.Config{
			ImportDepot:    "testimport",
			ImportPath:     "subdir",
			BranchMappings: [](config.BranchMapping){mainMap, devMap},
		},
	}
	r := runTransferOpts(t, logger, opts)
	logger.Debugf("Server root: %s", r)

	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, `//testimport/subdir/branches/dev/src.txt#1 - delete change 3 (text+C)
//testimport/subdir/branches/dev/targ.txt#1 - add change 3 (text+C)
//testimport/subdir/trunk/main/src.txt#2 - delete change 4 (text+C)
//testimport/subdir/trunk/main/targ.txt#1 - add change 4 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	result, err = runCmd("p4 filelog //testimport/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `(?m)//testimport/subdir/branches/dev/src.txt
... #1 change 3 delete on .* by .*@git-client \(text\+C\).*
... ... delete into //testimport/subdir/trunk/main/src.txt#2
... ... delete from //testimport/subdir/trunk/main/src.txt#1`, result)

	assert.Regexp(t, `(?m)//testimport/subdir/branches/dev/targ.txt
... #1 change 3 add on .* by .*@git-client \(text\+C\).*
... ... branch from //testimport/subdir/trunk/main/src.txt#1`, result)

	assert.Regexp(t, `(?m)//testimport/subdir/trunk/main/src.txt
... #2 change 4 delete on .* by .*@git-client \(text\+C\).*
... ... delete from //testimport/subdir/branches/dev/src.txt#1
... #1 change 2 add on .* by .*@git-client \(text\+C\).*
... ... delete into //testimport/subdir/branches/dev/src.txt#1
... ... branch into //testimport/subdir/branches/dev/targ.txt#1`, result)

	assert.Regexp(t, `(?m)//testimport/subdir/trunk/main/targ.txt
... #1 change 4 add on .* by .*@git-client \(text\+C\).*
... ... branch from //testimport/subdir/branches/dev/targ.txt#1`, result)

}
