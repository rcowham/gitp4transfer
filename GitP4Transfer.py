#!/usr/bin/env python3
# -*- coding: utf-8 -*-

# Copyright (c) 2021 Robert Cowham, Perforce Software Ltd
# ========================================
# Redistribution and use in source and binary forms, with or without
# modification, are permitted provided that the following conditions are
# met:
#
# 1.  Redistributions of source code must retain the above copyright
#     notice, this list of conditions and the following disclaimer.
#
# 2.  Redistributions in binary form must reproduce the above copyright
#     notice, this list of conditions and the following disclaimer in the
#     documentation and/or other materials provided with the
#     distribution.
#
# THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
# "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
# LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
# A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL PERFORCE
# SOFTWARE, INC. BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
# SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
# LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
# DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
# THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
# (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
# OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
#

"""
NAME:
    GitP4Transfer.py

DESCRIPTION:
    This python script (2.7/3.6+ compatible) will transfer Git changes into a Perforce
    Helix Core Repository, somewhat similar to 'git p4' (not historical) and also GitFusion (now deprecated).

    This script transfers changes in one direction - from a source Git server to a target p4 server.
    It handles LFS files in the source server (assuming git LFS is suitably installed and enabled)

    Requires Git version 2.7+ due to use of formatting flags

    Usage:

        python3 GitP4Transfer.py -h

    The script requires a config file, by default transfer.yaml.

    An initial example can be generated, e.g.

        GitP4Transfer.py --sample-config > transfer.yaml

    For full documentation/usage, see project doc:

        https://github.com/rcowham/gitp4transfer/blob/main/doc/GitP4Transfer.adoc

"""

# Notes:
#   Scan all commits for diffs
#   Scan all commits for other key info
#   Find start commit
#   Process in reverse order
#       Simple changes
#       Branch changes
#       Merge changes

from __future__ import print_function, division
from os import error

import sys
import re
import subprocess
import stat
import pprint
from string import Template
import argparse
import textwrap
import os.path
from datetime import datetime
import logging
import time
import platform
import collections

# Non-standard modules
import P4
import logutils

# Import yaml which will roundtrip comments
from ruamel.yaml import YAML
yaml = YAML()
subproc = subprocess  # Could have a wrapper for use on Windows

VERSION = """$Id: 74939df934a7a660e6beff62870f65635918300b $"""
ANON_BRANCH_PREFIX = "_anon"


if bytes is not str:
    # For python3, always encode and decode as appropriate
    def decode_text_stream(s):
        return s.decode() if isinstance(s, bytes) else s
else:
    # For python2.7, pass read strings as-is
    def decode_text_stream(s):
        return s


def anonymousBranch(branch):
    return branch.startswith(ANON_BRANCH_PREFIX)


def logrepr(self):
    return pprint.pformat(self.__dict__, width=240)


alreadyLogged = {}


# Log messages just once per run
def logOnce(logger, *args):
    global alreadyLogged
    msg = ", ".join([str(x) for x in args])
    if msg not in alreadyLogged:
        alreadyLogged[msg] = 1
        logger.debug(msg)


#
# P4 wildcards are not allowed in filenames.  P4 complains
# if you simply add them, but you can force it with "-f", in
# which case it translates them into %xx encoding internally.
#
def wildcard_decode(path):
    # Search for and fix just these four characters.  Do % last so
    # that fixing it does not inadvertently create new %-escapes.
    # Cannot have * in a filename in windows; untested as to
    # what p4 would do in such a case.
    if not platform.system() == "Windows":
        path = path.replace("%2A", "*")
    path = path.replace("%23", "#") \
               .replace("%40", "@") \
               .replace("%25", "%")
    return path


def wildcard_encode(path):
    # do % first to avoid double-encoding the %s introduced here
    path = path.replace("%", "%25") \
               .replace("*", "%2A") \
               .replace("#", "%23") \
               .replace("@", "%40")
    return path


def wildcard_present(path):
    m = re.search("[*#@%]", path)
    return m is not None


def isModeExec(mode):
    # Returns True if the given git mode represents an executable file,
    # otherwise False.
    return mode[-3:] == "755"


def isModeExecChanged(src_mode, dst_mode):
    return isModeExec(src_mode) != isModeExec(dst_mode)


_diff_tree_pattern = None


def parseDiffTreeEntry(entry):
    """Parses a single diff tree entry into its component elements.

    See git-diff-tree(1) manpage for details about the format of the diff
    output. This method returns a dictionary with the following elements:

    src_mode - The mode of the source file
    dst_mode - The mode of the destination file
    src_sha1 - The sha1 for the source file
    dst_sha1 - The sha1 fr the destination file
    status - The one letter status of the diff (i.e. 'A', 'M', 'D', etc)
    status_score - The score for the status (applicable for 'C' and 'R'
                   statuses). This is None if there is no score.
    src - The path for the source file.
    dst - The path for the destination file. This is only present for
          copy or renames. If it is not present, this is None.

    If the pattern is not matched, None is returned."""

    global _diff_tree_pattern
    if not _diff_tree_pattern:
        _diff_tree_pattern = re.compile(':(\d+) (\d+) (\w+) (\w+) ([A-Z])(\d+)?\t(.*?)((\t(.*))|$)')

    match = _diff_tree_pattern.match(entry)
    if match:
        return {
            'src_mode': match.group(1),
            'dst_mode': match.group(2),
            'src_sha1': match.group(3),
            'dst_sha1': match.group(4),
            'status': match.group(5),
            'status_score': match.group(6),
            'src': PathQuoting.dequote(match.group(7)),
            'dst': PathQuoting.dequote(match.group(10))
        }
    return None


P4.Revision.__repr__ = logrepr
P4.Integration.__repr__ = logrepr
P4.DepotFile.__repr__ = logrepr

python3 = sys.version_info[0] >= 3
if sys.hexversion < 0x02070000 or (0x0300000 <= sys.hexversion < 0x0303000):
    sys.exit("Python 2.7 or 3.3 or newer is required to run this program.")
reFetchMoveError = re.compile("Files are missing as a result of one or more move operations")

# Although this should work with Python 3, it doesn't currently handle Windows Perforce servers
# with filenames containing charaters such as umlauts etc: åäö


class P4TException(Exception):
    pass


class P4TLogicException(Exception):
    pass


class P4TConfigException(P4TException):
    pass


CONFIG_FILE = 'transfer.yaml'
GENERAL_SECTION = 'general'
SOURCE_SECTION = 'source'
TARGET_SECTION = 'target'
LOGGER_NAME = "GitP4Transfer"
CHANGE_MAP_DESC = "Updated change_map_file"

