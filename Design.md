# Design for multi-git repo conversion

## Overview

- Want to import Android source from git repos managed by the "repo" tool
- Potentially hundreds or more independent git repos
- Only want to take a small number of named branches
  
## Specs

- accept a list of git repos to import
- accept a start date and and an end date
- analyse the various repos to list all relevant commits in date time order across each git repo
- suggest a way to import those incrementally into P4
  - consider the current strategy of using git fast-export to create dump files
- List pros and cons of different approaches

## Design Notes

### Analysis Approach

Given the specs requirement to "analyse the various repos to list all relevant commits in date time order across each git repo", the design must:

1. **Discover and Index Repositories**
   - Accept a configuration list of git repos to import
   - For each repo, identify all branches matching the selection criteria
   - Extract commit metadata (hash, author, date, message) for commits within the date range

2. **Cross-Repo Timeline Analysis**
   - Merge commit logs from all repos into a global timeline sorted by commit date
   - Handle commits with identical timestamps from different repos
   - Preserve parent-child relationships within each repo

### Import Strategy Evaluation

#### Approach 1: Sequential Git Fast-Export
**Pros:**
- Leverages existing git fast-export infrastructure
- Preserves complete commit history including branches and merges
- Can create multiple dump files, one per repo or per date range
- Allows incremental import in chronological order
- Maintains file content integrity and binary handling

**Cons:**
- Requires sequential processing (may be slow for hundreds of repos)
- Multiple P4 submissions needed (one per dump file)
- May create redundant branches in P4 if repos have conflicting branch names
- Difficult to handle interdependencies between repos

#### Approach 2: Chronological Consolidation
**Pros:**
- Single timeline view enables proper causality ordering
- Can deduplicate common commits if repos share ancestry
- Facilitates P4 branch strategy aligned with actual history

**Cons:**
- Complex logic to merge multiple repos' histories
- Requires mapping file paths to avoid collisions
- Lose per-repo branch structure unless explicitly preserved
- Higher complexity in implementation

#### Approach 3: Hybrid Approach
**Pros:**
- Process repos individually but import in global chronological order
- Preserve repo structure while respecting cross-repo timing
- Balance between simplicity and correctness

**Cons:**
- Medium complexity implementation
- Still requires merging timelines

### Recommended Approach

**Hybrid Sequential Processing:**
1. Scan all repos to build a global timeline index (lightweight metadata only)
2. Group commits into time-ordered batches (e.g., daily or by date range specified)
3. For each batch, process contributing repos in order
4. Use git fast-export per batch, create individual P4 submissions
5. Track import state to enable resumption after failures

This balances correctness, implementability, and robustness.

### Key Implementation Considerations

- **Configuration**: YAML/JSON format specifying repos, branches, date ranges
- **Commit Filtering**: Allow filtering by branch pattern and date range
- **Namespace Handling**: Strategy for avoiding file path collisions across repos
- **Metadata Preservation**: Maintain original commit authorship and timestamps in P4
- **Error Handling**: Resumable imports with checkpointing for large batches
- **Verification**: Diff repos before/after import to catch data loss

### Extra notes

Source repos from Android have a structure:

Name

accessories/manifest
assets/android-studio-ux-assets
brillo/manifest
device/aaeon/upboard
device/amlogic/yukawa
device/amlogic/yukawa-kernel
:
platform/packages/apps/AccountsAndSyncSettings
platform/packages/apps/AlarmClock
platform/packages/apps/AvatarPicker
platform/packages/apps/BasicSmsReceiver
platform/packages/apps/Benchmark
:

So file pathes within those repos will not have any conflicts.


An example of the default manifest file is:

