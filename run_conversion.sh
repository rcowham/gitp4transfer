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
 
run_conversion.sh <git_fast_export> [-p <P4Root>] [-d] [-dummy] [-depot <import depot>] [-graph <graphFile.dot>] [-m <max commits>]
 
   or

run_conversion.sh -h

    -d          Debug
    -depot      Depot to use for this import (default is 'import')
    -dummy      Create dummy archives as placeholders (no real content) - much faster
    -graph      Create Graphviz output showing commit structure
    -m          Max no of commits to process
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
declare -i Dummy=0
declare -i MaxCommits=0
declare ConfigFile=""
declare P4Root=""
declare GitFile=""
declare GraphFile=""
declare ImportDepot="import"

set +u
while [[ $# -gt 0 ]]; do
    case $1 in
        (-h) usage -h  && exit 1;;
        # (-man) usage -man;;
        (-c) ConfigFile=$2; shiftArgs=1;;
        (-p) P4Root=$2; shiftArgs=1;;
        (-d) Debug=1;;
        (-depot) ImportDepot=1;;
        (-dummy) Dummy=1;;
        (-graph) GraphFile=$2; shiftArgs=1;;
        (-m) MaxCommits=$2; shiftArgs=1;;
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
declare MaxCommitArgs=""
if [[ $MaxCommits -gt 0 ]]; then
    MaxCommitArgs="--max.commits=$MaxCommits"
fi
DummyFlag=""
if [[ $Dummy -ne 0 ]]; then
    DummyFlag="--dummy"
fi
GraphArgs=""
if [[ ! -z $GraphFile ]]; then
    GraphArgs="--graphfile=$GraphFile"
fi
ConfigArgs=""
if [[ ! -z $ConfigFile ]]; then
    ConfigArgs="--config=$ConfigFile"
fi

echo ./gitp4transfer --archive.root="$P4Root" $DebugFlag $DummyFlag $MaxCommitArgs $GraphArgs --import.depot="$ImportDepot" --journal="$P4Root/jnl.0" "$GitFile"
./gitp4transfer --archive.root="$P4Root" $ConfigArgs $DebugFlag $DummyFlag $MaxCommitArgs $GraphArgs --import.depot="$ImportDepot" --journal="$P4Root/jnl.0" "$GitFile"

pushd "$P4Root"
curr_dir=$(pwd)

declare P4PORT="rsh:p4d -r \"$curr_dir\" -L log -vserver=3 -i"
p4d -r . -jr jnl.0
p4d -r . -J journal -xu
p4 -p "$P4PORT" storage -r
p4 -p "$P4PORT" storage -w
p4 -p "$P4PORT" configure set monitor=1

echo "P4PORT=$P4PORT" > .p4config
export P4CONFIG=.p4config

rm -f dirs.txt
p4 dirs "//$ImportDepot/*" | while read -e f; do echo "$f/..." >> dirs.txt; done
echo "Verifying with -qu ..."
parallel -a dirs.txt p4 verify -qu {} > verify.out 2>&1
echo "Verify errors: $(wc -l verify.out)"

echo "Server is in directory:"
echo "$P4Root"