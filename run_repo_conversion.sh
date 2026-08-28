#!/usr/bin/env bash
# Run a native Git Android repo import and create a usable local P4ROOT.
# Dependencies: repo_p4transfer, p4d, p4, and GNU parallel in PATH.

set -o pipefail

if [[ -z "${BASH_VERSINFO:-}" || ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "This script requires Bash version >= 4" >&2
    exit 1
fi

msg() { printf '%s\n' "$*"; }
bail() { msg "Error: ${1:-Unknown error}" >&2; exit "${2:-1}"; }
check_cmd() {
    command -v "$1" >/dev/null 2>&1 || bail "$1 could not be found in PATH"
}

usage() {
    cat <<'EOF'
USAGE:
  run_repo_conversion.sh -manifest <manifest.xml> -config <config.yaml> -repo-root <repo-root> [options]

Required:
  -manifest <file>    Android repo manifest XML file
  -config <file>      repo_p4transfer YAML configuration file
  -repo-root <dir>    Root directory of the repo workspace

Options:
  -p <P4Root>         Resulting P4ROOT; defaults to a temporary directory
  -j <workers>        Bounded native archive worker count; defaults to 1
    -progress-commits <n>
                                            Log native import progress every n commits; defaults to 100
  -dummy               Write placeholder blob data; does not read Git blob contents
  -first-parent        Follow only first-parent history when planning commits
  -unicode             Convert the restored server to Unicode mode
  -insensitive         Create a case-insensitive P4ROOT
  -sensitive           Create a case-sensitive P4ROOT
  -d                   Enable repo_p4transfer debug logging
  -h                   Show this help

The import depot is taken from the supplied repo_p4transfer YAML config.
Set REPO_P4TRANSFER_BIN to use a repo_p4transfer binary outside PATH.

Example:
  ./run_repo_conversion.sh \
    -manifest ../aosp/.repo/manifests/default.xml \
    -config cmd/repo_p4transfer/sample_config.yaml \
    -repo-root ../aosp \
    -p P4Root -j 4 -dummy
EOF
}

ManifestFile=""
ConfigFile=""
RepoRoot=""
P4Root=""
ParallelWorkers=1
ProgressCommits=100
Debug=0
Dummy=0
FirstParent=0
ConvertUnicode=0
CaseInsensitive=0
CaseSensitive=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0 ;;
        -manifest) ManifestFile=${2:-}; shift 2 ;;
        -config|-c) ConfigFile=${2:-}; shift 2 ;;
        -repo-root) RepoRoot=${2:-}; shift 2 ;;
        -p) P4Root=${2:-}; shift 2 ;;
        -j) ParallelWorkers=${2:-}; shift 2 ;;
        -progress-commits) ProgressCommits=${2:-}; shift 2 ;;
        -d) Debug=1; shift ;;
        -dummy) Dummy=1; shift ;;
        -first-parent) FirstParent=1; shift ;;
        -unicode) ConvertUnicode=1; shift ;;
        -insensitive) CaseInsensitive=1; shift ;;
        -sensitive) CaseSensitive=1; shift ;;
        *) usage; bail "Unknown option: $1" ;;
    esac
done

[[ -n "$ManifestFile" ]] || bail "-manifest is required"
[[ -n "$ConfigFile" ]] || bail "-config is required"
[[ -n "$RepoRoot" ]] || bail "-repo-root is required"
[[ -f "$ManifestFile" ]] || bail "Manifest file does not exist: $ManifestFile"
[[ -f "$ConfigFile" ]] || bail "Config file does not exist: $ConfigFile"
[[ -d "$RepoRoot" ]] || bail "Repo root does not exist: $RepoRoot"
[[ "$ParallelWorkers" =~ ^[1-9][0-9]*$ ]] || bail "-j must be a positive integer"
[[ "$ProgressCommits" =~ ^[0-9]+$ ]] || bail "-progress-commits must be a non-negative integer"
[[ $CaseInsensitive -eq 0 || $CaseSensitive -eq 0 ]] || bail "Specify only one of -insensitive or -sensitive"

RepoP4TransferBin=${REPO_P4TRANSFER_BIN:-repo_p4transfer}
check_cmd "$RepoP4TransferBin"
check_cmd p4d
check_cmd p4
check_cmd parallel

if [[ -z "$P4Root" ]]; then
    P4Root=$(mktemp -d 2>/dev/null || mktemp -d -t gitp4transfer)
fi
mkdir -p "$P4Root" || bail "Failed to create P4ROOT: $P4Root"

P4DCaseFlag=""
if [[ $CaseInsensitive -ne 0 ]]; then
    P4DCaseFlag="-C1"
elif [[ $CaseSensitive -ne 0 ]]; then
    P4DCaseFlag="-C0"
fi

NativeArgs=(
    --native-git
    --manifest "$ManifestFile"
    --config "$ConfigFile"
    --repo-root "$RepoRoot"
    --archive-root "$P4Root"
    --journal "$P4Root/jnl.0"
    --parallel-workers "$ParallelWorkers"
    --progress-commits "$ProgressCommits"
)
[[ $Debug -ne 0 ]] && NativeArgs+=(--log-level debug)
[[ $Dummy -ne 0 ]] && NativeArgs+=(--dummy)
[[ $FirstParent -ne 0 ]] && NativeArgs+=(--first-parent)

printf 'Running:'
printf ' %q' "$RepoP4TransferBin" "${NativeArgs[@]}"
printf '\n'
"$RepoP4TransferBin" "${NativeArgs[@]}" || {
    msg "Server root is in: $P4Root"
    bail "Error running repo_p4transfer native Git import"
}

pushd "$P4Root" >/dev/null || bail "Failed to enter P4ROOT"
curr_dir=$(pwd)
P4PORT="rsh:p4d $P4DCaseFlag -r \"$curr_dir\" -L log -vserver=3 -i"
printf 'P4PORT=%s\n' "$P4PORT" > .p4config
export P4CONFIG=.p4config

p4d $P4DCaseFlag -r . -jr jnl.0 || bail "Failed to restore journal"
p4d $P4DCaseFlag -r . -J journal -xu || bail "Failed to upgrade repository"
p4 -p "$P4PORT" storage -r || bail "Failed to rebuild storage"
p4 -p "$P4PORT" storage -w || bail "Failed to upgrade storage"
p4 -p "$P4PORT" configure set monitor=1 || bail "Failed to enable monitoring"
p4 -p "$P4PORT" configure set lbr.bufsize=1M || bail "Failed to configure lbr.bufsize"
p4 -p "$P4PORT" configure set filesys.bufsize=1M || bail "Failed to configure filesys.bufsize"

if [[ $ConvertUnicode -ne 0 ]]; then
    p4d -r . -xi || bail "Failed to convert repository to Unicode mode; see jnl.invalid-utf8"
fi

p4 -p "$P4PORT" dirs "//*" | while IFS= read -r directory; do
    printf '%s/...\n' "$directory"
done > dirs.txt
if [[ -s dirs.txt ]]; then
    msg "Verifying with -qu ..."
    parallel -a dirs.txt p4 verify -qu {} > verify.out 2>&1
    msg "Verify errors: $(wc -l < verify.out)"
else
    : > verify.out
    msg "No depot directories to verify"
fi

popd >/dev/null
msg "Server is in directory:"
msg "$P4Root"