# This is for writing to sample config file
yaml.preserve_quotes = True
DEFAULT_CONFIG = yaml.load(r"""
# counter_name: Unique counter on target server to use for recording source changes processed. No spaces.
#    Name sensibly if you have multiple instances transferring into the same target p4 repository.
#    The counter value represents the last transferred change number - script will start from next change.
#    If not set, or 0 then transfer will start from first change.
counter_name: GitP4Transfer_counter

# instance_name: Name of the instance of GitP4Transfer - for emails etc. Spaces allowed.
instance_name: "Git LFS Transfer from XYZ"

# For notification - if smtp not available - expects a pre-configured nms FormMail script as a URL
#   E.g. expects to post using 2 fields: subject, message
# Alternatively, use the following entries (suitable adjusted) to use Mailgun for notifications
#   api: "<Mailgun API key"
#   url: "https://api.mailgun.net/v3/<domain or sandbox>"
#   mail_from: "Fred <fred@example.com>"
#   mail_to:
#   - "fred@example.com"
mail_form_url:

# The mail_* parameters must all be valid (non-blank) to receive email updates during processing.
# mail_to: One or more valid email addresses - comma separated for multiple values
#     E.g. somebody@example.com,somebody-else@example.com
mail_to:

# mail_from: Email address of sender of emails, E.g. p4transfer@example.com
mail_from:

# mail_server: The SMTP server to connect to for email sending, E.g. smtpserver.example.com
mail_server:

# ===============================================================================
# Note that for any of the following parameters identified as (Integer) you can specify a
# valid python expression which evaluates to integer value, e.g.
#     "24 * 60"
#     "7 * 24 * 60"
# Such values should be quoted (in order to be treated as strings)
# -------------------------------------------------------------------------------
# sleep_on_error_interval (Integer): How long (in minutes) to sleep when error is encountered in the script
sleep_on_error_interval: 60

# poll_interval (Integer): How long (in minutes) to wait between polling source server for new changes
poll_interval: 60

# change_batch_size (Integer): changelists are processed in batches of this size
change_batch_size: 1000

# The following *_interval values result in reports, but only if mail_* values are specified
# report_interval (Integer): Interval (in minutes) between regular update emails being sent
report_interval: 30

# error_report_interval (Integer): Interval (in minutes) between error emails being sent e.g. connection error
#     Usually some value less than report_interval. Useful if transfer being run with --repeat option.
error_report_interval: 15

# summary_report_interval (Integer): Interval (in minutes) between summary emails being sent e.g. changes processed
#     Typically some value such as 1 week (10080 = 7 * 24 * 60). Useful if transfer being run with --repeat option.
summary_report_interval: "7 * 24 * 60"

# max_logfile_size (Integer): Max size of file to (in bytes) after which it should be rotated
#     Typically some value such as 20MB = 20 * 1024 * 1024. Useful if transfer being run with --repeat option.
max_logfile_size: "20 * 1024 * 1024"

# change_description_format: The standard format for transferred changes.
#    Keywords prefixed with $. Use \\n for newlines. Keywords allowed:
#     $sourceDescription, $sourceChange, $sourceRepo, $sourceUser
change_description_format: "$sourceDescription\\n\\nTransferred from git://$sourceRepo@$sourceChange"

# superuser: Set to n if not a superuser (so can't update change times - can just transfer them).
superuser: "y"

source:
    # git_repo: root directory for git repo
    #    This will be used to update the client workspace Root: field for target workspace
    git_repo:

target:
    # P4PORT to connect to, e.g. some-server:1666 - if this is on localhost and you just
    # want to specify port number, then use quotes: "1666"
    p4port:
    # P4USER to use
    p4user:
    # P4CLIENT to use, e.g. p4-transfer-client
    p4client:
    # P4PASSWD for the user - valid password. If blank then no login performed.
    # Recommended to make sure user is in a group with a long password timeout!
    # Make sure your P4TICKETS file is correctly found in the environment
    p4passwd:
    # P4CHARSET to use, e.g. none, utf8, etc - leave blank for non-unicode p4d instance
    p4charset:

# branch_maps: An array of git branches to migrate and where to.
#    Note that other branches encountered will be given temp names under anon_branches_root
#    Entries specify 'git_branch' and 'targ'. No wildcards.
branch_maps:
  - git_branch:  "master"
    targ:   "//git_import/master"

# import_anon_branches: Set this to 'y' to import anonymous branches
#   Any other value means they will not be imported.
import_anon_branches: n

# anon_branches_root: A depot path used for anonymous git branches (names automatically generated).
#   Mandatory.
#   Such branches only contain files modified on git branch.
#   Name of branch under this root is _anonNNNN with a unique ID.
#   If this field is empty, then no anonymous branches will be created/imported.
#anon_branches_root: //git_import/temp_branches
anon_branches_root:

""")


def ensureDirectory(directory):
    if not os.path.isdir(directory):
        os.makedirs(directory)


def makeWritable(fpath):
    "Make file writable"
    os.chmod(fpath, stat.S_IWRITE + stat.S_IREAD)


def p4time(unixtime):
    "Convert time to Perforce format time"
    return time.strftime("%Y/%m/%d:%H:%M:%S", time.localtime(unixtime))


def printSampleConfig():
    "Print defaults from above dictionary for saving as a base file"
    print("")
    print("# Save this output to a file to e.g. transfer.yaml and edit it for your configuration")
    print("")
    yaml.dump(DEFAULT_CONFIG, sys.stdout)
    sys.stdout.flush()


def fmtsize(num):
    for x in ['bytes', 'KB', 'MB', 'GB', 'TB']:
        if num < 1024.0:
            return "%3.1f %s" % (num, x)
        num /= 1024.0



class PathQuoting:
    """From git-filter-repo.py - great for python2 - but needs conversion to bytes for python3"""
    _unescape = {b'a': b'\a',
                 b'b': b'\b',
                 b'f': b'\f',
                 b'n': b'\n',
                 b'r': b'\r',
                 b't': b'\t',
                 b'v': b'\v',
                 b'"': b'"',
                 b'\\':b'\\'}
    _unescape_re = re.compile(br'\\([a-z"\\]|[0-9]{3})')
    _escape = [bytes([x]) for x in range(127)]+[
              b'\\'+bytes(ord(c) for c in oct(x)[2:]) for x in range(127,256)]
    _reverse = dict(map(reversed, _unescape.items()))
    for x in _reverse:
        _escape[ord(x)] = b'\\'+_reverse[x]
    _special_chars = [len(x) > 1 for x in _escape]

    @staticmethod
    def unescape_sequence(orig):
        seq = orig.group(1)
        return PathQuoting._unescape[seq] if len(seq) == 1 else bytes([int(seq, 8)])

    @staticmethod
    def dequote(quoted_string):
        if quoted_string and quoted_string.startswith('"'):
            assert quoted_string.endswith('"')
            quoted_string = quoted_string.encode()
            result = PathQuoting._unescape_re.sub(PathQuoting.unescape_sequence,
                                          quoted_string[1:-1])
            return result.decode()
        return quoted_string


class GitFileChanges():
    "Convenience class for file changes as part of a git commit"

    def __init__(self, modes, shas, changeTypes, filenames) -> None:
        self.modes = modes
        self.shas = shas
        self.changeTypes = changeTypes
        self.filenames = filenames


class GitCommit():
    "Convenience class for a git commit"

    def __init__(self, commitID, name, email, description) -> None:
        self.commitID = commitID
        self.name = name
        self.email = email
        self.description = description
        self.parents = []
        self.fileChanges = []
        self.branch = None
        self.parentBranch = None
        self.firstOnBranch = False


