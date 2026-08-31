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

## repo_p4transfer: configurable parallel `git cat-file --batch` processes?

Question: would a configurable number of parallel `git cat-file --batch`
processes help native-Git import performance?

Short answer: not by itself - there's no concurrent call site today that would
actually use extra processes.

### What's parallel today vs what's serial

* Parallel: `P4Writer`'s `ParallelWorkers` archive workers only do the
  staged-file -> gzip-compressed archive step, via the `archiveJobs` channel.
* Serial: everything upstream. `WriteCommit` loops over a commit's file
  changes one at a time; for each, `writeChange` calls `change.Content()`
  (hits the single persistent `cat-file --batch-command` process) and
  synchronously copies the full blob into a staging file before moving to the
  next file. Only then is the file handed off (async) to the compression pool.
  So the git-read step is a strict bottleneck regardless of
  `--parallel-workers`.

### Why more cat-file processes wouldn't help as-is

`cat-file --batch-command`'s request/response protocol is single-threaded per
process - concurrent requests on one process would corrupt the stream. Real
parallelism would need:

1. A pool of N separate `cat-file --batch-command` processes (not N requests
   to one process).
2. Restructuring `writeChange`'s per-commit file loop to fan out across
   goroutines bounded by that pool - this is the real work, not the process
   pool itself.
3. Mutex-guarding `P4Writer`'s shared maps (`fileRevisions`, `blobArchives`,
   `commitBranches`), currently mutated assuming single-threaded access.