```
<?xml version="1.0" encoding="UTF-8"?>
<manifest>

  <remote  name="aosp"
           fetch=".."
           review="https://android-review.googlesource.com/" />
  <default revision="android17-release"
           remote="aosp"
           sync-j="4" />

  <superproject name="platform/superproject" remote="aosp" revision="android-latest-release"/>
  <contactinfo bugurl="go/repo-bug" />

  <!-- BEGIN open-source projects -->
  <project path="build/make" name="platform/build" groups="pdk,sysui-studio" >
    <linkfile src="CleanSpec.mk" dest="build/CleanSpec.mk" />
    <linkfile src="buildspec.mk.default" dest="build/buildspec.mk.default" />
    <linkfile src="core" dest="build/core" />
    <linkfile src="envsetup.sh" dest="build/envsetup.sh" />
    <linkfile src="target" dest="build/target" />
    <linkfile src="tools" dest="build/tools" />
  </project>
  <project path="build/blueprint" name="platform/build/blueprint" groups="pdk,tradefed" />
  <project path="build/pesto" name="platform/build/pesto" groups="pdk" />
  <project path="build/release" name="platform/build/release" groups="pdk,tradefed,sysui-studio" />
  <project path="build/soong" name="platform/build/soong" groups="pdk,tradefed,sysui-studio" >
    <linkfile src="root.bp" dest="Android.bp" />
    <linkfile src="bootstrap.bash" dest="bootstrap.bash" />
  </project>
  <project path="art" name="platform/art" groups="pdk" />
  <project path="bionic" name="platform/bionic" groups="pdk" />
  <project path="bootable/deprecated-ota" name="platform/bootable/deprecated-ota" groups="pdk-cw,pdk-arsp-ag-sot,pdk-arsp-common" />
  <project path="bootable/recovery" name="platform/bootable/recovery" groups="pdk" />
  <project path="bootable/libbootloader" name="platform/bootable/libbootloader" groups="vts,pdk" />
  <project path="cts" name="platform/cts" groups="cts,pdk-cw-fs,pdk-fs" />
  <project path="dalvik" name="platform/dalvik" groups="pdk-cw-fs,pdk-fs" />
  <project path="developers/build" name="platform/developers/build" groups="developers,pdk" />
  <project path="developers/samples/android" name="platform/developers/samples/android" groups="developers" />
  <project path="development" name="platform/development" groups="developers,pdk-cw-fs,pdk-fs,sysui-studio" />
  <project path="device/amlogic/yukawa" name="device/amlogic/yukawa" groups="device,yukawa,pdk" />
:
```

### Revised Recommendations Based on Extra Notes

The Extra Notes reveal critical information that **simplifies the design significantly**:

1. **No Path Collision Risk**: Android's repo structure ensures each repository already has a unique path prefix (e.g., `platform/build`, `device/amlogic/yukawa`, `brillo/manifest`). This eliminates the namespace handling complexity identified earlier.

2. **Manifest Maps Repos Consistently**: The manifest file provides a definitive mapping from logical paths to repository names, making it straightforward to organize imports without conflicts.

3. **Revised Recommendation: Simplified Sequential Approach**

   Given the well-structured namespace, **Approach 1 (Sequential Git Fast-Export)** becomes more viable than the initially recommended Hybrid approach:
   
   **Steps:**
   1. Parse the manifest to identify all project paths and repository names
   2. For each repo, scan commits within the specified date range on named branches
   3. Build a global timeline index (metadata only: commit hash, timestamp, repo)
   4. Sort by timestamp globally and create fast-export dumps in chronological order
   5. Import each dump to P4 sequentially, preserving the manifest-defined path structure
   
   **Advantages over Hybrid:**
   - Simpler implementation (no need to merge/consolidate histories)
   - Preserves per-repo branch structure naturally
   - Leverages existing git fast-export tools
   - Android's namespace design handles path collisions automatically
   - Still imports in chronological order as required by specs
   
   **Key Implementation Detail:**
   - Use the manifest's `path` attribute as the P4 import location for each repo's files
   - This ensures consistency with Android's documented structure

## Implementation Notes

- create a basic implemenation in cmd/repo_p4transfer
- use libraries such as
    - "github.com/rcowham/kingpin"
- use the structure of cmd/gitgraph/gitgraph.go as a template for new module
- take an XML manfiest file as one of the inputs
  - test/android/manifest/default.xml
- take a suitable yaml config file for other inputs
  - the list of repos should be as per the manifest - not repeated in the yaml
  - suggested YAML config structure:
    ```yaml
    # Date range for filtering commits across all repos
    dateRange:
      startDate: "2024-01-01"
      endDate: "2024-12-31"
    
    # Branch patterns to filter (repos may have different branches)
    branches:
      - "main"
      - "develop"
      - "android*"  # glob patterns supported
    
    # P4 target configuration
    p4Target:
      client: "p4_client_name"
      depot: "//depot/android_import"
    
    # Optional: filter specific repos from manifest
    # If empty or omitted, use all repos from manifest
    selectedRepos: []
    ```
  - manifest remains source of truth for which repos exist
  - YAML config controls filtering and import destination

