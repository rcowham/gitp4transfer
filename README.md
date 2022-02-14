# GitP4Transfer

## GitP4Transfer.py

Script to migrate history from a git LFS repository into p4 (Perforce Helix Core).

It loops over commits in reverse order:

* Checks out the commit (LFS will get the contents of the files)
* Uses git diff-tree and then replays the contents of each file
* Focusses on a single branch at a time (e.g. master), not branches
* Stores latest processed commit as a counter
* Can process active branches 

## gitp4transfer - Go program

This uses git's fast-import file format.

Probably won't work for LFS files, although maybe via `git lfs migrate`??

### Todo

* Report on everything
* Write checkpoint (2004.1 format)
  * What will happen with .gz for all files including text? Maybe just use filetypes and fix after upgrades?
* Option to extract all
* Need to rename branches, or remap them

Concerns:

* Converts to string - should we leave as bytes?
* Gzip in threads?
* UTF encoding issues?
* 