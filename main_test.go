// Tests for gitp4transfer

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"testing"

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

func TestAdd(t *testing.T) {
	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *&debug {
		logger.Level = logrus.DebugLevel
	}
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
	commitChan := g.GitParse("")
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

}
