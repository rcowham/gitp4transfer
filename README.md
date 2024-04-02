# GitP4Transfer

Options to import git repositories into Perforce Helix Core.

## GitP4Transfer.py

Script to migrate history from a git LFS repository into p4 (Perforce Helix Core).

As of 2022/04/19 this is functional for a single branch at a time, although still considered Beta state.

It loops over commits in reverse order:

* Checks out the commit (LFS will get the contents of the files)
* Uses `git diff-tree` and then replays the contents of each file
* Focusses on a single branch at a time (e.g. master), not branches
* Stores latest processed commit as a counter in Helix Core - so incremental or partial runs can be done
* Can process active branches with some manual steps.

For details of design/usage etc see: [GitP4Transfer.adoc](doc/GitP4Transfer.adoc)

## gitp4transfer - Go version

The intent of this is to process `git fast-export` files and import then into Helix Core. It does this by creating
a Helix Core checkpoint file (and associated archive files).

This is documented in: [GitP4Transfer.adoc](doc/GitP4Transfer.adoc#gitp4transfer-tool-for-git-and-plastic-scm-migrations)

It is considered to be in beta release.
