package main

// gitgraph program
// This processes a git fast-export file and writes the following:
//   * a graph file (graphviz dot format) showing git commit relationships

import (
	"bufio"
	"fmt"
	"io"               // profiling only
	_ "net/http/pprof" // profiling only
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emicklei/dot"
	libfastimport "github.com/rcowham/go-libgitfastimport"

	"github.com/perforce/p4prometheus/version"
	"github.com/sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
)

var defaultP4user = "git-user" // Default user if non found

type GitGraphOption struct {
	gitExportFile string
	graphFile     string
	firstCommit   int
	lastCommit    int
	maxCommits    int
	squash        bool
	debugCommit   int // For debug breakpoint
}

// GitCommit - A git commit
type GitCommit struct {
	commit       *libfastimport.CmdCommit
	user         string
	branch       string // branch name
	label        string // node label
	parentBranch string
	childCount   int
	mergeCount   int
	hasNode      bool
	gNode        dot.Node // Optional link to GraphizNode
}

// HasPrefix tests whether the string s begins with prefix (or is prefix)
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[0:len(prefix)] == prefix
}

func getUserFromEmail(email string) string {
	if email == "" {
		return defaultP4user
	}
	parts := strings.Split(email, "@")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return defaultP4user
}

func newGitCommit(commit *libfastimport.CmdCommit) *GitCommit {
	user := getUserFromEmail(commit.Author.Email)
	gc := &GitCommit{commit: commit, user: user}
	gc.branch = strings.Replace(commit.Ref, "refs/heads/", "", 1)
	if hasPrefix(gc.branch, "refs/tags") || hasPrefix(gc.branch, "refs/remote") {
		gc.branch = ""
	}
	gc.label = fmt.Sprintf("Commit: %d %s", gc.commit.Mark, gc.branch)
	return gc
}

type CommitMap map[int]*GitCommit

// GitGraph - Transfer via git fast-export file
type GitGraph struct {
	logger    *logrus.Logger
	opts      GitGraphOption
	commits   map[int]*GitCommit
	testInput string     // For testing only
	graph     *dot.Graph // If outputting a graph
}

func NewGitGraph(logger *logrus.Logger, opts *GitGraphOption) *GitGraph {
	g := &GitGraph{logger: logger,
		opts:    *opts,
		commits: make(map[int]*GitCommit)}
	return g
}

