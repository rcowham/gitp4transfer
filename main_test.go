// Tests for gitp4transfer

package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func createP4DRepo(t *testing.T) string {
	d := t.TempDir()
	os.Chdir(d)
	return d
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

func (p4t *P4Test) cleanupTestTree() {
	os.Chdir(p4t.startDir)
	err := os.RemoveAll(p4t.testRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to remove %s: %v", p4t.startDir, err)
	}
}

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

func writeToTempFile(contents string) string {
	f, err := os.CreateTemp("", "*.txt")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprint(f, contents)
	if err != nil {
		fmt.Println(err)
	}
	return f.Name()
}

// func TestParseBasic(t *testing.T) {
// 	input := `blob
// mark :1
// data 5
// test

// reset refs/heads/main
// commit refs/heads/main
// mark :2
// author Robert Cowham <rcowham@perforce.com> 1644399073 +0000
// committer Robert Cowham <rcowham@perforce.com> 1644399073 +0000
// data 5
// test
// M 100644 :1 test.txt

// `

// 	logger := logrus.New()
// 	logger.Level = logrus.InfoLevel
// 	if *&debug {
// 		logger.Level = logrus.DebugLevel
// 	}
// 	g := NewGitP4Transfer(logger)
// 	g.testInput = input

// 	commits, files := g.RunGetCommits("")

// 	assert.Equal(t, 1, len(commits))
// 	for k, v := range commits {
// 		switch k {
// 		case 2:
// 			assert.Equal(t, 2, v.commit.Mark)
// 			assert.Equal(t, 1, len(v.files))
// 			assert.Equal(t, "Robert Cowham", v.commit.Committer.Name)
// 			assert.Equal(t, "rcowham@perforce.com", v.commit.Author.Email)
// 			assert.Equal(t, "refs/heads/main", v.commit.Ref)
// 		default:
// 			assert.Fail(t, "Unexpected range")
// 		}
// 	}
// 	assert.Equal(t, 1, len(files))
// 	for k, v := range files {
// 		switch k {
// 		case 1:
// 			assert.Equal(t, "test.txt", v.name)
// 			assert.Equal(t, "test\n", v.blob.Data)
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}

// }

// func TestSimpleBranch(t *testing.T) {
// 	logger := logrus.New()
// 	logger.Level = logrus.InfoLevel
// 	if *&debug {
// 		logger.Level = logrus.DebugLevel
// 	}
// 	d := createGitRepo(t)
// 	os.Chdir(d)
// 	logger.Debugf("Git repo: %s", d)
// 	src := "src.txt"
// 	srcContents1 := "contents\n"
// 	writeToFile(src, srcContents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m initial")

// 	runCmd("git checkout -b branch1")
// 	srcContents2 := "contents2\n"
// 	writeToFile(src, srcContents2)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'changed on branch'")

// 	runCmd("git checkout main")
// 	runCmd("git merge branch1")
// 	export := "export.txt"
// 	// fast-export with rename detection implemented
// 	output, err := runCmd(fmt.Sprintf("git fast-export --all -M > %s", export))
// 	if err != nil {
// 		t.Errorf("ERROR: Failed to git export '%s': %v\n", export, err)
// 	}
// 	assert.Equal(t, "", output)
// 	// logger.Debugf("Export file:\n%s", export)

// 	// buf := strings.NewReader(input)
// 	g := NewGitP4Transfer(logger)

// 	commits, files := g.RunGetCommits(export)

// 	assert.Equal(t, 2, len(commits))
// 	for k, v := range commits {
// 		switch k {
// 		case 2:
// 			assert.Equal(t, 2, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/branch1", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		case 4:
// 			assert.Equal(t, 4, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/branch1", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}
// 	assert.Equal(t, 2, len(files))
// 	for k, v := range files {
// 		switch k {
// 		case 1:
// 			assert.Equal(t, src, v.name)
// 			assert.Equal(t, srcContents1, v.blob.Data)
// 		case 3:
// 			assert.Equal(t, src, v.name)
// 			assert.Equal(t, srcContents2, v.blob.Data)
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}

// }

// func TestSimpleBranchMerge(t *testing.T) {
// 	logger := logrus.New()
// 	logger.Level = logrus.InfoLevel
// 	if *&debug {
// 		logger.Level = logrus.DebugLevel
// 	}
// 	d := createGitRepo(t)
// 	os.Chdir(d)
// 	logger.Debugf("Git repo: %s", d)
// 	file1 := "file1.txt"
// 	file2 := "file2.txt"
// 	contents1 := "contents\n"
// 	writeToFile(file1, contents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'initial commit on main'")

