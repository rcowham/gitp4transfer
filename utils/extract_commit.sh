#!/bin/bash
# Extract commit from .git file

function bail () { echo -e "Error: ${1:-Unknown Error}\n"; exit ${2:-1}; }

commit=${1:-Unknown}
git_file=${2:-Unknown}

[[ -z $commit || $commit == "Unknown" ]] && bail "First parameter should be a commit ID with colon prefix, e.g. :12345"
[[ $commit =~ :[0-9]+ ]] || bail "Commit ID must start with colon and contain digits: '$commit'"
[[ ! -e $git_file || $git_file == "Unknown" ]] && bail "Second parameter should be a git file to search"

# 2553069:mark :398037
# 2553716:from :398037
# 2555800:from :398037

start=$(grep -n "^mark $commit" "$git_file" | head -1 | cut -d: -f1)
end=$(grep -n "^from $commit" "$git_file" | head -1 | cut -d: -f1)
start=$((start - 2))

sed -n "${start},${end}p" "$git_file"
