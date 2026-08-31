# Git Fast-Export Flags Analysis

## Overview

The `git fast-export` command supports several flags to handle edge cases when exporting repositories. For the repo_p4transfer tool, we need to consider two important categories:

1. **Tagged Object Handling**: `--tag-of-filtered-object`
2. **Signed Tag Handling**: `--signed-tags`

## Flag: --tag-of-filtered-object

### Purpose
Determines how to handle tags that point to objects outside the export scope (e.g., commits before the `--since` date).

### Options

#### 1. `abort` (Strict Mode)
- **Behavior**: Stop immediately if a tag points to a filtered object
- **Use Case**: Quality assurance, strict validation
- **Pros**: 
  - Catches incomplete histories
  - Forces explicit tag handling
- **Cons**: 
  - Can block entire transfer for a single problematic tag
  - Difficult for large Android repos with many tags

#### 2. `drop` (Permissive Mode)
- **Behavior**: Remove tags that point to filtered objects
- **Use Case**: **RECOMMENDED for Android repos** with date range filtering
- **Pros**:
  - Allows transfer to proceed
  - Clean output without broken references
  - Useful when full history isn't available
- **Cons**:
  - Loses tag information
  - Users won't see tags outside the date range
- **Android Context**: When importing only 2024 commits, older tags from 2023 would be dropped

#### 3. `rewrite` (Smart Mode)
- **Behavior**: Rewrite tags to dereference, pointing to the commit instead of intermediate tags
- **Use Case**: Preserving tag intent when commit exists in export
- **Pros**:
  - Preserves as much tag information as possible
  - Can salvage tags by rewriting them to valid commits
- **Cons**:
  - Complex behavior, hard to predict results
  - May create confusing tag-to-commit mappings
  - Requires understanding of tag structure in repos

### Recommendation for repo_p4transfer

**Use `--tag-of-filtered-object=drop`** for the following reasons:

1. **Practical for Android repos**: When filtering by date range, many historical tags will be outside scope
2. **Consistent behavior**: Predictable outcomes across all repositories
3. **Clean P4 import**: Avoids broken tag references in Perforce
4. **Configuration option**: Should be configurable via YAML:
   ```yaml
   git_export:
     tag_of_filtered_object: "drop"  # abort | drop | rewrite
   ```

---

## Flag: --signed-tags

### Purpose
Determines how to handle GPG-signed tags when exporting.

### Options

#### 1. `verbatim` (Preserve Signatures)
- **Behavior**: Export signed tags exactly as they are
- **Use Case**: Cryptographic verification critical
- **Pros**:
  - Preserves GPG signatures for verification
  - Maintains authenticity record
- **Cons**:
  - P4 doesn't natively support GPG signatures
  - Signature verification lost in P4
  - Import may fail if signatures become invalid

#### 2. `warn-verbatim` (Warn but Preserve)
- **Behavior**: Log warnings if signatures are invalid, but export anyway
- **Use Case**: Debugging signature issues
- **Pros**:
  - Detects invalid signatures during export
  - Provides visibility into problems
- **Cons**:
  - Same P4 incompatibility as `verbatim`
  - May have unresolved signature issues

#### 3. `warn-strip` (Warn and Remove) ⭐
- **Behavior**: Log warnings and strip signatures before export
- **Use Case**: **RECOMMENDED for repo_p4transfer**
- **Pros**:
  - Detects signature issues in log
  - Cleans data for P4 import
  - P4-compatible output
  - Debuggable: warnings tell you what was stripped
- **Cons**:
  - Loses GPG signature information
  - More verbose logging

#### 4. `strip` (Silent Strip)
- **Behavior**: Remove signatures silently without warnings
- **Use Case**: Production mode, high-volume exports
- **Pros**:
  - Clean, efficient export
  - No noise in logs
  - P4-compatible
- **Cons**:
  - No visibility into signature issues
  - Could hide problems

#### 5. `abort` (Strict Mode)
- **Behavior**: Stop if any signed tags are encountered
- **Use Case**: Strict validation scenarios
- **Pros**:
  - Ensures manual review of all signed tags
- **Cons**:
  - Blocks transfer
  - Impractical for large repos like Android

### Recommendation for repo_p4transfer

**Use `--signed-tags=warn-strip`** for the following reasons:

1. **P4 Compatibility**: Perforce doesn't natively preserve GPG signatures
2. **Debuggability**: Warnings are logged so users know what happened
3. **Reliability**: Ensures export proceeds without signature-related failures
4. **Traceability**: Audit trail of stripped signatures in debug logs
5. **Configuration option**: Should be configurable via YAML:
   ```yaml
   git_export:
     signed_tags: "warn-strip"  # verbatim | warn-verbatim | warn-strip | strip | abort
   ```