## Implementation Status

### Completed

**Basic implementation in `cmd/repo_p4transfer`** with the following components:

1. **Manifest Parsing**
   - Parses Android XML manifest files to discover repositories
   - Extracts project paths and repository names
   - Builds internal index for quick lookup

2. **Configuration Loading**
   - Loads YAML config files with filtering and P4 target settings
   - Validates date ranges
   - Supports branch pattern filtering with wildcard matching

3. **Timeline Building**
   - Scans all repositories for commits within specified date range
   - Extracts commit metadata (hash, timestamp, author, message, branch)
   - Sorts commits globally by timestamp to maintain causality
   - Groups commits by repository for export

4. **CLI Interface**
   - Uses kingpin for command-line argument parsing
   - Required flags: --config, --repo-root
   - Optional flags: --manifest, --log-level
   - Version support and help documentation

5. **Testing**
   - Unit tests for manifest loading
   - Unit tests for config loading and validation
   - Branch pattern matching tests
   - Commit sorting tests
   - All tests passing ✓

6. **Documentation**
   - Comprehensive README with usage examples
   - Sample configuration file
   - Makefile for building

7. **Debug Logging** ✓
   - Git command execution logging in debug mode
   - Branch discovery and filtering details
   - Per-branch commit count reporting
   - Timeline sorting and date range verification
   - Commit batching information by repo
   - Export file size and processing details
   - **Stderr logging for all git commands** (success and error)
   - Complete audit trail for troubleshooting

### Debug Logging Features

With `--log-level debug`, the following information is output:

**Git Command Execution & Error Handling:**
- All git commands logged with full arguments before execution
- Stderr captured and logged even on success (DEBUG level)
- Command failures include stderr details in error message
- Provides complete visibility into git operation details

**Repository Scanning:**
- Lists each git repository being scanned (with progress counter)
- Shows git commands executed with full arguments
- Captures stderr output from git commands
- Reports number of branches discovered and filtered
- Displays branches matched against filter patterns

**Commit Extraction:**
- Logs each branch being scanned
- Reports commit count per branch
- Shows commit hash, timestamp, author during extraction
- Tracks when branches don't exist or have no commits
- Logs any warnings or errors from git log command


**Timeline Building:**
- Displays total commits collected across repos
- Shows chronological range (earliest to latest commit)
- Lists commit distribution per repository

**Batch Processing:**
- Reports grouping of commits by repository
- Shows first and last commit timestamp per repo
- Displays export file sizes
- Logs fast-export command details with commit hash count

### In Progress / TODO

- **P4 Import Logic**: Currently logs what would be imported, placeholder for actual P4 client integration
- **Fast-Export Generation**: Framework in place, needs full git fast-export command refinement for multi-branch scenarios
- **Progress Tracking**: Add checkpoints for resumable imports on failure
- **Verification**: Add pre/post import verification to detect data loss
- **Error Recovery**: Implement retry logic and proper error reporting
- **Performance**: Optimize for large repositories (hundreds of repos with thousands of commits)

### Building and Testing

```bash
cd cmd/repo_p4transfer
make              # Build binary
go test -v        # Run all tests
./repo_p4transfer --help  # Show help
```

### Sample Usage

```bash
./repo_p4transfer \
  --manifest ../../test/android/manifest/default.xml \
  --config sample_config.yaml \
  --repo-root /path/to/android/repos \
  --log-level debug
```

### Sample Debug Output

When running with `--log-level debug`, output includes detailed information about command execution, stderr logging, and commit batching:

