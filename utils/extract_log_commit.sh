#!/bin/bash
# Extract commit from log file output of gitp4transfer

function bail () { echo -e "Error: ${1:-Unknown Error}\n"; exit ${2:-1}; }

commit=${1:-Unknown}
git_file=${2:-Unknown}

[[ -z $commit || $commit == "Unknown" ]] && bail "First parameter should be a commit ID with colon prefix, e.g. :12345"
[[ $commit =~ :[0-9]+ ]] || bail "Commit ID must start with colon and contain digits: '$commit'"
[[ ! -e $git_file || $git_file == "Unknown" ]] && bail "Second parameter should be a git file to search"

# 2553069:mark :398037
# 2553716:from :398037
# 2555800:from :398037

lines=$(grep -n "Commit: " "$git_file" | grep -A1 "$commit" | head -2)
start=$(echo "$lines" | head -1 | cut -d: -f1)
end=$(echo "$lines" | tail -1 | cut -d: -f1)

sed -n "${start},${end}p" "$git_file"