// ParseGitImport - incrementally parse the git file, collecting stats and optionally saving archives as we go
// Useful for parsing very large git fast-export files without loading too much into memory!
func (g *GitGraph) ParseGitImport() {
	var buf io.Reader

	if g.testInput != "" {
		buf = strings.NewReader(g.testInput)
	} else {
		file, err := os.Open(g.opts.gitExportFile)
		if err != nil {
			fmt.Printf("ERROR: Failed to open file '%s': %v\n", g.opts.gitExportFile, err)
			os.Exit(1)
		}
		defer file.Close()
		buf = bufio.NewReader(file)
	}

	var cmt *GitCommit
	lastBranchCommit := make(map[string]int, 0) // Record last commit per branch
	branchSkipCount := make(map[string]int, 0)  // Record how many have been skipped per branch

	f := libfastimport.NewFrontend(buf, nil, nil)
CmdLoop:
	for {
		cmd, err := f.ReadCmd()
		if err != nil {
			if err != io.EOF {
				g.logger.Errorf("Failed to read cmd1: %v", err)
				panic("Unrecoverable error")
			} else {
				break
			}
		}
		switch cmd.(type) {
		case libfastimport.CmdCommit:
			commit := cmd.(libfastimport.CmdCommit)
			g.logger.Infof("Commit:  %+v", commit)
			cmt = newGitCommit(&commit)
			g.commits[commit.Mark] = cmt
			if cmt.commit.From != "" {
				if intVar, err := strconv.Atoi(cmt.commit.From[1:]); err == nil {
					parent := g.commits[intVar]
					parent.childCount += 1
					if cmt.branch == "" {
						cmt.branch = parent.branch
					}
					// currCommit.parentBranch = parent.parentBranch
					// if currCommit.parentBranch == "" {
					cmt.parentBranch = parent.branch
					// }
				}
			} else {
				cmt.branch = "main"
			}
			if len(cmt.commit.Merge) > 0 {
				for _, merge := range cmt.commit.Merge {
					if intVar, err := strconv.Atoi(merge[1:]); err == nil {
						mergeCmt := g.commits[intVar]
						mergeCmt.mergeCount += 1
					}
				}
			}
			if g.opts.maxCommits != 0 && len(g.commits) > g.opts.maxCommits {
				break CmdLoop
			}

		default:
		}
	}
	keys := make([]int, 0)
	for k := range g.commits {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	// Now we create graph nodes as appropriate
	for _, k := range keys {
		cmt := g.commits[k]
		if (g.opts.firstCommit == 0 || cmt.commit.Mark >= g.opts.firstCommit) &&
			(g.opts.lastCommit == 0 || cmt.commit.Mark <= g.opts.lastCommit) {
			if !g.opts.squash ||
				cmt.branch != cmt.parentBranch ||
				len(cmt.commit.Merge) > 0 ||
				cmt.mergeCount != 0 ||
				cmt.childCount > 1 ||
				cmt.commit.Mark == g.opts.firstCommit ||
				cmt.commit.Mark == g.opts.lastCommit {
				if pid, ok := lastBranchCommit[cmt.branch]; ok {
					cmt.commit.From = fmt.Sprintf(":%d", pid) // reset parent
				}
				cmt.gNode = g.graph.Node(cmt.label)
				cmt.hasNode = true
				g.createGraphEdges(cmt, branchSkipCount[cmt.branch])
				lastBranchCommit[cmt.branch] = cmt.commit.Mark
				branchSkipCount[cmt.branch] = 0
			} else {
				branchSkipCount[cmt.branch] += 1
			}
		}
	}
}

func (g *GitGraph) createGraphEdges(cmt *GitCommit, skipCount int) {
	// Sets the branch for the current commit, using its parent if not otherwise specified
	if cmt == nil {
		return
	}
	if cmt.commit.From != "" {
		if intVar, err := strconv.Atoi(cmt.commit.From[1:]); err == nil {
			parent := g.commits[intVar]
			if parent != nil {
				parent.gNode = g.graph.Node(parent.label)
				label := "p"
				if skipCount > 0 {
					label = fmt.Sprintf("p%d", skipCount)
				}
				g.graph.Edge(parent.gNode, cmt.gNode, label)
			}
		}
	}
	if len(cmt.commit.Merge) < 1 {
		return
	}
	for _, merge := range cmt.commit.Merge {
		if intVar, err := strconv.Atoi(merge[1:]); err == nil {
			mergeFrom := g.commits[intVar]
			if mergeFrom != nil {
				mergeFrom.gNode = g.graph.Node(mergeFrom.label)
				g.graph.Edge(mergeFrom.gNode, cmt.gNode, "m")
			}
		}
	}
}

func main() {
	// Tracing code
	// ft, err := os.Create("trace.out")
	// if err != nil {
	// 	panic(err)
	// }
	// defer ft.Close()
	// err = trace.Start(ft)
	// if err != nil {
	// 	panic(err)
	// }
	// defer trace.Stop()
	// End of trace code
	// var err error

	// Turn on profiling
	// defer profile.Start(profile.MemProfile).Stop()
	// go func() {
	// 	http.ListenAndServe(":8080", nil)
	// }()

	var (
		gitexport = kingpin.Arg(
			"gitexport",
			"Git fast-export file to process.",
		).String()
		maxCommits = kingpin.Flag(
			"max.commits",
			"Max no of commits to process (default 0 means all).",
		).Default("0").Short('m').Int()
		outputGraph = kingpin.Flag(
			"output",
			"Graphviz dot file to output git commit/file structure to.",
		).Short('o').String()
		graphFirstCommit = kingpin.Flag(
			"first.commit",
			"ID of first commit to include in graph output (default 0 means all commits).",
		).Default("0").Short('f').Int()
		graphLastCommit = kingpin.Flag(
			"last.commit",
			"ID of last commit to include in graph output (default of 0 means all commits).",
		).Default("0").Short('l').Int()
		squash = kingpin.Flag(
			"squash",
			"Squash commits (leaving branches/merges only).",
		).Short('s').Bool()
		debug = kingpin.Flag(
			"debug",
			"Enable debugging level.",
		).Default("0").Int()
		// debugCommit = kingpin.Flag(
		// 	"debug.commit",
		// 	"For debugging - to allow breakpoints to be set - only valid if debug > 0.",
		// ).Default("0").Int()
	)
	kingpin.UsageTemplate(kingpin.CompactUsageTemplate).Version(version.Print("gitgraph")).Author("Robert Cowham")
	kingpin.CommandLine.Help = "Parses one or more git fast-export files to create a graphviz DOT file\n"
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *debug > 0 {
		logger.Level = logrus.DebugLevel
	}
	startTime := time.Now()
	logger.Infof("%v", version.Print("gitp4transfer"))
	logger.Infof("Starting %s, gitexport: %v", startTime, *gitexport)

	opts := &GitGraphOption{
		gitExportFile: *gitexport,
		maxCommits:    *maxCommits,
		graphFile:     *outputGraph,
		firstCommit:   *graphFirstCommit,
		lastCommit:    *graphLastCommit,
		squash:        *squash,
	}
	logger.Infof("Options: %+v", opts)
	logger.Infof("OS: %s/%s", runtime.GOOS, runtime.GOARCH)
	g := NewGitGraph(logger, opts)
	g.graph = dot.NewGraph(dot.Directed)
	g.ParseGitImport()
	f, err := os.OpenFile(g.opts.graphFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		g.logger.Error(err)
	}
	defer f.Close()

	f.Write([]byte(g.graph.String()))

}