class GitInfo:
    "Extract info about Git repo"

    def __init__(self, logger) -> None:
        self.logger = logger
        self.anonBranchInd = 0

    def read_pipe_lines(self, c):
        self.logger.debug('Reading pipe: %s\n' % str(c))

        expand = not isinstance(c, list)
        p = subprocess.Popen(c, stdout=subprocess.PIPE, shell=expand)
        pipe = p.stdout
        val = [decode_text_stream(line) for line in pipe.readlines()]
        if pipe.close() or p.wait():
            raise Exception('Command failed: %s' % str(c))
        return val

    def getBasicCommitInfo(self, refs):
        "Return a dict indexed by commit hash of populated GitCommit objects"
        # Format string: %cn=committer, %ce=committer email, %B=raw desc
        cmd = ('git rev-list %s %s' % (' '.join(refs), '--format=%cn%n%ce%n%B%n"__END_OF_DESC__"'))
        if self.logger:
            self.logger.debug(cmd)
        dtp = subproc.Popen(cmd, shell=True, bufsize=-1, stdout=subprocess.PIPE)
        f = dtp.stdout
        line = decode_text_stream(f.readline())
        if not line:
            raise SystemExit(("Nothing to analyze; repository is empty."))
        cont = bool(line)
        commits = {}
        while cont:
            if not line:
                break
            commitID = decode_text_stream(line).rstrip().split()[1]
            name = decode_text_stream(f.readline()).rstrip()
            email = decode_text_stream(f.readline()).rstrip()
            desc = []

            in_desc = True
            while in_desc:
                line = decode_text_stream(f.readline()).rstrip()
                if line.startswith('__END_OF_DESC__'):
                    in_desc = False
                elif line:
                    desc.append(line)
            commits[commitID] = GitCommit(commitID, name, email, '\n'.join(desc))
            line = decode_text_stream(f.readline())
        # Close the output, ensure command completed successfully
        dtp.stdout.close()
        if dtp.wait():
            raise SystemExit(("Error: rev-list pipeline failed; see above.")) # pragma: no cover
        return commits

    def getCommitDiffs(self, refs):
        "Return array of commits in reverse order for processing, together with dict of commits"
        # Setup the rev-list/diff-tree process and read info about file diffs
        # Learned from git-filter-repo
        cmd = ('git rev-list --first-parent --reverse {}'.format(' '.join(refs)) +
                     ' | git diff-tree --stdin --always --root --format=%H%n%P%n%cn%n%ce%n%B%n"__END_OF_DESC__"%n%cd' +
                     ' --date=iso-local -M -t -c --raw --combined-all-paths')
        if self.logger:
            self.logger.debug(cmd)
        dtp = subproc.Popen(cmd, shell=True, bufsize=-1, stdout=subprocess.PIPE)
        f = dtp.stdout
        commitList = []
        commits = {}
        line = decode_text_stream(f.readline())
        if not line:
            return commitList, commits
        cont = bool(line)

        while cont:
            commitID = decode_text_stream(line).rstrip()
            parents = decode_text_stream(f.readline()).split()
            name = decode_text_stream(f.readline()).rstrip()
            email = decode_text_stream(f.readline()).rstrip()
            desc = []
            in_desc = True
            while in_desc:
                line = decode_text_stream(f.readline()).rstrip()
                if line.startswith('__END_OF_DESC__'):
                    in_desc = False
                elif line:
                    desc.append(line)
            date = decode_text_stream(f.readline()).rstrip()

            # We expect a blank line next; if we get a non-blank line then
            # this commit modified no files and we need to move on to the next.
            # If there is no line, we've reached end-of-input.
            line = decode_text_stream(f.readline())
            if not line:
                cont = False
            line = line.rstrip()

            # If we haven't reached end of input, and we got a blank line meaning
            # a commit that has modified files, then get the file changes associated
            # with this commit.
            fileChanges = []
            if cont and not line:
                cont = False
                for line in f:
                    line = decode_text_stream(line)
                    if not line.startswith(':'):
                        cont = True
                        break
                    n = 1 + max(1, len(parents))
                    assert line.startswith(':'*(n-1))
                    relevant = line[n-1:-1]
                    splits = relevant.split(None, n)
                    modes = splits[0:n]
                    splits = splits[n].split(None, n)
                    shas = splits[0:n]
                    splits = splits[n].split('\t')
                    change_types = splits[0]
                    filenames = [PathQuoting.dequote(x) for x in splits[1:]]
                    fileChanges.append(GitFileChanges(modes, shas, change_types, filenames))

            commits[commitID] = GitCommit(commitID, name, email, '\n'.join(desc))
            commits[commitID].parents = parents
            commits[commitID].fileChanges = fileChanges
            commitList.append(commitID)

        # Close the output, ensure rev-list|diff-tree pipeline completed successfully
        dtp.stdout.close()
        if dtp.wait():
            raise SystemExit(("Error: rev-list|diff-tree pipeline failed; see above.")) # pragma: no cover
        return commitList, commits

    def getFileChanges(self, commit):
        "Return file changes for a commit which is a merge - thus itself against its first parent"
        cmd = ('git diff-tree -r {} {}'.format(commit.commitID, commit.parents[0]))
        if self.logger:
            self.logger.debug(cmd)
        dtp = subproc.Popen(cmd, shell=True, bufsize=-1, stdout=subprocess.PIPE)
        f = dtp.stdout
        fileChanges = []
        for line in f:
            line = decode_text_stream(line)
            if not line.startswith(':'):
                continue
            n = 2 # 1 + max(1, len(commit.parents))
            assert line.startswith(':'*(n-1))
            relevant = line[n-1:-1]
            splits = relevant.split(None, n)
            modes = splits[0:n]
            splits = splits[n].split(None, n)
            shas = splits[0:n]
            splits = splits[n].split('\t')
            change_types = splits[0]
            # filenames = [PathQuoting.dequote(x) for x in splits[1:]]
            filenames = [x for x in splits[1:]]
            fileChanges.append(GitFileChanges(modes, shas, change_types, filenames))
        dtp.stdout.close()
        if dtp.wait():
            raise SystemExit(("Error: {} failed; see above.".format(cmd))) # pragma: no cover
        return fileChanges

    def getBranchCommits(self, branchRefs):
        "Returns a list of commit ids on the referenced branches"
        branchCommits = {}
        for b in branchRefs:
            branchCommits[b] = []
            cmd = ('git rev-list --first-parent {}'.format(b))
            if self.logger:
                self.logger.debug(cmd)
            dtp = subproc.Popen(cmd, shell=True, bufsize=-1, stdout=subprocess.PIPE)
            f = dtp.stdout
            line = decode_text_stream(f.readline())
            if not line:
                return
            cont = bool(line)
            while cont:
                commit = decode_text_stream(line).rstrip()
                branchCommits[b].append(commit)
                line = decode_text_stream(f.readline())
                if not line:
                    break

        dtp.stdout.close()
        if dtp.wait():
            raise SystemExit("Error: {} failed; see above.".format(cmd)) # pragma: no cover
        return branchCommits

    def updateBranchInfo(self, branchRefs, commitList, commits):
        "Updates the branch details for every commit"
        branchCommits = self.getBranchCommits(branchRefs)
        for b in branchRefs:
            for id in branchCommits[b]:
                if not id in commitList:
                    raise P4TException("Failed to find commit: %s" % id)
                commits[id].branch = b
        # Now update anonymous branches - in commit order (so parents first)
        for id in commitList:
            if not commits[id].branch:
                firstParent = commits[id].parents[0]
                assert(commits[firstParent].branch is not None)
                if not commits[firstParent].branch.startswith(ANON_BRANCH_PREFIX):
                    self.anonBranchInd += 1
                    anonBranch = "%s%04d" % (ANON_BRANCH_PREFIX, self.anonBranchInd)
                    commits[id].branch = anonBranch
                    commits[id].parentBranch = commits[firstParent].branch
                    commits[id].firstOnBranch = True
                else:
                    commits[id].branch = commits[firstParent].branch


