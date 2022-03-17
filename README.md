# GitP4Transfer

## GitP4Transfer.py

Script to migrate history from a git LFS repository into p4 (Perforce Helix Core).

As of 2022/03/17 this is functional for a single branch at a time, although still considered Alpha state.

It loops over commits in reverse order:

* Checks out the commit (LFS will get the contents of the files)
* Uses git diff-tree and then replays the contents of each file
* Focusses on a single branch at a time (e.g. master), not branches
* Stores latest processed commit as a counter
* Can process active branches 

For details of design/usage etc see: [GitP4Transfer.adoc](doc/GitP4Transfer.adoc)

## gitp4transfer - Go version

The intent of this is to process `git fast-export` files.

As of 2022-03/17 - this is not yet functional!!