4. Handling journal-order determinism - `WriteRev`/`WriteInteg` calls happen
   in a fixed per-commit file order today; parallelizing reads means either
   reordering results back to source order before writing the journal, or
   accepting non-deterministic output (risky for a migration tool, and would
   break golden-output-style tests like `main_test.go`'s regex assertions).

### When it would actually pay off

* Repos with large/binary blobs where per-blob decompression or I/O wait
  dominates (multiple processes can use multiple cores/concurrent I/O queue
  depth).
* Slow or network-backed repo storage (NFS, network disks) where read latency
  benefits from concurrency.
* Not much benefit for the common case of many small text files on local SSD -
  there the bottleneck is more likely per-request looping overhead (already
  fixed by the persistent process) or the gzip compression step (already
  parallelized).

Recommendation: profile a real batch first to see whether wall-clock time is
actually dominated by git blob reads vs compression, before adding this. If it
is, the fix is "parallelize `writeChange`'s file loop with a bounded worker
pool of paired (cat-file process, mutex-safe map access)" - a real
concurrency-model change, not just a new config knob.

## repo_p4transfer: is --native-git restartable after a crash?

Was no: the journal was truncated (`os.Create`) on every run, `P4Writer`'s
changelist counter and revision/dedup maps were in-memory only, and there was
no record of which batches had already completed. `GitRepositorySource` had a
`Checkpoint`/`Resume` primitive, but `importNativeGit` never wired it up.

### Implemented (batch-granularity checkpoint/resume)

* `internal/importer/p4_writer.go`: exported `WriterState` (+ `FileRevisionState`,
  `BlobArchiveState`) and `P4Writer.ExportState()`/`P4WriterOptions.InitialState`,
  so a caller can snapshot and restore `changeNumber`, `fileRevisions`,
  `blobArchives`, `commitBranches`, `branchReady`. Supplying `InitialState`
  also skips re-writing the journal header (the journal is being appended to,
  not recreated).
* `cmd/repo_p4transfer/repo_p4transfer.go`: after each batch fully completes,
  `importNativeGit` atomically writes `<journal>.checkpoint.json` (temp file +
  rename) containing the next batch index, the journal's current byte offset,
  `snapshotsCreated`, and `writer.ExportState()`. On startup, if that
  checkpoint exists (and `--fresh-start` wasn't given), the journal is
  truncated back to the recorded offset (discarding any partially-written
  in-progress batch) and reopened for append, `P4Writer` is seeded from the
  saved state, and processing resumes at the recorded batch index. On full
  completion the checkpoint file is removed.
* New `--fresh-start` flag discards an existing checkpoint and reimports from
  scratch.

Resume granularity is per-batch, not per-commit: an interrupted batch is
redone from its start (can mean re-processing a large batch, e.g. the
55,325-commit `frameworks/base` batch that motivated the earlier
argument-list-too-long fix), but this keeps the design simple and avoids
per-commit state-serialization overhead (batches can be recomputed from a
fresh `BuildTimeline()` scan since repos aren't expected to change underfoot
during an import). Finer-grained per-commit resume (via the existing
`GitRepositorySource.Checkpoint`/`Resume`) is a possible future enhancement if
batch-level redo proves too costly in practice.

Tested via `TestImportNativeGitResumesAfterInterruption` (interrupts a
2-repo, 4-batch import by hiding one repo's directory, resumes, and asserts
the resulting journal is byte-identical to an uninterrupted run) and
`TestP4WriterExportStateResumesAcrossInstances`.

### Clean stop on request (Ctrl-C / SIGTERM)

Added `RepoP4Transfer.RequestStop()` (backed by `atomic.Bool`), checked at the
top of each batch iteration in `importNativeGit`: if set, the currently
in-flight batch finishes and is checkpointed as usual, then the loop stops
before starting the next batch (returns nil, not an error). `main()` wires
`SIGINT`/`SIGTERM` to call `RequestStop()`; a second signal forces an
immediate `os.Exit`. Re-running the command resumes via the same
checkpoint/resume mechanism as a crash. Tested via
`TestImportNativeGitStopsCleanlyOnRequest`.

## repo_p4transfer: Validate() was re-walking full repo history every batch

Real-world log evidence (`frameworks/base`, 52,505 batches): a batch of 3
trivial (0-file-change) merge commits took 16s, and the overall average was
~11s/batch regardless of batch size - a near-constant per-batch cost pointed
at something scanning proportional to total repo size, not batch size.

Root cause confirmed by direct measurement: `GitRepositorySource.Validate()`
ran `git rev-list --objects --stdin <batch commit IDs>` with no exclusion
boundary, so every batch walked the *entire* reachable history from those
commits - for `frameworks/base` that's 2,201,987 objects in 8.68s, every
single batch. Adding `--not <boundary>` (the last commit already validated for
that repo) cut it to 56 objects in 0.07s - a ~124x speedup for that batch.

### Implemented

* `internal/importer/git_repository.go`: `GitRepositorySourceOptions.ValidateBoundary`
  excludes a commit's ancestry from `Validate()`'s `rev-list --objects` walk.
  Passed as an argv `--not <boundary>` flag (not a stdin line - git rejects
  `--not` as a raw stdin line under `--stdin` mode: "options not supported in
  --stdin mode"), which applies correctly to stdin-supplied positive revisions.
* `cmd/repo_p4transfer/repo_p4transfer.go`: tracks `validatedThrough
  map[string]string` (repo path -> last validated commit), updated after each
  successful `Validate()` and passed as the next batch's boundary for that
  repo. Persisted in `nativeGitCheckpoint.ValidatedThrough` so resumed runs
  don't lose the bound and fall back to full-history walks.
* A single boundary commit is sufficient even for merge commits pulling in a
  second parent from elsewhere: `--not` computes a genuine set difference, so
  truly-unvalidated objects reachable via an unrelated parent are still walked
  and checked; only objects already known-good (reachable from the boundary)
  are skipped.

Tested via `TestGitRepositorySourceValidateBoundaryExcludesPriorAncestry`
(bounded and unbounded validation both succeed; a nonexistent boundary commit
causes a git error, proving the option is actually wired through rather than
silently ignored).

## repo_p4transfer: "diff-tree commit mismatch" crash on merge/empty commits

Real-world crash after the Validate() fix above: `readChanges` failed with
"diff-tree commit mismatch for <commit>: got <other-commit>" partway through a
large `frameworks/base` batch.

Root cause: `git diff-tree --stdin` produces **no output at all** - not even
the commit's own echoed ID - for a commit whose diff-tree output would
otherwise be empty. This happens for (a) a commit with no tree difference from
its parent, and (b) any true multi-parent merge commit, since git suppresses
merge diffs by default unless `-m`/`-c`/`--cc` is given. Confirmed empirically:
an `--allow-empty` commit and a real 2-parent merge commit both produced zero
bytes from `git diff-tree --stdin --raw -z ...` alone.

`readChanges`'s persistent-process protocol assumed every requested commit
would always be echoed first; when one wasn't, it read the *next* commit's
echo instead and errored on the mismatch.

### Fix

`internal/importer/git_repository.go`: replaced the hard mismatch error with
`consumeDiffEcho`, which checks whether the next available token equals the
requested commit ID. If not, since git only ever emits echoes in request order
and never repeats one, the token must belong to a later commit - it's buffered
(as before) and the current commit is reported with zero file changes (which
matches the file-level effect of a suppressed/empty diff, and matches what the
original one-shot `git diff-tree <single-commit>` implementation would have
produced before this was optimized into a persistent process).

Tested via `TestGitRepositorySourceHandlesCommitsWithNoDiffTreeOutput`
(sequence of real commit, empty commit, true merge commit, real commit -
verifies zero file changes for the empty/merge commits and correct file
changes for the real ones either side, all through the same persistent
diff-tree process).

## repo_p4transfer: checkpoint save became the dominant per-batch cost

After the Validate() fix above, debug timing logs at ~95% through a real
`frameworks/base` run (23,079 file revisions, 48,931 blob archives tracked)
showed:

```
Validate() took 15-104ms
processed N commits in 5-16ms
source Close() took ~20us
checkpoint save took ~428ms
```

Checkpoint save (`journalOutput.Sync()` + `writer.ExportState()` + JSON-marshal
the whole `WriterState` + `os.WriteFile` + `os.Rename`) dominated every batch
by roughly 4-25x, and - critically - its cost is proportional to the *total
accumulated state size*, not to the batch just completed. A 1-commit batch and
a 4-commit batch both took ~427ms to checkpoint. Doing this after literally
every one of 52,524 batches meant paying that cost tens of thousands of times
over the run.

### Fix

`cmd/repo_p4transfer/repo_p4transfer.go`: gate the checkpoint save by elapsed
wall-clock time (`--checkpoint-interval`, default 30s) instead of doing it
after every batch. A stop request (Ctrl-C/SIGTERM) always forces an immediate
save regardless of the interval, so no completed work is lost when stopping
cleanly. This trades a larger "redo window" on crash (up to one interval's
worth of batches, or one batch if it alone exceeds the interval) for far less
time spent on checkpointing overall.

Tested via `TestImportNativeGitCheckpointIntervalSkipsSaves` (a long interval
means an earlier successful batch's progress isn't yet on disk when a later
batch fails, proving the skip actually happens).

## repo_p4transfer: "Transfer completed successfully" logged after a clean stop

A user hit Ctrl-C mid-import; the log correctly showed "Stopped cleanly;
checkpoint saved ... (rerun to resume)" but then ALSO logged "Transfer
completed successfully" - because `main()` logged that unconditionally after
`ExportAndImport()` returned nil, without distinguishing "stopped early,
partial" from "fully completed" (`importNativeGit` returns nil in both cases).
`run_repo_conversion.sh` then proceeded to build a P4 server from the partial
journal, which is misleading/wasteful (that server would need to be discarded
and rebuilt after resuming).

### Fix

* `cmd/repo_p4transfer/repo_p4transfer.go`: added `RepoP4Transfer.importIncomplete`
  (set when `importNativeGit` stops early), exposed via `ImportIncomplete()`.
  `main()` now logs a distinct warning and exits with status 2 instead of
  claiming success when the import is partial.
* `run_repo_conversion.sh`: checks the exit code explicitly; on exit 2 it
  prints the P4Root location and stops *before* running `p4d -jr`, instead of
  building a server from a partial journal.

Tested via updated assertions in `TestImportNativeGitStopsCleanlyOnRequest`
(`ImportIncomplete()` true after a stop, false after full completion).


