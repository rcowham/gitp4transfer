# Git Fast-Export Flags Implementation Guide

## Overview

The `repo_p4transfer` tool now supports configurable git fast-export flags to handle edge cases when exporting repositories. This guide explains how to use these flags for different scenarios.

## Implemented Flags

### 1. `--tag-of-filtered-object`

Controls how to handle tags that point to commits outside the export scope (e.g., commits before the `--since` date).

**Configuration Key**: `git_export.tag_of_filtered_object`

**Valid Values**:
- `abort` - Stop if any tag points to a filtered object
- `drop` - Remove tags that point to filtered objects ⭐ **RECOMMENDED**
- `rewrite` - Rewrite tags to dereference commits

#### When to Use Each Mode

**`drop` (Recommended for date-filtered imports)**:
```yaml
date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"

git_export:
  tag_of_filtered_object: "drop"
```

**Use Case**: When importing Android repos with a specific date range, older tags from 2023 would automatically be dropped, allowing the import to proceed cleanly.

**Example Behavior**:
- Repo has tags: `v1.0` (2023-06-01), `v1.1` (2024-02-15), `v1.2` (2025-01-01)
- Date range: 2024-01-01 to 2024-12-31
- Result: `v1.0` (dropped - before date), `v1.1` (kept), `v1.2` (dropped - after date)

---

**`abort` (Strict validation)**:
```yaml
git_export:
  tag_of_filtered_object: "abort"
```

**Use Case**: Quality assurance, when you want to catch incomplete data before import.

**Example Behavior**:
- Same scenario as above → Export STOPS with error
- Forces manual review and explicit tag handling

---

**`rewrite` (Advanced, complex behavior)**:
```yaml
git_export:
  tag_of_filtered_object: "rewrite"
```

**Use Case**: Preserving tag intent when commits exist in export range.

**Example Behavior**:
- Tag pointing to commit outside range → Rewrite to point to nearest valid commit
- May create confusing tag-to-commit mappings

---

### 2. `--signed-tags`

Controls how to handle GPG-signed tags in the export.

**Configuration Key**: `git_export.signed_tags`

**Valid Values**:
- `verbatim` - Keep signatures exactly as they are
- `warn-verbatim` - Log warnings if signatures are invalid, keep tags
- `warn-strip` - Log warnings and strip signatures ⭐ **RECOMMENDED**
- `strip` - Remove signatures silently
- `abort` - Stop if any signed tags exist

#### When to Use Each Mode

**`warn-strip` (Recommended for P4 import)**:
```yaml
git_export:
  signed_tags: "warn-strip"
```

**Use Case**: **DEFAULT for Android repos**

**Rationale**:
- Perforce doesn't natively support GPG signatures
- Provides visibility into what's being stripped (warnings in logs)
- Ensures clean P4 import without signature-related failures
- Debuggable: check logs to see which tags were processed

**Example Output**:
```
DEBUG [platform/build] Command stderr: warning: tag 'v1.0' is signed but signature is not valid
WARN [platform/build] Command stderr: warning: tag 'v1.1' has PGP signature
DEBUG [platform/build] Using --signed-tags=warn-strip
```

---

**`strip` (Production mode)**:
```yaml
git_export:
  signed_tags: "strip"
```

**Use Case**: High-volume imports, no need for signature visibility

**Difference from `warn-strip`**:
- No warnings logged
- Quieter output
- Less debuggable if issues arise

---

**`verbatim` (Preserve signatures)**:
```yaml
git_export:
  signed_tags: "verbatim"
```

**Use Case**: Rare - when signature preservation is critical

**Problems**:
- P4 doesn't display or verify signatures
- Signatures may become invalid after import (commits change)
- Can cause export failures if signatures are invalid

---

**`abort` (Strict validation)**:
```yaml
git_export:
  signed_tags: "abort"
```

**Use Case**: Catch all signed tags for manual review

**Effect**: Export stops if ANY signed tags found → requires explicit handling

---

### 3. `--use-done-feature`

Enables done marker for resumable exports (advanced feature).

**Configuration Key**: `git_export.use_done_feature`

**Valid Values**:
- `true` - Enable resumable exports
- `false` - Standard mode (default)

**Use Case**: Large exports that might timeout or be interrupted

```yaml
git_export:
  use_done_feature: true
```

**Benefit**: If export fails midway, can resume from checkpoint instead of restarting

---

## Configuration Examples

### Example 1: Date-Filtered Import (Android Release)

```yaml
date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"

branches:
  - "main"
  - "android14-release"

git_export:
  tag_of_filtered_object: "drop"  # Ignore tags outside date range
  signed_tags: "warn-strip"        # Strip signatures, log warnings
  use_done_feature: false
```

**Expected Behavior**:
- Tags from 2023 and 2025 are dropped
- Signed tags (usually release tags) have signatures removed
- Warnings logged for all signature stripping
- Clean export with no tag-related failures