```
Building timeline of commits across all repos...
DEBUG Scanning 500 selected repos
DEBUG [1/500] Scanning repo: platform/build
DEBUG [platform/build] Executing: git branch -r
DEBUG [platform/build] Command stderr: (empty or warnings)
DEBUG [platform/build] Found 12 branches
DEBUG Filtered 12 branches to 3 matching branches
DEBUG [1/3] Scanning branch: main
DEBUG [platform/build] Executing: git log --since=2024-01-01 --until=2024-12-31 --format=%H|%aI|%an|%s main
DEBUG [platform/build] Command stderr: (empty or warnings)
DEBUG [platform/build:main] Found 45 commits
DEBUG [2/3] Scanning branch: develop
DEBUG [platform/build] Executing: git log --since=2024-01-01 --until=2024-12-31 --format=%H|%aI|%an|%s develop
DEBUG [platform/build:develop] Found 23 commits

DEBUG Sorting 1247 total commits chronologically
DEBUG Timeline range: 2024-01-15 09:23:45 to 2024-12-28 16:42:18
INFO Found 1247 commits across all repos in date range

Starting export and import to P4...
INFO Target P4 depot: //depot/android_import
DEBUG Grouping 1247 commits into 45 repos for batched export
INFO Processing repo: platform/build (68 commits)
DEBUG   Repo path: platform/build
DEBUG   Full path: /path/to/repos/platform/build
DEBUG   First commit: a1b2c3d4 at 2024-01-15 09:23:45
DEBUG   Last commit: x9y8z7w6 at 2024-12-28 14:22:18
DEBUG Repo 'platform/build': 68 commits
DEBUG [platform/build] Executing: git fast-export --all <hash1> <hash2> ... (68 hashes)
DEBUG [platform/build] Command stderr: (empty or warnings)
DEBUG [platform/build] Fast-export output size: 2847362 bytes
DEBUG [platform/build] Export file written to: /tmp/export_platform_build.txt
INFO Would import /tmp/export_platform_build.txt to P4 depot //depot/android_import at path platform/build
```

**Stderr Logging Examples:**

If git commands produce warnings or errors, they are logged:
```
DEBUG [platform/repo] Command stderr: warning: refname 'refs/remotes/origin/X' is ambiguous
WARN [platform/repo] Command stderr: error: pathspec 'nonexistent' did not match any files
ERROR [platform/repo] git command failed: exit status 128 (stderr: fatal: Not a git repository)
```


DEBUG [1/3] Scanning branch: main
DEBUG [platform/build] Executing: git log --since=2024-01-01 --until=2024-12-31 --format=%H|%aI|%an|%s main
DEBUG [platform/build:main] Found 45 commits

DEBUG Sorting 1247 total commits chronologically
DEBUG Timeline range: 2024-01-15 09:23:45 to 2024-12-28 16:42:18
INFO Found 1247 commits across all repos in date range

