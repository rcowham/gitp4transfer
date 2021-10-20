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

    Usage:

        python3 GitP4Transfer.py -h

    The script requires a config file, by default transfer.yaml.

    An initial example can be generated, e.g.

        GitP4Transfer.py --sample-config > transfer.yaml

    For full documentation/usage, see project doc:

        https://github.com/rcowham/gitp4transfer/blob/main/doc/GitP4Transfer.adoc

"""

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

# Non-standard modules
import P4
import logutils

# Import yaml which will roundtrip comments
from ruamel.yaml import YAML
yaml = YAML()

VERSION = """$Id: 74939df934a7a660e6beff62870f65635918300b $"""


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
#    Note that other branches encountered will be given temp names under temp_branches_root
#    Entries specify git_branch and targ. No wildcards.
branch_maps:
  - git_branch:  "master"
    targ:   "//git_import/master"

# temp_branches_root: A depot path used for temp branches (names automatically generated).
#   Mandatory.
#   Such branches only contain files modified on git branch.
#   Name is something like git_temp_<suffix>
temp_branches_root: //git_import/temp_branches

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
        for v in self.options.branch_maps:
            exclude = ''
            # srcPath = v['git_branch']
            line = "%s%s/... //%s/..." % (exclude, v['targ'], self.p4.client)
            clientView.append(line)
            for exclude in ['.git*', '.git/...']:
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


class TempBranch:
    "Alternates between branch names - allowing old one to be deleted"

    def __init__(self):
        self.tempBranches = ['p4_export1', 'p4_export2']
        self.ind = -1
        self.currBranch = ''
        self.oldBranch = ''

    def getNext(self):
        "Swap temp branches, and we delete old one after"
        self.ind = (self.ind + 1) % 2
        if self.currBranch:
            self.oldBranch = self.currBranch
        self.currBranch = self.tempBranches[self.ind]
        return self.currBranch

    def getOld(self):
        return self.oldBranch


tempBranch = TempBranch()


class GitCommit():
    "Parse commit from git log output"

    # commit <hash>
    # Author: <author>
    # Date:   <author date>
    #
    # <title line>
    #
    # <full commit message>
    def __init__(self, logLines) -> None:
        self.hash = self.author = self.date = self.description = ""
        startDesc = False
        for line in logLines.split("\n"):
            line = line.strip()
            if line.startswith("commit "):
                self.hash = line[7:]
            elif line.startswith("Author: "):
                self.author = line[8:]
            elif line.startswith("Date: "):
                self.date = line[6:]
            elif not line and not startDesc:
                startDesc = True
            elif startDesc:
                self.description += line

class GitSource(P4Base):
    "Functionality for reading from source Perforce repository"

    def __init__(self, section, options):
        super(GitSource, self).__init__(section, options, 'src')

    def run_cmd(self, cmd, dir=".", get_output=True, timeout=35, stop_on_error=True):
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
                result = subprocess.check_call(cmd, stderr=subprocess.STDOUT, shell=True, timeout=timeout)
                self.logger.debug('Result: %d' % result)
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
        revRange = ''
        if counter and counter != '0':
            revRange = '{rev}..HEAD'.format(rev=counter)
        maxChanges = 0
        if self.options.change_batch_size:
            maxChanges = self.options.change_batch_size
        if self.options.maximum and self.options.maximum < maxChanges:
            maxChanges = self.options.maximum
        args = ['git', 'rev-list', '--reverse']
        if maxChanges > 0:
            args.append('--max-count=%d' % maxChanges)
        args.append(revRange)
        args.append('master')
        self.logger.debug('reading changes: %s' % ' '.join(args))
        commits = self.run_cmd(' '.join(args)).split('\n')
        commits = [x for x in commits if x]
        self.logger.debug('processing %d commits' % len(commits))
        return commits

    def getCommit(self, commitID):
        """returns commit members"""
        args = ['git', 'log', '--max-count=1', commitID]
        logLines = self.run_cmd(' '.join(args))
        return GitCommit(logLines=logLines)

    def checkoutCommit(self, commitID):
        """Expects change number as a string, and returns list of filerevs"""
        args = ['git', 'checkout', '-b', tempBranch.getNext(), commitID]
        self.run_cmd(' '.join(args))
        if tempBranch.getOld():
            args = ['git', 'branch', '-D', '-f', tempBranch.getOld()]
            self.run_cmd(' '.join(args))

class P4Target(P4Base):
    "Functionality for transferring changes to target Perforce repository"

    def __init__(self, section, options, source):
        super(P4Target, self).__init__(section, options, 'targ')
        self.source = source
        self.filesToIgnore = []

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

    def replicateCommit(self, commit):
        """This is the heart of it all. Replicate a single commit/change"""

        self.filesToIgnore = []
        self.p4cmd('reconcile', '-mead')
        openedFiles = self.p4cmd('opened')
        lenOpenedFiles = len(openedFiles)
        if lenOpenedFiles > 0:
            self.logger.debug("Opened files: %d" % lenOpenedFiles)
            # self.fixFileTypes(fileRevs, openedFiles)
            description = self.formatChangeDescription(
                sourceDescription=commit.description,
                sourceChange=commit.hash, sourcePort='git_repo',
                sourceUser=commit.author)
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
            self.logger.info("source = {} : target  = {}".format(commit, newChangeId))
            description = self.formatChangeDescription(
                sourceDescription=commit.description,
                sourceChange=commit.hash, sourcePort='git_repo',
                sourceUser=commit.author)
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
            epilog="Copyright (C) 2012-21 Sven Erik Knop/Robert Cowham, Perforce Software Ltd"
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
        self.target.connect('target replicate')
        self.target.createClientWorkspace()
        commitIDs = self.source.missingCommits(self.target.getCounter())
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
                self.source.checkoutCommit(id)
                commit = self.source.getCommit(id)
                self.logger.info(msg)
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