// 	writeToFile(file1, contents1+contents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'commit2 on main'")

// 	runCmd("git checkout -b branch1")
// 	contents2 := "contents2\n"
// 	writeToFile(file2, contents2)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'changed on branch'")

// 	runCmd("git checkout main")
// 	runCmd("git merge branch1")
// 	export := "export.txt"
// 	// fast-export with rename detection implemented
// 	output, err := runCmd(fmt.Sprintf("git fast-export main --all -M"))
// 	if err != nil {
// 		t.Errorf("ERROR: Failed to git export '%s': %v\n", export, err)
// 	}
// 	assert.NotEqual(t, "", output)
// 	logger.Debugf("Export file:\n%s", output)

// 	// buf := strings.NewReader(input)
// 	g := NewGitP4Transfer(logger)
// 	g.testInput = output
// 	commits, files := g.RunGetCommits("")

// 	assert.Equal(t, 3, len(commits))
// 	for k, v := range commits {
// 		switch k {
// 		case 2:
// 			assert.Equal(t, 2, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/main", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		case 4:
// 			assert.Equal(t, 4, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/main", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		case 6:
// 			assert.Equal(t, 6, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/main", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}
// 	assert.Equal(t, 3, len(files))
// 	for k, v := range files {
// 		switch k {
// 		case 1:
// 			assert.Equal(t, file1, v.name)
// 			assert.Equal(t, contents1, v.blob.Data)
// 		case 3:
// 			assert.Equal(t, file1, v.name)
// 			assert.Equal(t, contents1+contents1, v.blob.Data)
// 		case 5:
// 			assert.Equal(t, file2, v.name)
// 			assert.Equal(t, contents2, v.blob.Data)
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}

// }

// func TestSimpleMerge(t *testing.T) {
// 	logger := logrus.New()
// 	logger.Level = logrus.InfoLevel
// 	if *&debug {
// 		logger.Level = logrus.DebugLevel
// 	}
// 	d := createGitRepo(t)
// 	os.Chdir(d)
// 	logger.Debugf("Git repo: %s", d)
// 	file1 := "file1.txt"
// 	file2 := "file2.txt"
// 	contents1 := "contents\n"
// 	contents1branch := "contents - branch1\n"
// 	runCmd("git checkout main")
// 	writeToFile(file1, contents1)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'initial commit on main'")

// 	runCmd("git checkout -b branch1")
// 	writeToFile(file1, contents1branch)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'commit2 on branch1'")

// 	runCmd("git checkout main")
// 	contents2 := "contents2\n"
// 	writeToFile(file2, contents2)
// 	runCmd("git add .")
// 	runCmd("git commit -m 'changed on main'")

// 	runCmd("git merge branch1")
// 	export := "export.txt"
// 	// Pretty output
// 	debugOut, _ := runCmd("git log --graph --oneline --decorate")
// 	logger.Debugf("Git log:\n%s", debugOut)

// 	// fast-export with rename detection implemented
// 	output, err := runCmd(fmt.Sprintf("git fast-export --all -M --show-original-ids"))
// 	if err != nil {
// 		t.Errorf("ERROR: Failed to git export '%s': %v\n", export, err)
// 	}
// 	assert.NotEqual(t, "", output)
// 	logger.Debugf("Export file:\n%s", output)

// 	// buf := strings.NewReader(input)
// 	g := NewGitP4Transfer(logger)
// 	g.testInput = output
// 	commits, files := g.RunGetCommits("")

// 	assert.Equal(t, 4, len(commits))
// 	for k, v := range commits {
// 		switch k {
// 		case 2:
// 			assert.Equal(t, 2, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/branch1", v.commit.Ref) // unexpected, but seems to be the way things work...
// 			assert.Equal(t, 1, len(v.files))
// 		case 4:
// 			assert.Equal(t, 4, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/main", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		case 6:
// 			assert.Equal(t, 6, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/branch1", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		case 7:
// 			assert.Equal(t, 7, v.commit.Mark)
// 			assert.Equal(t, "refs/heads/main", v.commit.Ref)
// 			assert.Equal(t, 1, len(v.files))
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}
// 	assert.Equal(t, 3, len(files))
// 	for k, v := range files {
// 		switch k {
// 		case 1:
// 			assert.Equal(t, file1, v.name)
// 			assert.Equal(t, contents1, v.blob.Data)
// 		case 3:
// 			assert.Equal(t, file2, v.name)
// 			assert.Equal(t, contents2, v.blob.Data)
// 		case 5:
// 			assert.Equal(t, file1, v.name)
// 			assert.Equal(t, contents1branch, v.blob.Data)
// 		default:
// 			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
// 		}
// 	}

