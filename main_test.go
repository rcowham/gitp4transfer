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
	"testing"

	"github.com/rcowham/gitp4transfer/journal"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

var debug bool = false

func init() {
	flag.BoolVar(&debug, "debug", false, "Set to have debug logging for tests.")
}

func runCmd(cmdLine string) (string, error) {
	cmd := exec.Command("/bin/bash", "-c", cmdLine)
	stdout, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(stdout), nil
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
	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *&debug {
		logger.Level = logrus.DebugLevel
	}
	return logger
}

func runTransfer(t *testing.T, logger *logrus.Logger) {

	// fast-export with rename detection implemented
	output, err := runCmd(fmt.Sprintf("git fast-export --all -M"))
	if err != nil {
		t.Errorf("ERROR: Failed to git export '%s': %v\n", output, err)
	}
	logger.Debugf("Export file:\n%s", output)

	g := NewGitP4Transfer(logger)
	g.testInput = output
	opts := GitParserOptions{importDepot: "import"}
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
			j.WriteRev(f.depotFile, f.rev, f.p4action, f.fileType, c.commit.Mark, int(c.commit.Author.Time.Unix()))
		}
	}

	jnl := filepath.Join(p4t.serverRoot, "jnl.0")
	writeToFile(jnl, buf.String())
	runCmd("p4d -r . -jr jnl.0")
	runCmd("p4d -r . -J journal -xu")

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
	opts := GitParserOptions{importDepot: "import"}
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
	j.WriteRev(f.depotFile, f.rev, f.p4action, f.fileType, c.commit.Mark, int(c.commit.Author.Time.Unix()))
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
@pv@ 3 @db.rev@ @//import/src.txt@ 1 3 1 2 %d %d 00000000000000000000000000000000 @//import/src.txt@ @1.2@ 3 
@pv@ 0 @db.revcx@ 2 @//import/src.txt@ @1.1@ 1 
`, dt, dt, dt)
	assert.Equal(t, expectedJournal, buf.String())

	p4t := MakeP4Test(t.TempDir())
	os.Chdir(p4t.serverRoot)
	logger.Debugf("P4D serverRoot: %s", p4t.serverRoot)
	jnl := filepath.Join(p4t.serverRoot, "jnl.0")
	writeToFile(jnl, expectedJournal)
	runCmd("p4d -r . -jr jnl.0")
	runCmd("p4d -r . -J journal -xu")
	f.WriteFile(p4t.serverRoot, c.commit.Mark)
	result, err := runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/src.txt#1 - edit change 2 (text+C)\n", result)
	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "", result)
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

	runTransfer(t, logger)

	result, err := runCmd("p4 files //...@2")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/src.txt#1 - edit change 2 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents1, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/src.txt#2 - edit change 4 (text+C)\n", result)

	result, err = runCmd("p4 print -q //import/src.txt#2")
	assert.Equal(t, nil, err)
	assert.Equal(t, srcContents2, result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/src.txt#2")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `lbrPath .*/1.4.gz`, result)
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
	assert.Equal(t, "//import/src.txt.gz#1 - edit change 2 (binary+F)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/src.txt.gz#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType binary\+F`, result)
	assert.Regexp(t, `lbrType binary\+F`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2$`, result)
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
	assert.Equal(t, "//import/src.txt#1 - add change 2 (text+C)\n", result)

	result, err = runCmd("p4 verify -qu //...")
	assert.Equal(t, "<nil>", fmt.Sprint(err))
	assert.Equal(t, "", result)

	result, err = runCmd("p4 fstat -Ob //import/src.txt#1")
	assert.Equal(t, nil, err)
	assert.Regexp(t, `headType text\+C`, result)
	assert.Regexp(t, `lbrType text\+C`, result)
	assert.Regexp(t, `(?m)lbrPath .*/1.2.gz$`, result)

	result, err = runCmd("p4 files //...")
	assert.Equal(t, nil, err)
	assert.Equal(t, "//import/src.txt#2 - delete change 3 (text)\n", result)

}