Starting export and import to P4...
INFO Target P4 depot: //depot/android_import
DEBUG Grouping 1247 commits into 45 repos for batched export
INFO Processing repo: platform/build (68 commits)
DEBUG   Repo path: platform/build
DEBUG   Full path: /path/to/repos/platform/build
DEBUG   First commit: a1b2c3d4 at 2024-01-15 09:23:45
DEBUG   Last commit: x9y8z7w6 at 2024-12-28 14:22:18
DEBUG Repo 'platform/build': 68 commits
DEBUG [platform/build] Executing: git fast-export --all <hash1> <hash2> ... (with 68 commit hashes)
DEBUG [platform/build] Fast-export output size: 2847362 bytes
DEBUG [platform/build] Export file written to: /tmp/export_platform_build.txt
INFO Would import /tmp/export_platform_build.txt to P4 depot //depot/android_import at path platform/build
```

### Key Design Features Implemented

1. **Manifest as Source of Truth**: Repos defined in manifest, not duplicated in config
2. **Chronological Ordering**: All commits ordered by timestamp across repos
3. **Branch Filtering**: Glob pattern support (*, main, android*-release)
4. **Date Range Filtering**: Inclusive start/end date filtering
5. **Concurrent Scanning**: Framework ready for parallel repo scanning (future optimization)
6. **Extensible Architecture**: Uses interfaces for easy addition of P4 client logic
7. **Comprehensive Debug Logging**: Full command execution trace and batch composition details

## Redesign Thoughts

Consider re-implmenting gitp4transfer to:

* process clean git repos - so don't need all the workarounds required to process PlastSCM git fast exports
* pros and cons for reading git repos commit by commit rather than processing git fast export files
* utilize similar mechanisms as currently to create a p4 journal and to write archive files and gzip them in parallel
* can these be extracted quickly and easily from the .git structure itself?

### Responses

#### Process Clean Git Repositories

For `repo_p4transfer` inputs, a clean-repository import path is preferable to accepting a general fast-export stream. Git already guarantees commit, tree, blob, rename, parent, and merge object relationships, eliminating much of the defensive handling needed for malformed PlasticSCM exports. The existing fast-export mode should remain supported for PlasticSCM and other external sources, but it should not define the architecture for native Git imports.

The direct-reader path must still handle Git-specific edge cases: shallow or partial clones, missing promisor objects, symbolic refs from `repo`, annotated tags, merges, and multiple branches containing the same commit. These are source-validation concerns rather than malformed-input recovery.

#### Commit-by-Commit Reading Versus Fast Export

Reading Git commits directly is the better fit when the importer must preserve one global chronological sequence across many repositories. It lets the importer enumerate only selected commits and read each commit's tree changes when that commit is ready to become a Perforce changelist. It also avoids creating thousands of intermediate export files and parsing a text protocol that Git has already parsed internally.

Advantages of direct Git access:

* Lower peak memory: process a commit and release its tree/blob data rather than hold an entire fast-export batch in memory.
* Direct access to commit parents, author/committer timestamps, tree IDs, and file modes.
* No mark-collision or cross-batch mark-remapping layer.
* Easier resume/checkpoint logic based on Git object IDs and completed Perforce changelists.
* Exact use of the manifest path while constructing the Perforce depot path.

Disadvantages:

* Requires a Git object reader or carefully controlled Git plumbing calls.
* Rename and copy detection is not intrinsic to commit trees; it must be computed from parent and child trees when Perforce integration history is required.
* Partial clones must either be hydrated before import or rejected with a clear missing-object error.
* Fast-export remains useful as a stable interchange format and for existing PlasticSCM workflows.

Recommendation: retain `gitp4transfer`'s fast-export reader as a compatibility mode, and introduce a separate native-Git import mode. Avoid converting native Git repositories to fast-export merely to parse them again.

#### Reuse Journal and Archive Writers

The Perforce-specific parts should be retained. Once a native Git reader produces an internal commit model equivalent to `GitCommit` and `GitFile`, the existing journal record generation, Perforce path mapping, typemap selection, archive layout, gzip work pool, and verification workflow can be reused. This isolates the change to the source-reader layer.

The internal model should retain only the metadata needed after a commit is written: object ID, parent IDs, branch context, file action, depot path, file type, and archive source. Blob bytes should be streamed to the archive writer and released immediately. Archive worker concurrency should be bounded independently of CPU count to control in-flight blob memory.

#### Reading the .git Object Database

The data can be extracted directly from `.git`, but a Git library is preferable to ad hoc pack-file parsing. A mature Go library such as `github.com/go-git/go-git/v5` can read commits, trees, and blobs from ordinary repositories. It may be insufficient for `repo` partial clones and promisor-object behavior, so native Git plumbing is the safer initial implementation:

* `git rev-list --topo-order` or `git log` for selected commit IDs and timestamps.
* `git diff-tree --no-commit-id --name-status -r <parent> <commit>` for file actions.
* `git cat-file --batch` or `git cat-file --batch-command` for streamed blob content.
* `git ls-tree` when full tree traversal is needed for root commits.

Use `git cat-file --batch-command` with persistent subprocesses rather than one subprocess per blob. Before starting an import, check for missing objects with a bounded validation pass and either hydrate the partial clone explicitly or fail before writing a partial Perforce journal.

#### Suggested Migration

1. Define a source-neutral internal commit/file model consumed by the existing journal and archive layers.
2. Extract the current fast-export parsing into a `FastExportSource` implementation without changing its behaviour.
3. Add a `GitRepositorySource` that enumerates one repository's selected commits and streams blob content through `git cat-file`.
4. Drive both sources from the existing globally sorted manifest timeline, preserving the batch summary and checkpoint concepts.
5. Validate a small Android manifest import against the current fast-export route before making native Git the default for `repo_p4transfer`.

### More thoughts

Is it easier to create a copy of main.go as a library which is then simplified for use by repo_p4transfer? This has some code duplication but we can remove the Plastic requirements

We can then look to, in a future phase, use this library with an alternative standalone copy of gitp4transfer

The `GitRepositorySource` that enumerates a batch file for one repository's selected commits and streams blob content through `git cat-file`.

The blob streaming should, as currently done for gitp4transfer, allow for:

* parallel processing
* use a single copy of each distinct blob - renaming to the first p4 file
* insert lazy copy references as currently

#### Response: Shared Importer Library

Do not create a second copy of `main.go`. It would permit removing PlasticSCM-specific workarounds initially, but fixes to Perforce journal generation, typemap handling, archive layout, duplicate blobs, and integration records would then need to be maintained in two implementations. The longer-term cost exceeds the short-term simplification.

Instead, extract the source-independent Perforce import logic into an `internal/importer` package. Keep the two command paths thin:

```text
internal/importer/
  model.go          source-neutral commit, file action, and blob interfaces
  p4writer.go       journal generation and depot revision state
  archive.go        archive layout, compression, duplicate and lazy references
  fast_export.go    FastExportSource adapter