// }

func zipBuf(data string) bytes.Buffer {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := gz.Write([]byte(data)); err != nil {
		log.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		log.Fatal(err)
	}
	return b
}

func createLogger() *logrus.Logger {
	if logger != nil {
		return logger
	}
	logger = logrus.New()
	logger.Level = logrus.InfoLevel
	if *&debug {
		logger.Level = logrus.DebugLevel
	}
	return logger
}

func runTransferWithDump(t *testing.T, logger *logrus.Logger, output string) string {
	g := NewGitP4Transfer(logger)
	g.testInput = output
	opts := GitParserOptions{importDepot: "import", defaultBranch: "main"}
	commitChan := g.GitParse(opts)
	commits := make([]GitCommit, 0)
	// just read all commits and test them
	for c := range commitChan {
		commits = append(commits, c)
	}

	buf := new(bytes.Buffer)
	p4t := MakeP4Test(t.TempDir())
	os.Chdir(p4t.serverRoot)
	logger.Debugf("P4D serverRoot: %s", p4t.serverRoot)

	j := journal.Journal{}
	j.SetWriter(buf)
	j.WriteHeader()
	for _, c := range commits {
		j.WriteChange(c.commit.Mark, c.commit.Msg, int(c.commit.Author.Time.Unix()))
		for _, f := range c.files {
			f.WriteFile(p4t.serverRoot, c.commit.Mark)
			f.WriteJournal(&j, &c)
		}
	}

	jnl := filepath.Join(p4t.serverRoot, "jnl.0")
	writeToFile(jnl, buf.String())
	runCmd("p4d -r . -jr jnl.0")
	runCmd("p4d -r . -J journal -xu")
	runCmd("p4 storage -r")
	runCmd("p4 storage -w")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, "Phase 1 of the storage upgrade has finished.\n", result)

	return p4t.serverRoot
}

func runTransfer(t *testing.T, logger *logrus.Logger) string {
	// fast-export with rename detection implemented
	output, err := runCmd(fmt.Sprintf("git fast-export --all -M"))
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	return runTransferWithDump(t, logger, output)
}

func TestAdd(t *testing.T) {
	logger := createLogger()

	d := createGitRepo(t)
	os.Chdir(d)
	logger.Debugf("Git repo: %s", d)
	src := "src.txt"
	srcContents1 := "contents\n"
	writeToFile(src, srcContents1)
	runCmd("git add .")
	runCmd("git commit -m initial")

	// fast-export with rename detection implemented
	output, err := runCmd(fmt.Sprintf("git fast-export --all -M"))
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Export file:\n%s", output)

	g := NewGitP4Transfer(logger)
	g.testInput = output
	opts := GitParserOptions{importDepot: "import", defaultBranch: "main"}
	commitChan := g.GitParse(opts)
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
	assert.Equal(t, srcContents1, f.blob.Data)

	buf := new(bytes.Buffer)
	j := journal.Journal{}
	j.SetWriter(buf)
	j.WriteHeader()
	c = commits[0]
	j.WriteChange(c.commit.Mark, c.commit.Msg, int(c.commit.Author.Time.Unix()))
	f = c.files[0]
	j.WriteRev(f.depotFile, f.rev, f.p4action, f.fileType, c.commit.Mark,
		f.depotFile, c.commit.Mark, int(c.commit.Author.Time.Unix()))
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

	p4t := MakeP4Test(t.TempDir())
	os.Chdir(p4t.serverRoot)
	logger.Debugf("P4D serverRoot: %s", p4t.serverRoot)
	jnl := filepath.Join(p4t.serverRoot, "jnl.0")
	writeToFile(jnl, expectedJournal)
	f.WriteFile(p4t.serverRoot, c.commit.Mark)
	runCmd("p4d -r . -jr jnl.0")
	runCmd("p4d -r . -J journal -xu")
	result, err := runCmd("p4 storage -r")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, "Phase 1 of the storage upgrade has finished.\n", result)
	result, err = runCmd("p4 files //...")
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
}

