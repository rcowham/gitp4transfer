# gitp4transfer - Go version

This is experimental. This doc is notes for ToDo list etc.

## Debugging

For a single test (with debug output):

    go test -run TestBranch -debug

    ~/go/bin/dlv debug github.com/rcowham/gitp4transfer -- --archive.root=/data/work/OnwardRepo.p4/p4root --debug=1 --max.commits=50 --import.depot=import --journal=/data/work/OnwardRepo.p4/p4root/jnl.0 /data/work/git-OnwardRepo


## Todo

* Report on everything
* Option to extract all
* Need to rename branches, or remap them
** Duplicate - or auto-write branch values?
* Rename of file on a branch (not previously changed)

Concerns:

* UTF encoding issues?

## Test Scenarios

* Given a root dir, write files
** For objects, write files as soon as you get a filename? Or at least consider that.
** Gzip or not
** Detect base file formats using magic signatures
** Issue around main/branch files
*** If From: is blank then assume on main/master?
* Specify main/master and follow back commits on that branch

Options:

* Parse files and contents and write out
** Requires changelist numbers - just use Marks from file.
* Option to filter only a subset of files (on any branch)
* Option to filter a single branch
* Mappings - to rename branches