class ChangeRevision:
    "Represents a change - created from P4API supplied information and thus encoding"

    def __init__(self, rev, change, n):
        self.rev = rev
        self.action = change['action'][n]
        self.type = change['type'][n]
        self.depotFile = change['depotFile'][n]
        self.localFile = None
        self.fileSize = 0
        self.digest = ""
        self.fixedLocalFile = None


    def depotFileRev(self):
        "Fully specify depot file with rev number"
        return "%s#%s" % (self.depotFile, self.rev)

    def localFileRev(self):
        "Fully specify local file with rev number"
        return "%s#%s" % (self.localFile, self.rev)

    def setLocalFile(self, localFile):
        self.localFile = localFile
        localFile = localFile.replace("%40", "@")
        localFile = localFile.replace("%23", "#")
        localFile = localFile.replace("%2A", "*")
        localFile = localFile.replace("%25", "%")
        localFile = localFile.replace("/", os.sep)
        self.fixedLocalFile = localFile

    def __repr__(self):
        return 'rev={rev} action={action} type={type} size={size} digest={digest} depotFile={depotfile}' .format(
            rev=self.rev,
            action=self.action,
            type=self.type,
            size=self.fileSize,
            digest=self.digest,
            depotfile=self.depotFile,
        )


class P4Base(object):
    "Processes a config"

    section = None
    P4PORT = None
    P4CLIENT = None
    P4CHARSET = None
    P4USER = None
    P4PASSWD = None
    counter = 0
    clientLogged = 0

    def __init__(self, section, options, p4id):
        self.section = section
        self.options = options
        self.logger = logging.getLogger(LOGGER_NAME)
        self.p4id = p4id
        self.p4 = None
        self.client_logged = 0

    def __str__(self):
        return '[section = {} P4PORT = {} P4CLIENT = {} P4USER = {} P4PASSWD = {} P4CHARSET = {}]'.format(
            self.section,
            self.P4PORT,
            self.P4CLIENT,
            self.P4USER,
            self.P4PASSWD,
            self.P4CHARSET,
            )

    def connect(self, progname):
        self.p4 = P4.P4()
        self.p4.port = self.P4PORT
        self.p4.client = self.P4CLIENT
        self.p4.user = self.P4USER
        self.p4.prog = progname
        self.p4.exception_level = P4.P4.RAISE_ERROR
        self.p4.connect()
        if self.P4CHARSET is not None:
            self.p4.charset = self.P4CHARSET
        if self.P4PASSWD is not None:
            self.p4.password = self.P4PASSWD
            self.p4.run_login()

    def p4cmd(self, *args, **kwargs):
        "Execute p4 cmd while logging arguments and results"
        self.logger.debug(self.p4id, args)
        output = self.p4.run(args, **kwargs)
        self.logger.debug(self.p4id, output)
        self.checkWarnings()
        return output

    def disconnect(self):
        if self.p4:
            self.p4.disconnect()

    def checkWarnings(self):
        if self.p4 and self.p4.warnings:
            self.logger.warning('warning result: {}'.format(str(self.p4.warnings)))

    # def resetWorkspace(self):
    #     self.p4cmd('sync', '//%s/...#none' % self.p4.P4CLIENT)

    def createClientWorkspace(self):
        """Create or adjust client workspace for target
        """
        clientspec = self.p4.fetch_client(self.p4.client)
        logOnce(self.logger, "orig %s:%s:%s" % (self.p4id, self.p4.client, pprint.pformat(clientspec)))

        self.root = self.source.git_repo
        clientspec._root = self.root
        clientspec["Options"] = clientspec["Options"].replace("normdir", "rmdir")
        clientspec["Options"] = clientspec["Options"].replace("noallwrite", "allwrite")
        clientspec["LineEnd"] = "unix"
        clientView = []
        v = self.options.branch_maps[0] # Start with first one - assume to be equivalent of master
        line = "%s/... //%s/..." % (v['targ'], self.p4.client)
        clientView.append(line)
        for exclude in ['.git/...']:
            line = "-%s/%s //%s/%s" % (v['targ'], exclude, self.p4.client, exclude)
            clientView.append(line)

        clientspec._view = clientView
        self.clientmap = P4.Map(clientView)
        self.clientspec = clientspec
        self.p4.save_client(clientspec)
        logOnce(self.logger, "updated %s:%s:%s" % (self.p4id, self.p4.client, pprint.pformat(clientspec)))

        self.p4.cwd = self.root
        ctr = P4.Map('//"'+clientspec._client+'/..."   "' + clientspec._root + '/..."')
        self.localmap = P4.Map.join(self.clientmap, ctr)
        self.depotmap = self.localmap.reverse()

    def updateClientWorkspace(self, branch):
        """ Adjust client workspace for new branch"""
        clientspec = self.p4.fetch_client(self.p4.client)
        logOnce(self.logger, "orig %s:%s:%s" % (self.p4id, self.p4.client, pprint.pformat(clientspec)))

        clientView = []
        if anonymousBranch(branch):
            targ = "%s/%s" % (self.options.anon_branches_root, branch)
        else:
            for v in self.options.branch_maps:
                if v['git_branch'] == branch:
                    targ = v['targ']
                    break
        line = "%s/... //%s/..." % (targ, self.p4.client)
        clientView.append(line)
        for exclude in ['.git/...']:
            line = "-%s/%s //%s/%s" % (targ, exclude, self.p4.client, exclude)
            clientView.append(line)

        clientspec._view = clientView
        self.clientmap = P4.Map(clientView)
        self.clientspec = clientspec
        self.p4.save_client(clientspec)
        logOnce(self.logger, "updated %s:%s:%s" % (self.p4id, self.p4.client, pprint.pformat(clientspec)))
        self.logger.debug("Updated client view for branch: %s" % branch)
        ctr = P4.Map('//"'+clientspec._client+'/..."   "' + clientspec._root + '/..."')
        self.localmap = P4.Map.join(self.clientmap, ctr)
        self.depotmap = self.localmap.reverse()

    def getBranchMap(self, origBranch, newBranch):
        """Create a mapping between original and new branches"""
        src = ""
        targ = ""
        if anonymousBranch(origBranch):
            src = "%s/%s" % (self.options.anon_branches_root, origBranch)
        else:
            for v in self.options.branch_maps:
                if v['git_branch'] == origBranch:
                    src = v['targ']
        if anonymousBranch(newBranch):
            targ = "%s/%s" % (self.options.anon_branches_root, newBranch)
        else:
            for v in self.options.branch_maps:
                if v['git_branch'] == newBranch:
                    targ = v['targ']
        line = "%s/... %s/..." % (src, targ)
        self.logger.debug("Map: %s" % line)
        branchMap = P4.Map(line)
        return branchMap


