# repo_p4transfer

Transfer Android source repositories managed by the "repo" tool to Perforce using git fast-export.

## Overview

This tool processes multiple git repositories defined in an Android manifest file and imports them to Perforce in chronological order. It:

1. Parses an XML manifest file to discover repositories
2. Scans all repositories for commits within a date range
3. Builds a chronological timeline across all repos
4. Creates git fast-export dumps for each repository
5. Imports dumps to P4 in chronological order

## Usage

```bash
repo_p4transfer --manifest <manifest.xml> --config <config.yaml> --repo-root <path/to/repos> [--export-dir <path/to/exports>] [--log-level debug]
```

### Required Flags

- `--manifest`: Path to Android manifest XML file (default: manifest.xml)
- `--config`: Path to configuration YAML file (required)
- `--repo-root`: Root directory containing all git repositories (required)

### Optional Flags

- `-C`, `--sample-config`: Print a sample config YAML file to stdout and exit (e.g. `repo_p4transfer -C > config.yaml`)
- `--export-dir`: Directory where to store git fast-export dump files (default: system temp directory)
- `--batch-map`: Output YAML file path for batch to repo path mapping
- `--dummy`: Write placeholder text for blob contents in export files (for quick testing)
- `--dry-run`: Write `batch_summary.tsv` and `batch_mapping.yaml` without creating fast-export files
- `--native-git`: Import directly from Git objects; requires `--archive-root` and writes `--journal`
- `--journal`: Perforce journal path for `--native-git` (default: `jnl.0`)
- `--archive-root`: Perforce archive root for `--native-git`
- `--parallel-workers`: Bounded archive worker count for `--native-git` (default: 1)
- `--progress-commits`: Log native import progress every N processed commits (default: 100; 0 disables periodic progress)
- `--first-parent`: Follow only the first parent commit when scanning git history
- `--fresh-start`: Ignore any existing `--native-git` checkpoint and start the import from scratch
- `--log-level`: Logging level: debug, info, warn, error (default: info)

Native imports also log progress at every batch boundary. Progress messages include completed work, throughput, elapsed time, and an ETA calculated from the observed processing rate.

### Resuming a `--native-git` import after a crash

After each batch fully completes, `--native-git` writes a checkpoint file
(`<journal>.checkpoint.json`) recording the next batch to process, the
journal's byte offset, and the P4 revision/dedup state as of that point.
Re-running the same command automatically resumes from that checkpoint: the
journal is truncated back to the recorded offset (discarding any
partially-written in-progress batch) and reopened for append, so no manual
cleanup is required. On full completion the checkpoint file is removed.

Resume granularity is per-batch, not per-commit: if a batch was interrupted
partway through, it is redone from its start on resume (this can mean
re-processing a large batch, e.g. thousands of commits for one repository, but
never corrupts or duplicates already-completed batches). Use `--fresh-start`
to discard an existing checkpoint and re-import from the beginning.

### Stopping a `--native-git` import cleanly

Press Ctrl-C (or send `SIGTERM`) once to stop after the currently in-flight
batch finishes, rather than killing the process mid-batch: the checkpoint is
saved as usual, so re-running the command resumes exactly where it left off.
A second Ctrl-C/`SIGTERM` forces an immediate exit.

## Debug Mode

When running with `--log-level debug`, the tool provides detailed output about the transfer process:

### Git Command Logging

All git commands are executed with both stdout and stderr captured:

**Successful Commands:**

```
DEBUG [repo/path] Executing: git branch -r
DEBUG [repo/path] Executing: git log --since=2024-01-01 --until=2024-12-31 --format=%H|%aI|%an|%s main
DEBUG [repo/path] Executing: git fast-export --all <hashes>
```

**Commands with Warnings (stderr logged at WARN level):**

```
WARN [repo/path] Command stderr: warning: refname '...' is ambiguous
WARN [repo/path] Command stderr: hint: The following pathnames are ambiguous...
```

**Commands that Fail (stderr logged with error details):**
```
ERROR [repo/path] Command failed: exit status 128 (stderr: fatal: not a git repository)
```

### Stderr Handling

- **Warnings**: stderr output is logged at DEBUG level even when the command succeeds
- **Failures**: stderr is included in error messages for troubleshooting
- **Errors**: Failed commands show both the error code and stderr content

This ensures you have full visibility into what git is reporting, even for non-fatal issues.

### Command Execution Output

All git commands executed are logged with their full arguments:
```
[repo/path] Executing: git branch -r
[repo/path] Executing: git log --since=2024-01-01 --until=2024-12-31 --format=%H|%aI|%an|%s main
[repo/path] Executing: git fast-export --all <commit-hashes> (with N commit hashes)
```

### Commit Timeline Building

