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
repo_p4transfer --manifest <manifest.xml> --config <config.yaml> --repo-root <path/to/repos> [--log-level debug]
```

### Required Flags

- `--manifest`: Path to Android manifest XML file (default: manifest.xml)
- `--config`: Path to configuration YAML file (required)
- `--repo-root`: Root directory containing all git repositories (required)

### Optional Flags

- `--log-level`: Logging level: debug, info, warn, error (default: info)

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

- Implement actual P4 import logic (currently logs what would be imported)
- Add dry-run mode to preview without actual P4 changes
- Add resumption logic for interrupted imports
- Add progress tracking and statistics collection
- Implement proper error recovery and retry logic
- Add verification step to compare before/after imports