func TestAddEdit(t *testing.T) {
	logger := createLogger()

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
	assert.Regexp(t, `Change 4 on .* by git\-user@git\-client`, result)
	assert.Regexp(t, `Change 2 on .* by git\-user@git\-client`, result)
	result, err = runCmd("p4 changes //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `Change 4 on .* by git\-user@git\-client`, result)
	assert.Regexp(t, `Change 2 on .* by git\-user@git\-client`, result)
	result, err = runCmd("p4 storage //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrRev 1.2`, result)
	assert.Regexp(t, `lbrRev 1.4`, result)
}

func TestAddBinary(t *testing.T) {
	logger := createLogger()

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
	assert.Regexp(t, `Change 4 on .* by git\-user@git\-client`, result)
	assert.Regexp(t, `Change 2 on .* by git\-user@git\-client`, result)
	result, err = runCmd("p4 changes //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `Change 4 on .* by git\-user@git\-client`, result)
	assert.Regexp(t, `Change 2 on .* by git\-user@git\-client`, result)
	result, err = runCmd("p4 storage //import/...")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrRev 1.2`, result)
	assert.Regexp(t, `lbrRev 1.4`, result)
}

func TestDeleteFile(t *testing.T) {
	logger := createLogger()

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

func TestRenameRename(t *testing.T) {
	// Rename of a file done twice
	logger := createLogger()

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

func TestRenameDir(t *testing.T) {
	// Git rename of a dir consisting of multiple files - expand to constituent parts
	logger := createLogger()

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

	r := runTransferWithDump(t, logger, newOutput)
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

	r := runTransferWithDump(t, logger, newOutput)
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
	assert.Regexp(t, `lbrFile //import/main/src/file2.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)
}

func TestBranch(t *testing.T) {
	logger := createLogger()

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
	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by git-user@git-client`, result)
	// assert.Regexp(t, `\.\.\. #1 change 4 add on .* by git-user@git-client (text+C) 'changed on dev '`, result)
	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/file1.txt#1`, result)

}

// TestTag - ensure tags are ignored as branch names
func TestTag(t *testing.T) {
	logger := createLogger()

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
}

func TestBigNode(t *testing.T) {
	n := &Node{name: ""}
	files := `Env/Assets/ArtEnv/Cookies/cookie.png
Env/Assets/ArtEnv/Cookies/cookie.png/cookie.png.meta
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
// 	assert.Regexp(t, `\.\.\. #1 change 4 add on .* by git-user@git-client`, result)
// 	// assert.Regexp(t, `\.\.\. #1 change 4 add on .* by git-user@git-client (text+C) 'changed on dev '`, result)
// 	assert.Regexp(t, `\.\.\. \.\.\. branch from //import/main/file1.txt#1`, result)

// }

func TestBranch2(t *testing.T) {
	// Multiple branches
	logger := createLogger()

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

	// result, err = runCmd("p4 print -q //import/main/file1.txt#1")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, contents1, result)

	// result, err = runCmd("p4 print -q //import/dev/file1.txt#1")
	// assert.Equal(t, nil, err)
	// assert.Equal(t, contents2, result)

	result, err = runCmd("p4 fstat -Ob //import/dev/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrFile //import/dev/file2.txt`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.3.gz$`, result)

	result, err = runCmd("p4 filelog //import/dev/file2.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `//import/dev/file2.txt`, result)
	assert.Regexp(t, `\.\.\. #1 change 3 add on .* by git-user@git-client`, result)
	// assert.Regexp(t, `\.\.\. #1 change 4 add on .* by git-user@git-client (text+C) 'changed on dev '`, result)
	assert.Regexp(t, `\.\.\. \.\.\. edit into //import/dev2/file2.txt#1`, result)

}

func TestBranchMerge(t *testing.T) {
	// Merge branches
	logger := createLogger()

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
\.\.\. #1 change 6 add on .* by git-user@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //import/main/file1.txt#1
\.\.\. \.\.\. edit into //import/main/file1.txt#1,#2`, result)

	assert.Regexp(t, `(?m)//import/main/file1.txt
\.\.\. #2 change 7 edit on .* by git-user@git-client \(text\+C\).*
\.\.\. \.\.\. branch from //import/branch1/file1.txt#1
\.\.\. #1 change 2 add on .* by git-user@git-client \(text\+C\).*
\.\.\. \.\.\. edit into //import/branch1/file1.txt#1`, result)

	assert.Regexp(t, `(?m)//import/main/file2.txt
\.\.\. #1 change 4 add on .* by git-user@git-client \(text\+C\).*`, result)

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