Detailed progress during timeline construction:
```
[1/500] Scanning repo: platform/build
  [repo] Executing: git branch -r
  [repo] Found 12 branches
  Filtered 12 branches to 3 matching branches
  [1/3] Scanning branch: main
  [repo:main] Found 45 commits
  [2/3] Scanning branch: develop
  [repo:develop] Found 23 commits
Sorting 68 total commits chronologically
Timeline range: 2024-01-15 09:23:45 to 2024-12-28 16:42:18
```

### Batch Processing Information

When exporting and importing, commits across repositories are grouped into contiguous time-ordered batches to preserve global chronological order in Perforce:

```
Grouped 500 commits into 12 contiguous time-ordered batches across repos
[1/12] Processing batch for repo: platform/build (45 commits)
  Repo path: platform/build
  Full path: /path/to/repos/platform/build
  First commit: a1b2c3d4 at 2024-01-01 10:30:15
  Last commit: x9y8z7w6 at 2024-01-05 14:22:45
  [platform/build] Exporting revision specs: a1b2c3d4^..x9y8z7w6
  [platform/build] Using --export-marks=marks_platform_build.txt
[2/12] Processing batch for repo: platform/art (30 commits)
  ...
[3/12] Processing batch for repo: platform/build (15 commits)
  [platform/build] Using --import-marks=marks_platform_build.txt
  [platform/build] Using --export-marks=marks_platform_build.txt
```

### Example Debug Run

```bash
./repo_p4transfer \
  --manifest ../../test/android/manifest/default.xml \
  --config sample_config.yaml \
  --repo-root /path/to/android/repos \
  --log-level debug
```

Expected output:
- Repository discovery and branch enumeration
- Per-branch commit counts and filtering
- Global timeline sort verification
- Batch composition and export process
- Per-repository commit ranges and statistics


## Configuration

The YAML config file controls filtering and import settings:

```yaml
# Date range for filtering commits
date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"

# Branch patterns to include (* wildcard supported)
branches:
  - "main"
  - "android*-release"

# P4 target configuration
import_depot: "//depot/android_import"
import_path: "android"
default_branch: "main"

# Optional: appended to every changelist description (--native-git only).
# Go text/template syntax; available variables: repo_path, commitid, user, email.
changelist_description_template: |
  Imported from {{.repo_path}} at Git commit {{.commitid}} by {{.email}}

# Optional: filter specific repos from manifest
# If empty, all repos from manifest are used
selected_repos: []
```

## Key Design Decisions

1. **Manifest is Source of Truth**: The XML manifest file defines which repositories are available. The YAML config does not repeat repo names, only filtering criteria.

2. **Chronological Ordering**: Commits are sorted by timestamp across all repositories and imported in order to maintain causality.

3. **Per-Repo Processing**: While commits are imported in global chronological order, each repository's history is preserved using git fast-export.

4. **Path Collision Avoidance**: Android's manifest paths (e.g., `platform/build`, `device/amlogic/yukawa`) are naturally unique and serve as P4 import paths.

## Implementation Details

### Types

- `RepoManifest`: Parsed XML structure from manifest file
- `ManifestProject`: Single repository definition with path and name
- `RepoP4TransferConfig`: Extended configuration with date range and branch filters
- `CommitMetadata`: Minimal commit info for timeline building
- `RepoP4Transfer`: Main coordinator orchestrating the transfer

### Main Steps

1. **Init()**: Load manifest and config, parse date ranges
2. **BuildTimeline()**: Scan all repos, extract commits in date range, sort chronologically
3. **ExportAndImport()**: Group commits by repo, create fast-export dumps, import to P4
4. **PrintStats()**: Display summary statistics

## Building

```bash
cd cmd/repo_p4transfer
make
```

## Testing

```bash
# With test manifest and repos
./repo_p4transfer --manifest ../../test/android/manifest/default.xml \
  --config sample_config.yaml \
  --repo-root /path/to/android/repos \
  --log-level debug
```

## TODO

- Add dry-run mode to preview without actual P4 changes
- Add resumption logic for interrupted imports
- Add progress tracking and statistics collection
- Implement proper error recovery and retry logic
- Add verification step to compare before/after imports
- Add incremental writing of map files
- Add pipelining of the work done by gitp4transfer with option to remove export files (save disk space) - in parallel?


## Initializing using Android repo tool

```

  706  2026/08/27 UTC 17:20:22 repo init --no-partial-clone -b android-latest-release -u https://android.googlesource.com/platform/manifest
  707  2026/08/27 UTC 17:21:02 cp ../test_manifest.xml .repo/manifests/
  708  2026/08/27 UTC 17:21:19 history | grep repo
  709  2026/08/27 UTC 17:21:29 history | grep 'repo '
  710  2026/08/27 UTC 17:21:37 repo init -m test_manifest.xml
  711  2026/08/27 UTC 17:21:58 nohup repo sync -c -j8 >/tmp/s.out &
```