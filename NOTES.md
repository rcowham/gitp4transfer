# gitp4transfer - Go version

This is experimental. This doc is notes for ToDo list etc.

## Debugging

For a single test (with debug output):

    go test -run TestBranch -debug

    ~/go/bin/dlv debug github.com/rcowham/gitp4transfer -- --archive.root=/data/work/OnwardRepo.p4/p4root --debug=1 --max.commits=50 --import.depot=import --journal=/data/work/OnwardRepo.p4/p4root/jnl.0 /data/work/git-OnwardRepo


## Todo

* Report on everything
* Option to extract all
* Need to rename branches, or remap them
** Duplicate - or auto-write branch values?
* Rename of file on a branch (not previously changed)

Concerns:

* UTF encoding issues?

## Test Scenarios

* Given a root dir, write files
** For objects, write files as soon as you get a filename? Or at least consider that.
** Gzip or not
** Detect base file formats using magic signatures
** Issue around main/branch files
*** If From: is blank then assume on main/master?
* Specify main/master and follow back commits on that branch

Options:

* Parse files and contents and write out
** Requires changelist numbers - just use Marks from file.
* Option to filter only a subset of files (on any branch)
* Option to filter a single branch
* Mappings - to rename branches

## repo_p4transfer: go-git vs exec'd git (native Git import path)

Question: would using go-git for the git commands in `internal/importer/git_repository.go`
have less overhead than spawning `git` via `os/exec`?

### Where the exec overhead actually is

Per-commit git calls break into two categories:

Already amortized (no problem):
* Blob reads go through one long-lived `git cat-file --batch-command` process
  (`startCatFile`/`OpenBlob`), reused for the whole source lifetime. No per-blob
  spawn - this is the highest-frequency operation and it's already solved.
* `Validate()` spawns `rev-list`/`cat-file --batch-check` once per batch, not
  per commit.

Spawned per commit (the real cost):
* `readCommitMetadata` -> `git show -s --format=...` - one process per commit.
* `readChanges` -> `git diff-tree ...` - one process per commit.
* Each rename/copy -> `git rev-parse <parent>:<path>` - one extra process per
  renamed/copied file.

For `frameworks/base`'s 55,325 commits, that's ~110,000+ process spawns for one
repo, times ~1000 repos for a full repo_p4transfer manifest.

### Would go-git help?

Yes in principle - it eliminates fork/exec entirely (real savings on Windows,
where CreateProcess is notably expensive; less dramatic on Linux/macOS where
fork/exec plus git's own startup, config/refs/packed-refs, is single-digit ms
per call, still non-trivial at this scale). But:

1. The cheap fix doesn't require go-git at all. `git diff-tree` supports
   `--stdin`, so `readCommit`/`readChanges` could follow the exact pattern
   already used for blobs: one persistent `git diff-tree --stdin` (and
   similarly batch `git show`/`git log --format=...` across the whole batch)
   instead of one process per commit. That removes almost all the actual spawn
   overhead while staying on the reference git implementation - a much smaller,
   lower-risk change than swapping libraries.

2. go-git has real downsides for this specific tool:
   * Rename/copy detection (`-M -C --find-copies-harder`) is a mature,
     heavily-tuned C heuristic; go-git's tree-diff/rename detection is known to
     be slower and less accurate on large trees.
   * This code explicitly sets `GIT_NO_LAZY_FETCH=1` and handles partial/shallow
     clones - go-git's parity with promisor objects and shallow clones is
     weaker than the real git CLI.
   * For a Perforce migration tool, behavioral fidelity to actual git semantics
     matters more than raw speed - subtle go-git differences (tree ordering,
     similarity thresholds, submodule/symlink edge cases) risk silently-wrong
     migrated history, a much worse failure mode than "slow".

Recommendation: batch the existing `git show`/`diff-tree` calls into persistent
processes (same pattern as `cat-file --batch-command`) rather than migrating to
go-git.

### Implemented (internal/importer/git_repository.go)

* Commit metadata: one `git log --no-walk --stdin --format=...` call per batch
  (was one `git show` per commit). Empirically confirmed `git log --stdin`
  buffers its output rather than streaming per line, so this is a single
  blocking bulk call, not an interactive persistent process; results are
  cached in a map keyed by commit ID.
* File changes: one persistent `git diff-tree --stdin` process per source
  (was one spawn per commit). A writer goroutine feeds every planned commit ID
  ahead of the reader so the reader never blocks waiting to detect a record
  boundary; verified empirically that `diff-tree --stdin` streams per request
  (unlike `git log --stdin`).
* Rename/copy source blob lookup: reuses the existing persistent
  `cat-file --batch-command` process via an `info <rev>` request (was one
  `git rev-parse` spawn per renamed/copied file).