---

### Example 2: Full History (No Filtering)

```yaml
date_range:
  start_date: "2000-01-01"
  end_date: "2099-12-31"

branches:
  - "*"  # All branches

git_export:
  tag_of_filtered_object: "verbatim"  # Keep all tags
  signed_tags: "warn-strip"            # Still strip signatures for P4
  use_done_feature: false
```

**Expected Behavior**:
- All historical tags preserved
- Signatures still stripped (P4 doesn't support them)
- Complete history imported to P4

---

### Example 3: Strict Validation

```yaml
git_export:
  tag_of_filtered_object: "abort"    # Stop on any filtered tags
  signed_tags: "abort"                # Stop on any signed tags
  use_done_feature: false
```

**Use Case**: Testing, validation, or when manual review is required

**Expected Behavior**:
- Export stops immediately on issues
- Forces explicit handling of problematic tags

---

### Example 4: Production Large-Scale Import

```yaml
date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"

git_export:
  tag_of_filtered_object: "drop"      # Clean handling
  signed_tags: "strip"                 # Silent stripping, no warnings
  use_done_feature: true               # Resumable for large repos
```

**Use Case**: High-volume Android import

**Expected Behavior**:
- Minimal logging overhead
- Resumable if interrupted
- Clean, efficient export

---

## Validation

The tool validates git_export options during config loading:

```
✓ tag_of_filtered_object must be: abort, drop, or rewrite
✓ signed_tags must be: verbatim, warn-verbatim, warn-strip, strip, or abort
✗ Invalid values cause immediate error with helpful message
```

## Debug Mode Output

With `--log-level debug`, you'll see:

```
DEBUG Git export tag_of_filtered_object: drop
DEBUG Git export signed_tags: warn-strip
DEBUG [platform/build] Using --tag-of-filtered-object=drop
DEBUG [platform/build] Using --signed-tags=warn-strip
DEBUG [platform/build] Executing: git fast-export --tag-of-filtered-object=drop --signed-tags=warn-strip --all <hashes>
DEBUG [platform/build] Command stderr: warning: tag 'v2.3' has PGP signature
WARN [platform/build] Command stderr: warning: tag 'v2.3' has PGP signature
```

## Migration Guide

### From Older Configuration

If your config doesn't have `git_export` section, add:

```yaml
git_export:
  tag_of_filtered_object: "drop"
  signed_tags: "warn-strip"
  use_done_feature: false
```

### Default Behavior

If `git_export` section is omitted, defaults are:
- `tag_of_filtered_object`: (empty - no flag applied)
- `signed_tags`: (empty - no flag applied)
- `use_done_feature`: false

To use defaults explicitly:

```yaml
git_export:
  tag_of_filtered_object: ""
  signed_tags: ""
  use_done_feature: false
```

## Testing Your Configuration

1. **Dry run with small repo**:
   ```bash
   ./repo_p4transfer \
     --manifest test_manifest.xml \
     --config sample_config.yaml \
     --repo-root /test/repos \
     --log-level debug
   ```

2. **Check debug output for**:
   - `Using --tag-of-filtered-object=...`
   - `Using --signed-tags=...`
   - Any warnings from git commands

3. **Verify tag handling**:
   - Count tags before export
   - Check export output for tag count
   - Confirm expected tags are present/absent

## Troubleshooting

### Tags Missing After Import
**Likely Cause**: `tag_of_filtered_object: drop` removing tags outside date range
**Solution**: Check date range, use `rewrite` mode, or extend date range

### Export Fails with Tag Errors
**Likely Cause**: `tag_of_filtered_object: abort` on filtered tags
**Solution**: Change to `drop` or `rewrite`, or adjust date range

### Signature Verification Fails
**Likely Cause**: Signatures present but P4 doesn't verify them
**Solution**: Use `warn-strip` or `strip` (recommended)

### Export Timeout on Large Repos
**Solution**: Enable `use_done_feature: true` for resumable exports

## Related Documentation

- [README.md](README.md) - Full tool documentation
- [GIT_FASTEXPORT_FLAGS.md](GIT_FASTEXPORT_FLAGS.md) - Technical details
- [sample_config.yaml](sample_config.yaml) - Configuration reference

## Summary

| Scenario | Recommended Config |
|----------|-------------------|
| Date-filtered Android import | `tag_of_filtered_object: drop`, `signed_tags: warn-strip` |
| Full history | `tag_of_filtered_object: verbatim`, `signed_tags: warn-strip` |
| Strict validation | `tag_of_filtered_object: abort`, `signed_tags: abort` |
| Production large-scale | `tag_of_filtered_object: drop`, `signed_tags: strip`, `use_done_feature: true` |
| Testing/debugging | Use `warn-strip` for visibility into what's being processed |
