// Tests for gitp4transfer

package main

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

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

func TestParseBasic(t *testing.T) {
	input := `blob
mark :1
data 5
test

reset refs/heads/main
commit refs/heads/main
mark :2
author Robert Cowham <rcowham@perforce.com> 1644399073 +0000
committer Robert Cowham <rcowham@perforce.com> 1644399073 +0000
data 5
test
M 100644 :1 test.txt

`

	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	// logger.Level = logrus.DebugLevel
	// buf := strings.NewReader(input)
	g := NewGitP4Transfer(logger)
	g.testInput = input

	commits, files := g.Run("")

	assert.Equal(t, 1, len(commits))
	for k, v := range commits {
		switch k {
		case 2:
			assert.Equal(t, 2, v.commit.Mark)
			assert.Equal(t, 1, len(v.files))
			assert.Equal(t, "Robert Cowham", v.commit.Committer.Name)
			assert.Equal(t, "rcowham@perforce.com", v.commit.Author.Email)
		default:
			assert.Fail(t, "Unexpected range")
		}
	}
	assert.Equal(t, 1, len(files))
	for k, v := range files {
		switch k {
		case 1:
			assert.Equal(t, "test.txt", v.name)
			assert.Equal(t, "test\n", v.blob.Data)
		default:
			assert.Fail(t, fmt.Sprintf("Unexpected range: %d", k))
		}
	}

}
