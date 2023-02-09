// Tests for gitp4transfer

package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcowham/gitp4transfer/config"
	"github.com/rcowham/gitp4transfer/journal"
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

func writeToFile(fname, contents string) {
	f, err := os.Create(fname)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprint(f, contents)
	if err != nil {
		panic(err)
	}
}

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
	j.WriteHeader(opts.config.ImportDepot)

	for _, c := range commits {
		j.WriteChange(c.commit.Mark, user, c.commit.Msg, int(c.commit.Author.Time.Unix()))
		for _, f := range c.files {
			f.CreateArchiveFile(nil, p4t.serverRoot, g.blobFileMatcher, c.commit.Mark)
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
	j.WriteHeader(opts.config.ImportDepot)
	c = commits[0]
	j.WriteChange(c.commit.Mark, defaultP4user, c.commit.Msg, int(c.commit.Author.Time.Unix()))
	f = c.files[0]
	j.WriteRev(f.p4.depotFile, f.p4.rev, f.p4.p4action, f.fileType, c.commit.Mark,
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
	f.CreateArchiveFile(nil, p4t.serverRoot, g.blobFileMatcher, c.commit.Mark)
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
	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .* \(text\+C\)`, result)

	result, err = runCmd("p4 filelog -i //import/dev/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 5 delete on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. delete from //import/main/src.txt#1`, result)

	result, err = runCmd("p4 filelog -i //import/dev/targ.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 2 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. delete into //import/dev/src.txt#1`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/dev/targ.txt#1`, result)

	result, err = runCmd("p4 filelog -i //import/main/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 2 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. delete into //import/dev/src.txt#1`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/dev/targ.txt#1`, result)
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
// 	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .* \(text\+C\)`, result)

// 	result, err = runCmd("p4 filelog -i //import/dev/file2.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `\.\.\. #1 change 5 delete on .* by .* \(text\+C\)`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. delete from //import/main/file2.txt#1`, result)

// 	result, err = runCmd("p4 filelog -i //import/dev/file3.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `\.\.\. #1 change 2 add on .* by .* \(text\+C\)`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. delete into //import/dev/file2.txt#1`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/dev/file3.txt#1`, result)

// 	result, err = runCmd("p4 filelog -i //import/main/file2.txt")
// 	assert.Equal(t, nil, err)
// 	assert.Regexp(t, `\.\.\. #1 change 2 add on .* by .* \(text\+C\)`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. delete into //import/dev/file2.txt#1`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/dev/file3.txt#1`, result)
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
	assert.Regexp(t, `//import/main/src.txt`, result)
	assert.Regexp(t, `\.\.\. #3 change 4 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/targ.txt#1,#2`, result)
	assert.Regexp(t, `\.\.\. #2 change 3 delete on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/main/targ.txt#1`, result)
	assert.Regexp(t, `\.\.\. #1 change 2 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/targ.txt#1,#2`, result)

	result, err = runCmd("p4 filelog //import/main/targ.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/main/targ.txt`, result)
	assert.Regexp(t, `\.\.\. #2 change 4 delete on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/main/src.txt#1,#3`, result)
	assert.Regexp(t, `\.\.\. #1 change 3 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/src.txt#1,#2`, result)

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

	result, err = runCmd("p4 filelog //import/dev/file1.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file1.txt`, result)
	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .* \(text\+C\)`, result)

	result, err = runCmd("p4 filelog //import/dev/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 6 delete on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. delete from //import/main/src.txt#1`, result)

	result, err = runCmd("p4 filelog //import/dev/targ.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 6 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/src.txt#1`, result)

	result, err = runCmd("p4 filelog //import/main/src.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 2 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. delete into //import/dev/src.txt#1`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch into //import/dev/targ.txt#1`, result)

	result, err = runCmd("p4 filelog //import/main/targ.txt")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `\.\.\. #1 change 7 add on .* by .* \(text\+C\)`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/dev/targ.txt#1,#2`, result)
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
	assert.Equal(t, `//import/main/src/Animatic/Animation/Bow01.txt#3 - delete change 3 (text+C)
//import/main/src/AnimBow/Animation/Bow01.txt#1 - add change 3 (text+C)
`,
		result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "", result)
	assert.Equal(t, "<nil>", fmt.Sprint(err))

	// result, err = runCmd("p4 fstat -Ob //import/main/src/file.txt#2")
	// assert.Equal(t, nil, err)
	// assert.Regexp(t, `headType text\+C`, result)
	// assert.NotRegexp(t, `lbrType`, result)
	// assert.NotRegexp(t, `lbrFile`, result)
	// assert.NotRegexp(t, `(?m)lbrPath`, result)
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
	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .*@git-client`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/file1.txt#1`, result)

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
	n := &Node{name: ""}
	n.addFile("file.txt")
	assert.Equal(t, 1, len(n.children))
	assert.Equal(t, "file.txt", n.children[0].name)
	f := n.getFiles("")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	f = n.getFiles("file.txt")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "file.txt", f[0])

	fname := "src/file2.txt"
	n.addFile(fname)
	assert.Equal(t, 2, len(n.children))
	assert.Equal(t, "src", n.children[1].name)
	assert.Equal(t, false, n.children[1].isFile)
	assert.Equal(t, 1, len(n.children[1].children))
	assert.Equal(t, fname, n.children[1].children[0].path)

	f = n.getFiles("src")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "src/file2.txt", f[0])

	n.addFile(fname) // IF adding pre-existing file then no change
	assert.Equal(t, 2, len(n.children))
	assert.Equal(t, "src", n.children[1].name)
	assert.Equal(t, false, n.children[1].isFile)
	assert.Equal(t, 1, len(n.children[1].children))

	fname = "src/file3.txt"
	n.addFile(fname)
	assert.Equal(t, 2, len(n.children))
	assert.Equal(t, "src", n.children[1].name)
	assert.Equal(t, false, n.children[1].isFile)
	assert.Equal(t, 2, len(n.children[1].children))
	assert.Equal(t, "file2.txt", n.children[1].children[0].name)
	assert.Equal(t, "file3.txt", n.children[1].children[1].name)

	f = n.getFiles("src")
	assert.Equal(t, 2, len(f))
	assert.Equal(t, "src/file2.txt", f[0])
	assert.Equal(t, "src/file3.txt", f[1])

	assert.True(t, n.findFile("src/file2.txt"))
	assert.False(t, n.findFile("src/file99.txt"))
	assert.False(t, n.findFile("file99.txt"))

	n.deleteFile("src/file2.txt")
	f = n.getFiles("src")
	assert.Equal(t, 1, len(f))
	assert.Equal(t, "src/file3.txt", f[0])

	n.deleteFile("src/file3.txt")
	f = n.getFiles("src")
	assert.Equal(t, 0, len(f))
}

func TestBigNode(t *testing.T) {
	n := &Node{name: ""}
	files := `Env/Assets/ArtEnv/Cookies/cookie.png
Env/Assets/ArtEnv/Cookies/cookie.png.meta
Env/Assets/Art/Structure/Universal.meta
Env/Assets/Art/Structure/Universal/Bunker.meta`

	for _, f := range strings.Split(files, "\n") {
		n.addFile(f)
	}
	assert.True(t, n.findFile("Env/Assets/Art/Structure/Universal/Bunker.meta"))
	assert.True(t, n.findFile("Env/Assets/Art/Structure/Universal.meta"))
	assert.False(t, n.findFile("src/file99.txt"))
	assert.False(t, n.findFile("file99.txt"))
	f := n.getFiles("Env/Assets/Art/Structure")
	assert.Equal(t, 2, len(f))
	assert.Equal(t, "Env/Assets/Art/Structure/Universal.meta", f[0])
	assert.Equal(t, "Env/Assets/Art/Structure/Universal/Bunker.meta", f[1])

	f = n.getFiles("")
	assert.Equal(t, 4, len(f))
	assert.Equal(t, "Env/Assets/ArtEnv/Cookies/cookie.png", f[0])
	assert.Equal(t, "Env/Assets/ArtEnv/Cookies/cookie.png.meta", f[1])

}

func TestBigNode2(t *testing.T) {
	n := &Node{name: ""}
	files := `Games/Content/Heroes/Weapons/Weapons/X.txt
Games/Content/Heroes/Weapons/Others/A.uasset
Games/Content/Heroes/Weapons/Others/B.uasset`

	for _, f := range strings.Split(files, "\n") {
		n.addFile(f)
	}
	assert.True(t, n.findFile("Games/Content/Heroes/Weapons/Others/A.uasset"))
	f := n.getFiles("")
	assert.Equal(t, 3, len(f))
	f = n.getFiles("Games/Content")
	assert.Equal(t, 3, len(f))

	f = n.getFiles("Games/Content/Heroes/Weapons/Weapons")
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
// 	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .*@git-client`, result)
// 	// assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .*@git-client (text+C) 'changed on dev '`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/file1.txt#1`, result)

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
	assert.Regexp(t, `\.\.\. #1 change 3 add on .* by .*@git-client`, result)
	// assert.Regexp(t, `\.\.\. #1 change 4 add on .* by .*@git-client (text+C) 'changed on dev '`, result)
	assert.Regexp(t, `\.\.\. \.\.\. edit into //import/dev2/file2.txt#1`, result)

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
	assert.Regexp(t, `\.\.\. #1 change 6 delete on .* by .*@git-client.*
\.\.\. \.\.\. delete from //import/main/file2.txt#1`, result)

	result, err = runCmd("p4 filelog //import/dev/file3.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file3.txt`, result)
	assert.Regexp(t, `\.\.\. #1 change 6 add on .* by .*@git-client.*
\.\.\. \.\.\. branch from //import/main/file2.txt#1`, result)

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
\.\.\. #1 change 6 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. edit into //import/main/file1.txt#2
\.\.\. \.\.\. branch from //import/main/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
\.\.\. #2 change 7 edit on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //import/branch1/file1.txt#1
\.\.\. #1 change 2 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. edit into //import/branch1/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
\.\.\. #1 change 4 add on .* by .*@git-client \(text\+C\).*`, result)

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
\.\.\. #2 change 6 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete into //import/main/file1.txt#2
\.\.\. #1 change 5 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //import/main/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
\.\.\. #2 change 7 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete from //import/branch1/file1.txt#2
\.\.\. #1 change 3 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. edit into //import/branch1/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
\.\.\. #1 change 3 add on .* by .*@git-client \(text\+C\).*`, result)

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
\.\.\. #1 change 4 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete into //import/main/file1.txt#2`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
\.\.\. #2 change 5 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete from //import/branch1/file1.txt#1
\.\.\. #1 change 3 add on .* by .*@git-client \(text\+C\).*`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
\.\.\. #1 change 3 add on .* by .*@git-client \(text\+C\).*`, result)

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
\.\.\. #1 change 6 add on .* by .*@git-client \(binary\+F\).*
\.\.\. \.\.\. edit into //import/main/file1.png#2
\.\.\. \.\.\. branch from //import/main/file1.png#1`, result)

	assert.Regexp(t, `(?m)//import/main/file1.png
\.\.\. #2 change 9 edit on .* by .*@git-client \(binary\+F\).*
\.\.\. \.\.\. branch from //import/branch1/file1.png#1
\.\.\. #1 change 2 add on .* by .*@git-client \(binary\+F\).*
\.\.\. \.\.\. edit into //import/branch1/file1.png#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.png
\.\.\. #1 change 4 add on .* by .*@git-client \(binary\+F\).*`, result)

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
\.\.\. #1 change 3 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete from //import/main/src.txt#1
\.\.\. \.\.\. delete into //import/main/src.txt#1,#2`, result)

	assert.Regexp(t, `(?m)//import/dev/targ.txt
\.\.\. #1 change 3 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //import/main/src.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/src.txt
\.\.\. #2 change 4 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete from //import/dev/src.txt#1
\.\.\. #1 change 2 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete into //import/dev/src.txt#1
\.\.\. \.\.\. branch into //import/dev/targ.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/targ.txt
\.\.\. #1 change 4 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //import/dev/targ.txt#1,#2`, result)

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
\.\.\. #1 change 3 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete from //testimport/subdir/trunk/main/src.txt#1
\.\.\. \.\.\. delete into //testimport/subdir/trunk/main/src.txt#1,#2`, result)

	assert.Regexp(t, `(?m)//testimport/subdir/branches/dev/targ.txt
\.\.\. #1 change 3 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //testimport/subdir/trunk/main/src.txt#1`, result)

	assert.Regexp(t, `(?m)//testimport/subdir/trunk/main/src.txt
\.\.\. #2 change 4 delete on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete from //testimport/subdir/branches/dev/src.txt#1
\.\.\. #1 change 2 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. delete into //testimport/subdir/branches/dev/src.txt#1
\.\.\. \.\.\. branch into //testimport/subdir/branches/dev/targ.txt#1`, result)

	assert.Regexp(t, `(?m)//testimport/subdir/trunk/main/targ.txt
\.\.\. #1 change 4 add on .* by .*@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //testimport/subdir/branches/dev/targ.txt#1,#2`, result)

}
