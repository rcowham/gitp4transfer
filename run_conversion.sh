#!/bin/bash
# Run's the gitp4transfer conversion tool and performs various p4d actions to create the resulting repository, upgrade it etc.
# Dependencies:
# - p4 and p4d in $PATH
# - GNU parallelel in $PATH (for verification step)

# shellcheck disable=SC2128
if [[ -z "${BASH_VERSINFO}" ]] || [[ -z "${BASH_VERSINFO[0]}" ]] || [[ ${BASH_VERSINFO[0]} -lt 4 ]]; then
    echo "This script requires Bash version >= 4";
    exit 1;
fi

# ============================================================

function msg () { echo -e "$*"; }
function bail () { msg "\nError: ${1:-Unknown Error}\n"; exit "${2:-1}"; }
function check_cmd () {
    if ! command -v $1 &> /dev/null
    then
        bail "$1 could not be found in $PATH - please install it"
    fi
}

function usage
{
   declare errorMessage=${2:-Unset}
 
   if [[ "$errorMessage" != Unset ]]; then
      echo -e "\\n\\nUsage Error:\\n\\n$errorMessage\\n\\n" >&2
   fi
 
   echo "USAGE for run_conversion.sh:
 
run_conversion.sh <git_fast_export> [-p <P4Root>] [-d] [-c <configfile>] [-dummy] [-crlf] [ [-insensitive] | [-sensitive] ]
    [-depot <import depot>] [-graph <graphFile>] [-m <max commits>] [-t <parallel threads>]

   or

run_conversion.sh -h

    -c           <configfile> name of Yaml config file to control conversion (means parameters below don't need to be provided)
    -d           Debug
    -depot       <import depot> - Depot to use for this import (default is 'import')
    -crlf        Convert CRLF to just LF for text files - useful for importing Plastic Windows exports to a Linux p4d
    -unicode     Create a unicode enabled p4d repository (runs p4d -xi)
    -dummy       Create dummy archives as placeholders (no real content) - much faster
    -graph       <graphfile.dot> Create Graphviz output showing commit structure (see also 'gitgraph' utility which is more flexible)
    -insensitive Specify case insensitive checkpoint (and lowercase archive files) - for Linux servers
    -sensitive   Specify case sensitive checkpoint and restore - for Mac/Windows servers (for testing only)
    -m           <max commits> - Max no of commits to process (stops after this number is reached)
    -t           <parallel threads> - No of parallel threads to use (default is No of CPUs)
    -p          <P4Root> - directory to use as resulting P4Root - will default to a tmp dir if not set
    <git_fast_export> The (input) git fast-export format file (required)

Examples:

./run_conversion.sh export.git
./run_conversion.sh export.git -p P4Root

nohup ./run_conversion.sh export.git -p P4Root -d -c config.yaml > out1 &

"
}

# Command Line Processing
 
declare -i shiftArgs=0
declare -i Debug=0
declare -i Dummy=0
declare -i CaseInsensitive=0
declare -i CaseSensitive=0
declare -i ConvertCRLF=0        # Whether to attempt to convert CRLF to LF only
declare -i ConvertUnicode=0     # Convert resulting repo to unicode mode p4d -xi
declare -i MaxCommits=0
declare -i ParallelThreads=0
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
        (-crlf) ConvertCRLF=1;;
        (-unicode) ConvertUnicode=1;;
        (-p) P4Root=$2; shiftArgs=1;;
        (-d) Debug=1;;
        (-depot) ImportDepot=$2; shiftArgs=1;;
        (-dummy) Dummy=1;;
        (-insensitive) CaseInsensitive=1;;
        (-sensitive) CaseSensitive=1;;
        (-graph) GraphFile=$2; shiftArgs=1;;
        (-m) MaxCommits=$2; shiftArgs=1;;
        (-t) ParallelThreads=$2; shiftArgs=1;;
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

# Validate commands are in $PATH
check_cmd gitp4transfer
check_cmd p4d
check_cmd p4
check_cmd parallel

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
declare ParallelThreadArgs=""
if [[ $ParallelThreads -gt 0 ]]; then
    ParallelThreadArgs="--parallel.threads=$ParallelThreads"
fi
DummyFlag=""
if [[ $Dummy -ne 0 ]]; then
    DummyFlag="--dummy"
fi
CaseInsensitiveFlag=""
P4DCaseFlag=""
if [[ $CaseInsensitive -ne 0 ]]; then
    CaseInsensitiveFlag="--case.insensitive"
    P4DCaseFlag="-C1"
fi
if [[ $CaseSensitive -ne 0 ]]; then
    P4DCaseFlag="-C0"
fi
CRLFFlag=""
if [[ $ConvertCRLF -ne 0 ]]; then
    CRLFFlag="--convert.crlf"
fi
GraphArgs=""
if [[ ! -z $GraphFile ]]; then
    GraphArgs="--graphfile=$GraphFile"
fi
ConfigArgs=""
if [[ ! -z $ConfigFile ]]; then
    ConfigArgs="--config=$ConfigFile"
fi

echo gitp4transfer --archive.root="$P4Root" $ConfigArgs $DebugFlag $DummyFlag $CaseInsensitiveFlag $CRLFFlag $MaxCommitArgs $ParallelThreadArgs $GraphArgs --import.depot="$ImportDepot" --journal="$P4Root/jnl.0" "$GitFile"
gitp4transfer --archive.root="$P4Root" $ConfigArgs $DebugFlag $DummyFlag $CaseInsensitiveFlag $CRLFFlag $MaxCommitArgs $ParallelThreadArgs $GraphArgs --import.depot="$ImportDepot" --journal="$P4Root/jnl.0" "$GitFile"

if [[ $? -ne 0 ]]; then
    echo "Server is in directory:"
    echo "$P4Root"
    bail "Error running gitp4transfer"
fi

pushd "$P4Root"
curr_dir=$(pwd)

declare P4PORT="rsh:p4d $P4DCaseFlag -r \"$curr_dir\" -L log -vserver=3 -i"
echo "P4PORT=$P4PORT" > .p4config
export P4CONFIG=.p4config
p4d $P4DCaseFlag -r . -jr jnl.0 || bail "Failed to restore journal"
p4d $P4DCaseFlag -r . -J journal -xu || bail "Failed to upgrade repository"
p4 -p "$P4PORT" storage -r
p4 -p "$P4PORT" storage -w || bail "Failed to perform storage upgrade"å
p4 -p "$P4PORT" configure set monitor=1
p4 -p "$P4PORT" configure set lbr.bufsize=1M
p4 -p "$P4PORT" configure set filesys.bufsize=1M

if [[ $ConvertUnicode -ne 0 ]]; then
    p4d -r . -xi || bail "Failed to convert repository to unicode - please see jnl.invalid-utf8 file listing unicode errors"
fi

echo "P4PORT=$P4PORT" > .p4config
export P4CONFIG=.p4config

# Now perform parallel p4 verify command to update all the MD5 checksums as appropriate for revisions/storage records
rm -f dirs.txt
# This next command might want to be tweaked, if you are using a config file to specify branches say at 3 levels
# In such a case, change "//$ImportDepot/*" to "//$ImportDepot/*/*"
p4 dirs "//$ImportDepot/*" | while read -e f; do echo "$f/..." >> dirs.txt; done
echo "Verifying with -qu ..."
parallel -a dirs.txt p4 verify -qu {} > verify.out 2>&1

# Count the verify errors and report them
echo "Verify errors: $(wc -l verify.out)"

echo "Server is in directory:"
echo "$P4Root"