tempBranch = "p4_exportBranch"


class GitSource(P4Base):
    "Functionality for reading from source Perforce repository"

    def __init__(self, section, options):
        super(GitSource, self).__init__(section, options, 'src')
        self.gitinfo = GitInfo(self.logger)

    def run_cmd(self, cmd, dir=".", get_output=True, timeout=2*60*60, stop_on_error=True):
        "Run cmd logging input and output"
        output = ""
        try:
            self.logger.debug("Running: %s" % cmd)
            if get_output:
                p = subprocess.Popen(cmd, cwd=dir, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True, shell=True)
                if python3:
                    output, _ = p.communicate(timeout=timeout)
                else:
                    output, _ = p.communicate()
                # rc = p.returncode
                self.logger.debug("Output:\n%s" % output)
            else:
                result = subprocess.run(cmd, shell=True, check=True, capture_output=True)
                self.logger.debug('Result: %s' % str(result))
        except subprocess.CalledProcessError as e:
            self.logger.debug("Output: %s" % e.output)
            if stop_on_error:
                msg = 'Failed run_cmd: %d %s' % (e.returncode, str(e))
                self.logger.debug(msg)
                raise e
        except Exception as e:
            self.logger.debug("Output: %s" % output)
            if stop_on_error:
                msg = 'Failed run_cmd: %s' % str(e)
                self.logger.debug(msg)
                raise e
        return output

    def missingCommits(self, counter):
        # self.gather_commits()
        branchRefs = [t['git_branch'] for t in self.options.branch_maps]
        self.gitinfo = GitInfo(self.logger)
        commitList, commits = self.gitinfo.getCommitDiffs(branchRefs)
        try:
            ind = commitList.index(counter)
            commitList = commitList[ind+1:]
        except ValueError:
            pass
        # self.gitinfo.updateBranchInfo(branchRefs, commitList, commits)
        self.logger.debug("commits: %s" % ' '.join(commitList))
        maxChanges = 0
        if self.options.change_batch_size:
            maxChanges = self.options.change_batch_size
        if self.options.maximum and self.options.maximum < maxChanges:
            maxChanges = self.options.maximum
        if maxChanges > 0:
            commitList = commitList[:maxChanges]
        self.logger.debug('processing %d commits' % len(commitList))
        self.commitList = commitList
        self.commits = commits
        return commitList, commits
    
    def fileModified(self, filename):
        "Returns true if git thinks file has changed on disk"
        args = ['git', 'status', '-z', filename]
        result = self.run_cmd(' '.join(args))
        return len(result) > 0

    def checkoutCommit(self, commitID):
        """Expects change number as a string, and returns list of filerevs"""
        args = ['git', 'switch', '-C', tempBranch, commitID]
        self.run_cmd(' '.join(args), get_output=False)