---

## Implementation Strategy for repo_p4transfer

### Configuration File Extension

```yaml
# Date range for filtering commits
date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"

# Branch patterns to include
branches:
  - "main"
  - "android*-release"

# P4 target configuration
import_depot: "//depot/android_import"
import_path: "android"
default_branch: "main"

# Git fast-export options
git_export:
  # Behavior for tags pointing to filtered objects
  # Options: abort (strict), drop (permissive), rewrite (smart)
  tag_of_filtered_object: "drop"
  
  # Behavior for signed tags
  # Options: verbatim, warn-verbatim, warn-strip, strip, abort
  signed_tags: "warn-strip"
  
  # Additional export options
  use_done_feature: false  # Enable done marker for resumable exports
```

### Code Changes Required

1. **Extend RepoP4TransferConfig** to include git_export options:
   ```go
   type GitExportOptions struct {
       TagOfFilteredObject string `yaml:"tag_of_filtered_object"`
       SignedTags          string `yaml:"signed_tags"`
       UseDoneFeature      bool   `yaml:"use_done_feature"`
   }
   ```

2. **Update createFastExport()** to use these flags:
   ```go
   args := []string{"fast-export"}
   
   // Add tag handling
   if r.cfg.GitExport.TagOfFilteredObject != "" {
       args = append(args, fmt.Sprintf("--tag-of-filtered-object=%s", 
           r.cfg.GitExport.TagOfFilteredObject))
   }
   
   // Add signed tag handling
   if r.cfg.GitExport.SignedTags != "" {
       args = append(args, fmt.Sprintf("--signed-tags=%s", 
           r.cfg.GitExport.SignedTags))
   }
   
   args = append(args, "--all")
   args = append(args, commitHashes...)
   ```

3. **Add validation** during config parsing:
   ```go
   validTagModes := map[string]bool{"abort": true, "drop": true, "rewrite": true}
   validSignedModes := map[string]bool{
       "verbatim": true, "warn-verbatim": true, "warn-strip": true, 
       "strip": true, "abort": true,
   }
   
   if !validTagModes[cfg.GitExport.TagOfFilteredObject] {
       return fmt.Errorf("invalid tag_of_filtered_object: %s", 
           cfg.GitExport.TagOfFilteredObject)
   }
   ```

4. **Enhanced debug logging**:
   ```go
   r.logger.Debugf("[%s] Git export options: tag_of_filtered_object=%s, signed_tags=%s",
       repoPath, r.cfg.GitExport.TagOfFilteredObject, r.cfg.GitExport.SignedTags)
   ```

---

## Android Repository Considerations

### Tag Patterns in Android Repos

Android repos typically have:
- **Release tags**: `android-13.0.0`, `android-14.0.0`, etc.
- **Build tags**: `android-13.0.0_r1`, `android-13.0.0_r2`
- **Milestone tags**: Various development milestone tags
- **Signed tags**: Many release tags are GPG-signed for authenticity

### Scenarios

**Scenario 1: Full history import**
```yaml
git_export:
  tag_of_filtered_object: "verbatim"  # Keep all tags
  signed_tags: "warn-strip"            # Strip signatures (P4 compatible)
```

**Scenario 2: Date-filtered import** (Recommended)
```yaml
date_range:
  start_date: "2024-01-01"
  end_date: "2024-12-31"

git_export:
  tag_of_filtered_object: "drop"       # Drop tags from 2023
  signed_tags: "warn-strip"            # Strip signatures with warnings
```

**Scenario 3: Strict validation**
```yaml
git_export:
  tag_of_filtered_object: "abort"      # Catch problematic tags
  signed_tags: "abort"                 # Require manual review of signed tags
```

---

## Summary of Recommendations

| Aspect | Flag | Value | Reason |
|--------|------|-------|--------|
| **Tag Filtering** | `--tag-of-filtered-object` | `drop` | Clean P4 import; handles date-filtered repos well |
| **Signed Tags** | `--signed-tags` | `warn-strip` | P4 compatibility; debuggable via warnings |
| **Configurability** | Both flags | YAML config | Different repos may need different settings |
| **Validation** | Both flags | On config load | Catch invalid options early |
| **Logging** | Both flags | Debug level | Full audit trail for troubleshooting |

---

## Next Steps

1. Create `GitExportOptions` struct in config
2. Update sample_config.yaml with git_export section
3. Implement flag validation in loadConfig()
4. Update createFastExport() to apply flags
5. Add comprehensive debug logging
6. Update documentation with examples
7. Add tests for flag combinations
8. Test with actual Android manifest files
