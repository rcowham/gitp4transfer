# GitP4Transfer

## GitP4Transfer.py

Script to migrate history from a git LFS repository into p4 (Perforce Helix Core).

It loops over commits in reverse order:

* Checks out the commit (LFS will get the contents of the files)
* Uses git diff-tree and then replays the contents of each file
* Focusses on a single branch at a time (e.g. master), not branches
* Stores latest processed commit as a counter
* Can process active branches 

## gitp4transfer - Go version

This processes `git fast-export` files.

For details of design/usage etc see: [GitP4Transfer.adoc](doc/GitP4Transfer.adoc)