cmd/gitp4transfer/
  main.go           fast-export command-line interface

cmd/repo_p4transfer/
  repo_p4transfer.go manifest, ref selection, and global batch planning
  git_source.go     GitRepositorySource adapter
```

The existing `gitp4transfer` remains a standalone application that passes `FastExportSource` records to the shared importer. `repo_p4transfer` uses the same importer with `GitRepositorySource`, avoiding fast-export files for native Android Git repositories. This preserves fast-export compatibility for PlasticSCM while allowing the native Git path to exclude its input-recovery code.

#### GitRepositorySource Scope

`GitRepositorySource` should consume one planned repository batch, rather than a generated batch file. It must:

- Read commits in the batch's existing chronological order.
- Read parent and merge relationships needed for Perforce branch and integration records.
- Generate file additions, edits, deletes, renames, and copies from commit tree differences.
- Prefix each source file with the manifest project path before Perforce depot mapping.
- Provide a content reader for each blob, identified by its Git object ID.

The source should use persistent Git plumbing processes per repository. `git cat-file --batch-command` is preferred for content because it avoids a process per blob and allows bytes to be copied directly to archive workers. `git diff-tree` and `git ls-tree` provide change and root-commit tree information. The source should fail before journal output if a required object is missing from a partial clone.

#### Blob and Archive Processing

The shared archive layer must retain the useful characteristics of the current importer:

- Submit compression/archive creation to a bounded worker pool.
- Key the blob cache by Git object ID, not fast-export mark.
- Write one archive for a distinct blob and create Perforce lazy-copy/librarian references for later uses.
- Release the blob byte stream as soon as the archive worker has consumed it.
- Keep only small metadata for completed blobs and commits, with a configurable cap for in-flight blob work.

This design avoids keeping a whole fast-export batch or a whole blob collection in memory, while preserving the existing duplicate-archive and lazy-reference behavior.

#### Implementation Steps

1. Create `internal/importer/model.go` with the source-neutral commit, file-action, identity, and blob-content interfaces. Add unit tests for the model only.
2. Move journal writing, depot revision state, typemap selection, and archive layout from `main.go` into `internal/importer`, retaining existing tests and behavior. Leave `gitp4transfer` as an adapter over the new package.
3. Move the already-created `FastExportSource` into `internal/importer/fast_export.go`; make the standalone command construct this source and invoke the shared import pipeline.
4. Add a `GitRepositorySource` prototype that imports a single linear repository with modify/add/delete actions. Use `git diff-tree` and `git cat-file --batch-command`; defer rename/copy inference initially.
5. Add manifest-path prefixing and verify that two repositories containing the same relative file path import into separate Perforce depot paths.
6. Add Git object-ID duplicate detection and route duplicate blobs through the existing lazy-copy archive logic.
7. Add rename, copy, branch, and merge inference with focused fixtures that compare journal output to the current fast-export path.
8. Add partial-clone validation, bounded parallelism, cancellation, and checkpoint/resume state before enabling native Git import for large Android manifests.
9. Validate a small Android manifest with both import paths, compare Perforce file content, revisions, and changelist order, then make native Git the default for `repo_p4transfer`.