class P4Target(P4Base):
    "Functionality for transferring changes to target Perforce repository"

    def __init__(self, section, options, source):
        super(P4Target, self).__init__(section, options, 'targ')
        self.source = source
        self.filesToIgnore = []
        self.currentBranch = ""

    def formatChangeDescription(self, **kwargs):
        """Format using specified format options - see call in replicateCommit"""
        format = self.options.change_description_format
        format = format.replace("\\n", "\n")
        t = Template(format)
        result = t.safe_substitute(**kwargs)
        return result

    def ignoreFile(self, fname):
        "Returns True if file is to be ignored"
        if not self.options.re_ignore_files:
            return False
        for exp in self.options.re_ignore_files:
            if exp.search(fname):
                return True
        return False
    
    def p4_integrate(self, src, dest):
        self.p4cmd("integrate", "-Dt", wildcard_encode(src), wildcard_encode(dest))

    # def p4_sync(f, *options):
    #     p4_system(["sync"] + list(options) + [wildcard_encode(f)])

    def p4_add(self, f):
        # forcibly add file names with wildcards
        if wildcard_present(f):
            self.p4cmd("add", "-f", f)
        else:
            self.p4cmd("add", f)

    def p4_delete(self, f):
        self.p4cmd("delete", wildcard_encode(f))

    def p4_edit(self, f, *options):
        self.p4cmd("edit", options, wildcard_encode(f))

    def p4_revert(self, f):
        self.p4cmd("revert", wildcard_encode(f))

    def p4_reopen(self, type, f):
        self.p4cmd("reopen", "-t", type, wildcard_encode(f))

    # def p4_reopen_in_change(changelist, files):
    #     cmd = ["reopen", "-c", str(changelist)] + files
    #     p4_system(cmd)

    def p4_move(self, src, dest):
        self.p4cmd("move", "-k", wildcard_encode(src), wildcard_encode(dest))

    def replicateCommit(self, commit):
        """This is the heart of it all. Replicate a single commit/change"""

        self.filesToIgnore = []
        # Branch processing currently removed for now.
        # if self.currentBranch == "":
        #     self.currentBranch = commit.branch
        # if self.currentBranch != commit.branch:
        #     self.updateClientWorkspace(commit.branch)
        #     if commit.firstOnBranch:
        #         self.p4cmd('sync', '-k')
        #     fileChanges = commit.fileChanges
        #     if len(commit.parents) > 1:
        #         # merge commit
        #         parentBranch = self.source.commits[commit.parents[1]].branch
        #         branchMap = self.getBranchMap(parentBranch, commit.branch)
        #     else:
        #         branchMap = self.getBranchMap(commit.parentBranch, commit.branch)
        #     if len(fileChanges) == 0:
        #         # Do a git diff-tree to make sure we detect files changed on the target branch.
        #         fileChanges = self.source.gitinfo.getFileChanges(commit)
        #     for fc in fileChanges:
        #         self.logger.debug("fileChange: %s %s" % (fc.changeTypes, fc.filenames[0]))
        #         if fc.changeTypes == 'A':
        #             self.p4cmd('rec', '-af', fc.filenames[0])
        #         elif fc.changeTypes == 'M' or fc.changeTypes == 'MM':
        #             # Translate target depot to source via client map and branch map
        #             depotFile = self.depotmap.translate(os.path.join(self.source.git_repo, fc.filenames[0]))
        #             src = branchMap.translate(depotFile, 0)
        #             self.p4cmd('sync', '-k', fc.filenames[0])
        #             self.p4cmd('integrate', src, fc.filenames[0])
        #             self.p4cmd('resolve', '-at')
        #             # After whatever p4 has done to the file contents we ensure it is as per git
        #             if self.source.fileModified(fc.filenames[0]):
        #                 self.p4cmd('edit', fc.filenames[0])
        #                 args = ['git', 'restore', fc.filenames[0]]
        #                 self.source.run_cmd(' '.join(args))
        #         elif fc.changeTypes == 'D':
        #             self.p4cmd('rec', '-d', fc.filenames[0])
        #         else: # Better safe than sorry! Various known actions not yet implemented
        #             raise P4TLogicException('Action not yet implemented: %s', fc.changeTypes)
        #     self.currentBranch = commit.branch
        # else:
        self.p4cmd('sync', '-k')
        fileChanges = commit.fileChanges
        if not fileChanges or (0 < len([f for f in fileChanges if f.changeTypes == 'MM'])):
            # Do a git diff-tree to make sure we detect files changed on the target branch rather than just dirs
            fileChanges = self.source.gitinfo.getFileChanges(commit)
        
        if not commit.parents:
            for fc in fileChanges:
                self.logger.debug("fileChange: %s %s" % (fc.changeTypes, fc.filenames[0]))
                if fc.filenames[0]:
                    filename = PathQuoting.dequote(fc.filenames[0])
                    if fc.changeTypes == 'A':
                        self.p4cmd('rec', '-af', filename)
                    elif fc.changeTypes == 'M':
                        self.p4cmd('rec', '-e', filename)
                    elif fc.changeTypes == 'D':
                        self.p4cmd('rec', '-d', filename)
                    else: # Better safe than sorry! Various known actions not yet implemented
                        raise P4TLogicException('Action not yet implemented: %s', fc.changeTypes)
        else:
            diff = self.source.gitinfo.read_pipe_lines("git diff-tree -r %s \"%s^\" \"%s\"" % (
                        "-M", commit.commitID, commit.commitID))
            filesToAdd = set()
            filesToChangeType = set()
            filesToDelete = set()
            editedFiles = set()
            pureRenameCopy = set()
            symlinks = set()
            filesToChangeExecBit = {}
            all_files = list()

            for line in diff:
                diff = parseDiffTreeEntry(line)
                modifier = diff['status']
                path = diff['src']
                all_files.append(path)

                if modifier == "M":
                    self.p4_edit(path)
                    if isModeExecChanged(diff['src_mode'], diff['dst_mode']):
                        filesToChangeExecBit[path] = diff['dst_mode']
                    editedFiles.add(path)
                elif modifier == "A":
                    filesToAdd.add(path)
                    filesToChangeExecBit[path] = diff['dst_mode']
                    if path in filesToDelete:
                        filesToDelete.remove(path)
                    dst_mode = int(diff['dst_mode'], 8)
                    if dst_mode == 0o120000:
                        symlinks.add(path)
                elif modifier == "D":
                    filesToDelete.add(path)
                    if path in filesToAdd:
                        filesToAdd.remove(path)
                elif modifier == "C":
                    src, dest = diff['src'], diff['dst']
                    all_files.append(dest)
                    self.p4_integrate(src, dest)
                    pureRenameCopy.add(dest)
                    if diff['src_sha1'] != diff['dst_sha1']:
                        self.p4_edit(dest)
                        pureRenameCopy.discard(dest)
                    if isModeExecChanged(diff['src_mode'], diff['dst_mode']):
                        self.p4_edit(dest)
                        pureRenameCopy.discard(dest)
                        filesToChangeExecBit[dest] = diff['dst_mode']
                    if self.isWindows:
                        # turn off read-only attribute
                        os.chmod(dest, stat.S_IWRITE)
                    os.unlink(dest)
                    editedFiles.add(dest)
                elif modifier == "R":
                    src, dest = diff['src'], diff['dst']
                    all_files.append(dest)
                    self.p4_edit(src, "-k")  # src must be open before move but may not exist
                    self.p4_move(src, dest)  # opens for (move/delete, move/add)
                    if isModeExecChanged(diff['src_mode'], diff['dst_mode']):
                        filesToChangeExecBit[dest] = diff['dst_mode']
                    editedFiles.add(dest)
                elif modifier == "T":
                    filesToChangeType.add(path)
                else:
                    raise Exception("unknown modifier %s for %s" % (modifier, path))
        
            for f in filesToChangeType:
                self.p4_edit(f, "-t", "auto")
            for f in filesToAdd:
                self.p4_add(f)
            for f in filesToDelete:
                self.p4_revert(f)
                self.p4_delete(f)

            # Set/clear executable bits
            # TODO
            # for f in filesToChangeExecBit.keys():
            #     mode = filesToChangeExecBit[f]
            #     setP4ExecBit(f, mode)

        openedFiles = self.p4cmd('opened')
        lenOpenedFiles = len(openedFiles)
        if lenOpenedFiles > 0:
            self.logger.debug("Opened files: %d" % lenOpenedFiles)
            # self.fixFileTypes(fileRevs, openedFiles)
            description = self.formatChangeDescription(
                sourceDescription=commit.description,
                sourceChange=commit.commitID, sourcePort='git_repo',
                sourceUser=commit.name)
            newChangeId = 0
            result = None
            try:
                # Debug for larger changelists
                if lenOpenedFiles > 1000:
                    self.logger.debug("About to fetch change")
                chg = self.p4.fetch_change()
                chg['Description'] = description
                if lenOpenedFiles > 1000:
                    self.logger.debug("About to submit")
                result = self.p4.save_submit(chg)
                a = -1
                while 'submittedChange' not in result[a]:
                    a -= 1
                newChangeId = result[a]['submittedChange']
                if lenOpenedFiles > 1000:
                    self.logger.debug("submitted")
                self.logger.debug(self.p4id, result)
                self.checkWarnings()
            except P4.P4Exception as e:
                raise e

            if newChangeId:
                self.logger.info("source = {} : target  = {}".format(commit.commitID, newChangeId))
                description = self.formatChangeDescription(
                    sourceDescription=commit.description,
                    sourceChange=commit.commitID, sourcePort='git_repo',
                    sourceUser=commit.name)
                self.updateChange(newChangeId=newChangeId, description=description)
            else:
                self.logger.error("failed to replicate change {}".format(commit))
            return newChangeId

    def updateChange(self, newChangeId, description):
        # need to update the user and time stamp - but only if a superuser
        if not self.options.superuser == "y":
            return
        newChange = self.p4.fetch_change(newChangeId)
        newChange._description = description
        self.p4.save_change(newChange, '-f')

    def getCounter(self):
        "Returns value of counter"
        result = self.p4cmd('counter', self.options.counter_name)
        if result and 'counter' in result[0]:
            return result[0]['value']
        return ''

    def setCounter(self, value):
        "Set's the counter to specified value"
        self.p4cmd('counter', self.options.counter_name, str(value))


def valid_datetime_type(arg_datetime_str):
    """custom argparse type for user datetime values given from the command line"""
    try:
        return datetime.strptime(arg_datetime_str, "%Y/%m/%d %H:%M")
    except ValueError:
        msg = "Given Datetime ({0}) not valid! Expected format, 'YYYY/MM/DD HH:mm'!".format(arg_datetime_str)
        raise argparse.ArgumentTypeError(msg)


