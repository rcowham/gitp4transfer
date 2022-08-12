#!/bin/bash
# Run's the conversion.

# shellcheck disable=SC2128
if [[ -z "${BASH_VERSINFO}" ]] || [[ -z "${BASH_VERSINFO[0]}" ]] || [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "This script requires Bash version >= 4";
    exit 1;
fi

# ============================================================
# Configuration section

# ============================================================

function msg () { echo -e "$*"; }
function bail () { msg "\nError: ${1:-Unknown Error}\n"; exit "${2:-1}"; }

function usage
{
   declare errorMessage=${2:-Unset}
 
   if [[ "$errorMessage" != Unset ]]; then
      echo -e "\\n\\nUsage Error:\\n\\n$errorMessage\\n\\n" >&2
   fi
 
   echo "USAGE for run_conversion.sh:
 
run_conversion.sh <git_fast_export> [-p <P4Root>] [-d]
 
   or

run_conversion.sh -h

    -d          Debug
    <P4Root>    Directory to use as resulting P4Root - will default to a tmp dir
    <git_fast_export> The (input) git fast-export format file (required)

Examples:

./run_conversion.sh export.git
./run_conversion.sh export.git -p P4Root

"
}

# Command Line Processing
 
declare -i shiftArgs=0
declare -i Debug=0
declare P4Root=""
declare GitFile=""
declare ImportDepot="import"

set +u
while [[ $# -gt 0 ]]; do
    case $1 in
        (-h) usage -h  && exit 1;;
        # (-man) usage -man;;
        (-p) P4Root=$2; shiftArgs=1;;
        (-d) Debug=1;;
        (-*) usage -h "Unknown command line option ($1)." && exit 1;;
        (*) GitFile=$1;;
    esac
 
    # Shift (modify $#) the appropriate number of times.
    shift; while [[ "$shiftArgs" -gt 0 ]]; do
        [[ $# -eq 0 ]] && usage -h "Incorrect number of arguments."
        shiftArgs=$shiftArgs-1
        shift
    done
done
set -u

if [[ -z $P4Root ]]; then
    P4Root=$(mktemp -d 2>/dev/null || mktemp -d -t 'myP4Root')
fi
mkdir -p "$P4Root/$ImportDepot" || die "Failed to mkdir $P4Root/$ImportDepot"

DebugFlag=""
if [[ $Debug -ne 0 ]]; then
    DebugFlag="--debug 1"
fi

./gitp4transfer --archive.root="$P4Root" $DebugFlag --import.depot="$ImportDepot" --journal="$P4Root/jnl.0" "$GitFile"

pushd "$P4Root"

declare P4PORT="rsh:p4d -r \"$P4Root\" -L log -vserver=3 -i"
p4d -r . -jr jnl.0
p4d -r . -J journal -xu
p4 -p "$P4PORT" storage -r
p4 -p "$P4PORT" storage -w

echo "P4PORT=$P4PORT" > .p4config

echo "Server is in directory:"
echo "$P4Root"