class GitP4Transfer(object):
    "Main transfer class"

    def __init__(self, *args):
        desc = textwrap.dedent(__doc__)
        parser = argparse.ArgumentParser(
            description=desc,
            formatter_class=argparse.RawDescriptionHelpFormatter,
            epilog="Copyright (C) 2021 Robert Cowham, Perforce Software Ltd"
        )

        parser.add_argument('-c', '--config', default=CONFIG_FILE, help="Default is " + CONFIG_FILE)
        parser.add_argument('-n', '--notransfer', action='store_true',
                            help="Validate config file and setup source/target workspaces but don't transfer anything")
        parser.add_argument('-m', '--maximum', default=None, type=int, help="Maximum number of changes to transfer")
        parser.add_argument('-r', '--repeat', action='store_true',
                            help="Repeat transfer in a loop - for continuous transfer as background task")
        parser.add_argument('-s', '--stoponerror', action='store_true', help="Stop on any error even if --repeat has been specified")
        parser.add_argument('--sample-config', action='store_true', help="Print an example config file and exit")
        parser.add_argument('--end-datetime', type=valid_datetime_type, default=None,
                            help="Time to stop transfers, format: 'YYYY/MM/DD HH:mm' - useful"
                            " for automation runs during quiet periods e.g. run overnight but stop first thing in the morning")
        self.options = parser.parse_args(list(args))

        if self.options.sample_config:
            printSampleConfig()
            return

        self.logger = logutils.getLogger(LOGGER_NAME)
        self.previous_target_change_counter = 0     # Current value

    def getOption(self, section, option_name, default=None):
        result = default
        try:
            if section == GENERAL_SECTION:
                result = self.config[option_name]
            else:
                result = self.config[section][option_name]
        except Exception:
            pass
        return result

    def getIntOption(self, section, option_name, default=None):
        result = default
        val = self.getOption(section, option_name, default)
        if isinstance(val, int):
            return val
        if val:
            try:
                result = int(eval(val))
            except Exception:
                pass
        return result

    def readConfig(self):
        self.config = {}
        try:
            with open(self.options.config) as f:
                self.config = yaml.load(f)
        except Exception as e:
            raise P4TConfigException('Could not read config file %s: %s' % (self.options.config, str(e)))

        errors = []
        self.options.counter_name = self.getOption(GENERAL_SECTION, "counter_name")
        if not self.options.counter_name:
            errors.append("Option counter_name must be specified")
        self.options.instance_name = self.getOption(GENERAL_SECTION, "instance_name", self.options.counter_name)
        self.options.mail_form_url = self.getOption(GENERAL_SECTION, "mail_form_url")
        self.options.mail_to = self.getOption(GENERAL_SECTION, "mail_to")
        self.options.mail_from = self.getOption(GENERAL_SECTION, "mail_from")
        self.options.mail_server = self.getOption(GENERAL_SECTION, "mail_server")
        self.options.sleep_on_error_interval = self.getIntOption(GENERAL_SECTION, "sleep_on_error_interval", 60)
        self.options.poll_interval = self.getIntOption(GENERAL_SECTION, "poll_interval", 60)
        self.options.change_batch_size = self.getIntOption(GENERAL_SECTION, "change_batch_size", 1000)
        self.options.report_interval = self.getIntOption(GENERAL_SECTION, "report_interval", 30)
        self.options.error_report_interval = self.getIntOption(GENERAL_SECTION, "error_report_interval", 30)
        self.options.summary_report_interval = self.getIntOption(GENERAL_SECTION, "summary_report_interval", 10080)
        self.options.max_logfile_size = self.getIntOption(GENERAL_SECTION, "max_logfile_size", 20 * 1024 * 1024)
        self.options.change_description_format = self.getOption(
            GENERAL_SECTION, "change_description_format",
            "$sourceDescription\n\nTransferred from git://$sourceRepo@$sourceChange")
        self.options.superuser = self.getOption(GENERAL_SECTION, "superuser", "y")
        self.options.branch_maps = self.getOption(GENERAL_SECTION, "branch_maps")
        if not self.options.branch_maps:
            errors.append("Option branch_maps must not be empty")
        self.options.anon_branches_root = self.getOption(GENERAL_SECTION, "anon_branches_root")
        self.options.import_anon_branches = self.getOption(GENERAL_SECTION, "import_anon_branches", "n")
        self.options.ignore_files = self.getOption(GENERAL_SECTION, "ignore_files")
        self.options.re_ignore_files = []
        if self.options.ignore_files:
            for exp in self.options.ignore_files:
                try:
                    self.options.re_ignore_files.append(re.compile(exp))
                except Exception as e:
                    errors.append("Failed to parse ignore_files: %s" % str(e))
        if errors:
            raise P4TConfigException("\n".join(errors))

        self.source = GitSource(SOURCE_SECTION, self.options)
        self.target = P4Target(TARGET_SECTION, self.options, self.source)

        self.readOption('git_repo', self.source)
        self.readP4Section(self.target)

    def readP4Section(self, p4config):
        if p4config.section in self.config:
            self.readOptions(p4config)
        else:
            raise P4TConfigException('Config file needs section %s' % p4config.section)

    def readOptions(self, p4config):
        self.readOption('P4CLIENT', p4config)
        self.readOption('P4USER', p4config)
        self.readOption('P4PORT', p4config)
        self.readOption('P4PASSWD', p4config, optional=True)
        self.readOption('P4CHARSET', p4config, optional=True)

    def readOption(self, option, p4config, optional=False):
        lcOption = option.lower()
        if lcOption in self.config[p4config.section]:
            p4config.__dict__[option] = self.config[p4config.section][lcOption]
        elif not optional:
            raise P4TConfigException('Required option %s not found in section %s' % (option, p4config.section))

    def revertOpenedFiles(self):
        "Clear out any opened files from previous errors - hoping they are transient - except for change_map"
        with self.target.p4.at_exception_level(P4.P4.RAISE_NONE):
            self.target.p4cmd('revert', "-k", "//%s/..." % self.target.P4CLIENT)

    def replicate_commits(self):
        "Perform a replication loop"
        os.chdir(self.source.git_repo)
        self.target.connect('target replicate')
        self.target.createClientWorkspace()
        commitIDs, commits = self.source.missingCommits(self.target.getCounter())
        if self.options.notransfer:
            self.logger.info("Would transfer %d commits - stopping due to --notransfer" % len(commitIDs))
            return 0
        self.logger.info("Transferring %d changes" % len(commitIDs))
        changesTransferred = 0
        if len(commitIDs) > 0:
            self.save_previous_target_change_counter()
            self.checkRotateLogFile()
            self.revertOpenedFiles()
            for id in commitIDs:
                if self.endDatetimeExceeded():  # Bail early
                    self.logger.info("Transfer stopped due to --end-datetime being exceeded")
                    return changesTransferred
                msg = 'Processing commit: {}'.format(id)
                self.logger.info(msg)
                commit = commits[id]
                self.source.checkoutCommit(id)
                self.target.replicateCommit(commit)
                self.target.setCounter(id)
                changesTransferred += 1
        self.target.disconnect()
        return changesTransferred

    def log_exception(self, e):
        "Log exceptions appropriately"
        etext = str(e)
        if re.search("WSAETIMEDOUT", etext, re.MULTILINE) or re.search("WSAECONNREFUSED", etext, re.MULTILINE):
            self.logger.error(etext)
        else:
            self.logger.exception(e)

    def save_previous_target_change_counter(self):
        "Save the latest change transferred to the target"
        chg = self.target.p4cmd('changes', '-m1', '-ssubmitted', '//{client}/...'.format(client=self.target.P4CLIENT))
        if chg:
            self.previous_target_change_counter = int(chg[0]['change']) + 1

    def send_summary_email(self, time_last_summary_sent, change_last_summary_sent):
        "Send an email summarising changes transferred"
        time_str = p4time(time_last_summary_sent)
        self.target.connect('target replicate')
        # Combine changes reported by time or since last changelist transferred
        changes = self.target.p4cmd('changes', '-l', '//{client}/...@{rev},#head'.format(
                client=self.target.P4CLIENT, rev=time_str))
        chgnums = [chg['change'] for chg in changes]
        counter_changes = self.target.p4cmd('changes', '-l', '//{client}/...@{rev},#head'.format(
                client=self.target.P4CLIENT, rev=change_last_summary_sent))
        for chg in counter_changes:
            if chg['change'] not in chgnums:
                changes.append(chg)
        changes.reverse()
        lines = []
        lines.append(["Date", "Time", "Changelist", "File Revisions", "Size (bytes)", "Size"])
        total_changes = 0
        total_rev_count = 0
        total_file_sizes = 0
        for chg in changes:
            sizes = self.target.p4cmd('sizes', '-s', '//%s/...@%s,%s' % (self.target.P4CLIENT,
                                                                         chg['change'], chg['change']))
            lines.append([time.strftime("%Y/%m/%d", time.localtime(int(chg['time']))),
                         time.strftime("%H:%M:%S", time.localtime(int(chg['time']))),
                         chg['change'], sizes[0]['fileCount'], sizes[0]['fileSize'],
                         fmtsize(int(sizes[0]['fileSize']))])
            total_changes += 1
            total_rev_count += int(sizes[0]['fileCount'])
            total_file_sizes += int(sizes[0]['fileSize'])
        lines.append([])
        lines.append(['Totals', '', str(total_changes), str(total_rev_count), str(total_file_sizes), fmtsize(total_file_sizes)])
        report = "Changes transferred since %s\n%s" % (
            time_str, "\n".join(["\t".join(line) for line in lines]))
        self.logger.debug("Transfer summary report:\n%s" % report)
        self.logger.info("Sending Transfer summary report")
        self.logger.notify("Transfer summary report", report, include_output=False)
        self.save_previous_target_change_counter()
        self.target.disconnect()

    def validateConfig(self):
        "Performs appropriate validation of config values - primarily streams"
        pass

    def setupReplicate(self):
        "Read config file and setup - raises exceptions if invalid"
        self.readConfig()
        self.target.connect('target replicate')
        self.validateConfig()
        self.logger.debug("connected to source and target")
        self.target.createClientWorkspace()

    def writeLogHeader(self):
        "Write header info to log"
        logOnce(self.logger, VERSION)
        logOnce(self.logger, "Python ver: 0x%08x, OS: %s" % (sys.hexversion, sys.platform))
        logOnce(self.logger, "P4Python ver: %s" % (P4.P4.identify()))
        logOnce(self.logger, "Options: ", self.options)
        logOnce(self.logger, "Reading config file")

    def rotateLogFile(self):
        "Rotate existing log file"
        self.logger.info("Rotating logfile")
        logutils.resetLogger(LOGGER_NAME)
        global alreadyLogged
        alreadyLogged = {}
        self.writeLogHeader()

    def checkRotateLogFile(self):
        "Rotate log file if greater than limit"
        try:
            fname = logutils.getCurrentLogFileName(LOGGER_NAME)
            fsize = os.path.getsize(fname)
            if fsize > self.options.max_logfile_size:
                self.logger.info("Rotating logfile since greater than max_logfile_size: %d" % fsize)
                self.rotateLogFile()
        except Exception as e:
            self.log_exception(e)

    def endDatetimeExceeded(self):
        """Determine if we should stop due to this being set"""
        if not self.options.end_datetime:
            return False
        present = datetime.now()
        return present > self.options.end_datetime

    def replicate(self):
        """Central method that performs the replication between server1 and server2"""
        if self.options.sample_config:
            return 0
        try:
            self.writeLogHeader()
            self.setupReplicate()
        except Exception as e:
            self.log_exception(e)
            logging.shutdown()
            return 1

        self.options.config = os.path.realpath(self.options.config)

        time_last_summary_sent = time.time()
        change_last_summary_sent = 0
        self.logger.debug("Time last summary sent: %s" % p4time(time_last_summary_sent))
        time_last_error_occurred = 0
        error_encountered = False   # Flag to indicate error encountered which may require reporting
        error_notified = False
        finished = False
        num_changes = 0
        while not finished:
            try:
                self.readConfig()       # Read every time to allow user to change them
                self.logger.setReportingOptions(
                    instance_name=self.options.instance_name,
                    mail_form_url=self.options.mail_form_url, mail_to=self.options.mail_to,
                    mail_from=self.options.mail_from, mail_server=self.options.mail_server,
                    report_interval=self.options.report_interval)
                logOnce(self.logger, self.source.options)
                logOnce(self.logger, self.target.options)
                self.source.disconnect()
                self.target.disconnect()
                num_changes = self.replicate_commits()
                if self.options.notransfer:
                    finished = True
                if num_changes > 0:
                    self.logger.info("Transferred %d changes successfully" % num_changes)
                if change_last_summary_sent == 0:
                    change_last_summary_sent = self.previous_target_change_counter
                if self.options.change_batch_size and num_changes >= self.options.change_batch_size:
                    self.logger.info("Finished processing batch of %d changes" % self.options.change_batch_size)
                    self.rotateLogFile()
                elif not self.options.repeat:
                    finished = True
                else:
                    if self.endDatetimeExceeded():
                        finished = True
                        self.logger.info("Stopping due to --end-datetime parameter being exceeded")
                    if error_encountered:
                        self.logger.info("Logging - reset error interval")
                        self.logger.notify("Cleared error", "Previous error has now been cleared")
                        error_encountered = False
                        error_notified = False
                    if time.time() - time_last_summary_sent > self.options.summary_report_interval * 60:
                        time_last_summary_sent = time.time()
                        self.send_summary_email(time_last_summary_sent, change_last_summary_sent)
                    time.sleep(self.options.poll_interval * 60)
                    self.logger.info("Sleeping for %d minutes" % self.options.poll_interval)
            except P4TException as e:
                self.log_exception(e)
                self.logger.notify("Error", "Logic Exception encountered - stopping")
                logging.shutdown()
                return 1
            except Exception as e:
                self.log_exception(e)
                if self.options.stoponerror:
                    self.logger.notify("Error", "Exception encountered and --stoponerror specified")
                    logging.shutdown()
                    return 1
                else:
                    # Decide whether to report an error
                    if not error_encountered:
                        error_encountered = True
                        time_last_error_occurred = time.time()
                    elif not error_notified:
                        if time.time() - time_last_error_occurred > self.options.error_report_interval * 60:
                            error_notified = True
                            self.logger.info("Logging - Notifying recurring error")
                            self.logger.notify("Recurring error", "Multiple errors seen")
                    self.logger.info("Sleeping on error for %d minutes" % self.options.sleep_on_error_interval)
                    time.sleep(self.options.sleep_on_error_interval * 60)
        self.logger.notify("Changes transferred", "Completed successfully")
        logging.shutdown()
        return 0


if __name__ == '__main__':
    result = 0
    try:
        prog = GitP4Transfer(*sys.argv[1:])
        result = prog.replicate()
    except Exception as e:
        print(str(e))
        result = 1
    sys.exit(result)
