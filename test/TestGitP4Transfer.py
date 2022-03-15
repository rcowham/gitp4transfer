# -*- encoding: UTF8 -*-
# Tests for the GitP4Transfer.py module.

from __future__ import print_function

import sys
import time
import P4
import subprocess
import inspect
import platform
from textwrap import dedent
import unittest
import os
import shutil
import stat
import re
import glob
import argparse
import datetime
from ruamel.yaml import YAML

# Bring in module to be tested
sys.path.append(os.path.join(os.path.dirname(os.path.abspath(__file__)), '..'))
import logutils         # noqa: E402
import GitP4Transfer   # noqa: E402

yaml = YAML()

python3 = sys.version_info[0] >= 3
if sys.hexversion < 0x02070000 or (0x0300000 < sys.hexversion < 0x0303000):
    sys.exit("Python 2.7 or 3.3 or newer is required to run this program.")

if python3:
    from io import StringIO
else:
    from StringIO import StringIO

P4D = "p4d"     # This can be overridden via command line stuff
P4USER = "testuser"
P4CLIENT = "test_ws"
TEST_ROOT = '_testrun_transfer'
TRANSFER_CLIENT = "transfer"
TRANSFER_TARGET_REMOTE = "transfer_remote"
TRANSFER_CONFIG = "transfer.yaml"
TEST_REPO_NAME = 'git_repo'

TEST_COUNTER_NAME = "GitP4Transfer"
INTEG_ENGINE = 3

saved_stdoutput = StringIO()
test_logger = None


def onRmTreeError(function, path, exc_info):
    os.chmod(path, stat.S_IWRITE)
    os.remove(path)


def ensureDirectory(directory):
    if not os.path.isdir(directory):
        os.makedirs(directory)


def localDirectory(root, *dirs):
    "Create and ensure it exists"
    dir_path = os.path.join(root, *dirs)
    ensureDirectory(dir_path)
    return dir_path


def create_file(file_name, contents):
    "Create file with specified contents"
    ensureDirectory(os.path.dirname(file_name))
    if python3:
        contents = bytes(contents.encode())
    with open(file_name, 'wb') as f:
        f.write(contents)


def append_to_file(file_name, contents):
    "Append contents to file"
    if python3:
        contents = bytes(contents.encode())
    with open(file_name, 'ab+') as f:
        f.write(contents)


def getP4ConfigFilename():
    "Returns os specific filename"
    if 'P4CONFIG' in os.environ:
        return os.environ['P4CONFIG']
    if os.name == "nt":
        return "p4config.txt"
    return ".p4config"


def run_cmd(cmd, dir=".", get_output=True, timeout=2*60*60, stop_on_error=True):
    "Run cmd logging input and output"
    output = ""
    try:
        test_logger.debug("Running: %s" % cmd)
        if get_output:
            p = subprocess.Popen(cmd, cwd=dir, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, universal_newlines=True, shell=True)
            if python3:
                output, _ = p.communicate(timeout=timeout)
            else:
                output, _ = p.communicate()
            # rc = p.returncode
            test_logger.debug("Output:\n%s" % output)
        else:
            result = subprocess.check_call(cmd, stderr=subprocess.STDOUT, shell=True, timeout=timeout)
            test_logger.debug('Result: %d' % result)
    except subprocess.CalledProcessError as e:
        test_logger.debug("Output: %s" % e.output)
        if stop_on_error:
            msg = 'Failed run_cmd: %d %s' % (e.returncode, str(e))
            test_logger.debug(msg)
            raise e
    except Exception as e:
        test_logger.debug("Output: %s" % output)
        if stop_on_error:
            msg = 'Failed run_cmd: %s' % str(e)
            test_logger.debug(msg)
            raise e
    return output


class TestServer:
    def __init__(self, logger):
        self.logger = logger

    def run_cmd(self, cmd, dir=".", get_output=True, timeout=35, stop_on_error=True):
        run_cmd(cmd, dir, get_output=get_output, timeout=timeout, stop_on_error=stop_on_error)

class GitRepo(TestServer):
    def __init__(self, root, logger):
        self.root = root
        super(GitRepo, self).__init__(logger)
        self.repo_root = os.path.join(root, TEST_REPO_NAME)

        ensureDirectory(self.repo_root)

        os.chdir(self.repo_root)
        self.run_cmd("git init .")
        self.run_cmd("git checkout main")

class P4Server(TestServer):
    def __init__(self, root, logger):
        self.root = root
        self.logger = logger
        super(P4Server, self).__init__(logger)
        self.server_root = os.path.join(root, "server")
        self.client_root = os.path.join(root, "client")

        ensureDirectory(self.root)
        ensureDirectory(self.server_root)
        ensureDirectory(self.client_root)

        self.p4d = P4D
        self.port = "rsh:%s -r \"%s\" -L log -i" % (self.p4d, self.server_root)
        self.p4 = P4.P4()
        self.p4.port = self.port
        self.p4.user = P4USER
        self.p4.client = P4CLIENT
        self.p4.connect()

        self.p4cmd('depots')  # triggers creation of the user

        self.p4.disconnect()  # required to pick up the configure changes
        self.p4.connect()

        self.client_name = P4CLIENT
        client = self.p4.fetch_client(self.client_name)
        client._root = self.client_root
        client._lineend = 'unix'
        self.p4.save_client(client)

    def shutDown(self):
        if self.p4.connected():
            self.p4.disconnect()

    def createTransferClient(self, name, root):
        pass

    def enableUnicode(self):
        cmd = '%s -r "%s" -L log -vserver=3 -xi' % (self.p4d, self.server_root)
        output = self.run_cmd(cmd, dir=self.server_root, get_output=True, stop_on_error=True)
        self.logger.debug(output)

    def getCounter(self):
        "Returns value of counter"
        result = self.p4.run('counter', TEST_COUNTER_NAME)
        if result and 'counter' in result[0]:
            return result[0]['value']
        return 0

    def p4cmd(self, *args):
        "Execute p4 cmd while logging arguments and results"
        if not self.logger:
            self.logger = logutils.getLogger(GitP4Transfer.LOGGER_NAME)
        self.logger.debug('testp4:', args)
        output = self.p4.run(args)
        self.logger.debug('testp4r:', output)
        return output


class TestGitP4Transfer(unittest.TestCase):

    def __init__(self, methodName='runTest'):
        global saved_stdoutput, test_logger
        saved_stdoutput.truncate(0)
        if test_logger is None:
            test_logger = logutils.getLogger(GitP4Transfer.LOGGER_NAME, stream=saved_stdoutput)
        else:
            logutils.resetLogger(GitP4Transfer.LOGGER_NAME)
        self.logger = test_logger
        super(TestGitP4Transfer, self).__init__(methodName=methodName)

    def assertRegex(self, *args, **kwargs):
        if python3:
            return super(TestGitP4Transfer, self).assertRegex(*args, **kwargs)
        else:
            return super(TestGitP4Transfer, self).assertRegexpMatches(*args, **kwargs)

    def assertContentsEqual(self, expected, content):
        if python3:
            content = content.decode()
        self.assertEqual(expected, content)

    def setUp(self):
        self.setDirectories()

    def tearDown(self):
        self.target.shutDown()
        time.sleep(0.1)
        # self.cleanupTestTree()

    def setDirectories(self):
        self.startdir = os.getcwd()
        self.transfer_root = os.path.join(self.startdir, TEST_ROOT)
        self.cleanupTestTree()

        ensureDirectory(self.transfer_root)

        self.source = GitRepo(self.transfer_root, self.logger)
        self.target = P4Server(os.path.join(self.transfer_root, 'target'), self.logger)

        self.transfer_client_root = localDirectory(self.transfer_root, TEST_REPO_NAME)
        self.writeP4Config()

    def writeP4Config(self):
        "Write appropriate files - useful for occasional manual debugging"
        p4config_filename = getP4ConfigFilename()
        targP4Config = os.path.join(self.transfer_root, 'target', p4config_filename)
        transferP4Config = os.path.join(self.transfer_client_root, p4config_filename)
        with open(targP4Config, "w") as fh:
            fh.write('P4PORT=%s\n' % self.target.port)
            fh.write('P4USER=%s\n' % self.target.p4.user)
            fh.write('P4CLIENT=%s\n' % self.target.p4.client)
        with open(transferP4Config, "w") as fh:
            fh.write('P4PORT=%s\n' % self.target.port)
            fh.write('P4USER=%s\n' % self.target.p4.user)
            fh.write('P4CLIENT=%s\n' % TRANSFER_CLIENT)

    def cleanupTestTree(self):
        os.chdir(self.startdir)
        if os.path.isdir(self.transfer_root):
            shutil.rmtree(self.transfer_root, False, onRmTreeError)

    def getDefaultOptions(self):
        config = {}
        config['transfer_client'] = TRANSFER_CLIENT
        config['target_remote'] = TRANSFER_TARGET_REMOTE
        config['workspace_root'] = self.transfer_client_root
        config['import_anon_branches'] = 'n'
        config['anon_branches_root'] = ''
        config['branch_maps'] = [{'git_branch': 'main',
                            'targ': '//depot/import'}]
        return config

    def setupTransfer(self):
        """Creates a config file with default mappings"""
        msg = "Test: %s ======================" % inspect.stack()[1][3]
        self.logger.debug(msg)
        config = self.getDefaultOptions()
        self.createConfigFile(options=config)

    def createConfigFile(self, srcOptions=None, targOptions=None, options=None):
        "Creates config file with extras if appropriate"
        if options is None:
            options = {}
        if srcOptions is None:
            srcOptions = {}
        if targOptions is None:
            targOptions = {}

        config = {}
        config['source'] = {}
        config['source']['git_repo'] = os.path.join(self.source.root, TEST_REPO_NAME)
        for opt in srcOptions.keys():
            config['source'][opt] = srcOptions[opt]

        config['target'] = {}
        config['target']['p4port'] = self.target.port
        config['target']['p4user'] = P4USER
        config['target']['p4client'] = TRANSFER_CLIENT
        for opt in targOptions.keys():
            config['target'][opt] = targOptions[opt]

        config['logfile'] = os.path.join(self.transfer_root, 'temp', 'test.log')
        if not os.path.exists(os.path.join(self.transfer_root, 'temp')):
            os.mkdir(os.path.join(self.transfer_root, 'temp'))
        config['counter_name'] = TEST_COUNTER_NAME

        for opt in options.keys():
            config[opt] = options[opt]

        # write the config file
        self.transfer_cfg = os.path.join(self.transfer_root, TRANSFER_CONFIG)
        with open(self.transfer_cfg, 'w') as f:
            yaml.dump(config, f)

    def run_GitP4Transfer(self, *args):
        base_args = ['-c', self.transfer_cfg, '-s']
        if args:
            base_args.extend(args)
        pt = GitP4Transfer.GitP4Transfer(*base_args)
        result = pt.replicate()
        return result

    def assertCounters(self, sourceValue, targetValue):
        commits = run_cmd('git rev-list main').split('\n')
        commits = [x for x in commits if x]
        sourceCounter = len(commits)
        targetCounter = len(self.target.p4.run("changes"))
        self.assertEqual(sourceCounter, sourceValue, "Source counter is not {} but {}".format(sourceValue, sourceCounter))
        self.assertEqual(targetCounter, targetValue, "Target counter is not {} but {}".format(targetValue, targetCounter))

    # def testArgParsing(self):
    #     "Basic argparsing for the module"
    #     self.setupTransfer()
    #     args = ['-c', self.transfer_cfg, '-s']
    #     pt = GitP4Transfer.GitP4Transfer(*args)
    #     self.assertEqual(pt.options.config, self.transfer_cfg)
    #     self.assertTrue(pt.options.stoponerror)
    #     args = ['-c', self.transfer_cfg]
    #     pt = GitP4Transfer.GitP4Transfer(*args)

    # def testArgParsingErrors(self):
    #     "Basic argparsing for the module"
    #     self.setupTransfer()
    #     # args = ['-c', self.transfer_cfg, '--end-datetime', '2020/1/1']
    #     # try:
    #     #     self.assertTrue(False, "Failed to get expected exception")
    #     # except Exception as e:
    #     #     pass
    #     args = ['-c', self.transfer_cfg, '--end-datetime', '2020/1/1 13:01']
    #     pt = GitP4Transfer.GitP4Transfer(*args)
    #     self.assertEqual(pt.options.config, self.transfer_cfg)
    #     self.assertFalse(pt.options.stoponerror)
    #     self.assertEqual(datetime.datetime(2020, 1, 1, 13, 1), pt.options.end_datetime)
    #     self.assertTrue(pt.endDatetimeExceeded())
    #     args = ['-c', self.transfer_cfg, '--end-datetime', '2040/1/1 13:01']
    #     pt = GitP4Transfer.GitP4Transfer(*args)
    #     self.assertEqual(pt.options.config, self.transfer_cfg)
    #     self.assertFalse(pt.options.stoponerror)
    #     self.assertEqual(datetime.datetime(2040, 1, 1, 13, 1), pt.options.end_datetime)
    #     self.assertFalse(pt.endDatetimeExceeded())

    # def testMaximum(self):
    #     "Test  only max number of changes are transferred"
    #     self.setupTransfer()
    #     args = ['-c', self.transfer_cfg, '-m1']
    #     pt = GitP4Transfer.GitP4Transfer(*args)
    #     self.assertEqual(pt.options.config, self.transfer_cfg)

    #     inside = localDirectory(self.source.client_root, "inside")
    #     inside_file1 = os.path.join(inside, "inside_file1")
    #     create_file(inside_file1, 'Test content')

    #     self.source.p4cmd('add', inside_file1)
    #     desc = 'inside_file1 added'
    #     self.source.p4cmd('submit', '-d', desc)
    #     self.source.p4cmd('edit', inside_file1)
    #     append_to_file(inside_file1, "New line")
    #     desc = 'file edited'
    #     self.source.p4cmd('submit', '-d', desc)

    #     pt.replicate()
    #     self.assertCounters(1, 1)

    #     pt.replicate()
    #     self.assertCounters(2, 2)

    # def testConfigValidation(self):
    #     "Make sure specified config options such as client views are valid"
    #     msg = "Test: %s ======================" % inspect.stack()[0][3]
    #     self.logger.debug(msg)

    #     self.createConfigFile()

    #     msg = ""
    #     try:
    #         base_args = ['-c', self.transfer_cfg, '-s']
    #         pt = GitP4Transfer.GitP4Transfer(*base_args)
    #         pt.setupReplicate()
    #     except Exception as e:
    #         msg = str(e)
    #     self.assertRegex(msg, "One of options views/stream_views must be specified")
    #     self.assertRegex(msg, "Option workspace_root must not be blank")

    #     # Integer config related options
    #     config = self.getDefaultOptions()
    #     config['poll_interval'] = 5
    #     config['report_interval'] = "10 * 5"
    #     self.createConfigFile(options=config)

    #     base_args = ['-c', self.transfer_cfg, '-s']
    #     pt = GitP4Transfer.GitP4Transfer(*base_args)
    #     pt.setupReplicate()
    #     self.assertEqual(5, pt.options.poll_interval)
    #     self.assertEqual(50, pt.options.report_interval)

    #     # Other streams related validation
    #     config = self.getDefaultOptions()
    #     config['views'] = []
    #     config['stream_views'] = [{'src': '//src_streams/rel*',
    #                                'targ': '//targ_streams/rel*',
    #                                'type': 'release',
    #                                'parent': '//targ_streams/main'}]
    #     self.createConfigFile(options=config)

    #     msg = ""
    #     try:
    #         base_args = ['-c', self.transfer_cfg, '-s']
    #         pt = GitP4Transfer.GitP4Transfer(*base_args)
    #         pt.setupReplicate()
    #     except Exception as e:
    #         msg = str(e)
    #     self.assertRegex(msg, "Option transfer_target_stream must be specified if streams are being used")

    #     # Other streams related validation
    #     config = self.getDefaultOptions()
    #     config['views'] = []
    #     config['transfer_target_stream'] = ['//targ_streams/transfer_target_stream']
    #     config['stream_views'] = [{'fred': 'joe'}]
    #     self.createConfigFile(options=config)

    #     msg = ""
    #     try:
    #         base_args = ['-c', self.transfer_cfg, '-s']
    #         pt = GitP4Transfer.GitP4Transfer(*base_args)
    #         pt.setupReplicate()
    #     except Exception as e:
    #         msg = str(e)
    #     self.assertRegex(msg, "Missing required field 'src'")
    #     self.assertRegex(msg, "Missing required field 'targ'")
    #     self.assertRegex(msg, "Missing required field 'type'")
    #     self.assertRegex(msg, "Missing required field 'parent'")

    #     # Other streams related validation
    #     config = self.getDefaultOptions()
    #     config['views'] = []
    #     config['transfer_target_stream'] = ['//targ_streams/transfer_target_stream']
    #     config['stream_views'] = [{'src': '//src_streams/*rel*',
    #                                'targ': '//targ_streams/rel*',
    #                                'type': 'new',
    #                                'parent': '//targ_streams/main'}]
    #     self.createConfigFile(options=config)

    #     msg = ""
    #     try:
    #         base_args = ['-c', self.transfer_cfg, '-s']
    #         pt = GitP4Transfer.GitP4Transfer(*base_args)
    #         pt.setupReplicate()
    #     except Exception as e:
    #         msg = str(e)
    #     self.assertRegex(msg, "Wildcards need to match")
    #     self.assertRegex(msg, "Stream type 'new' is not one of allowed values")

    # def testChangeFormatting(self):
    #     "Formatting options for change descriptions"
    #     self.setupTransfer()
    #     inside = localDirectory(self.source.client_root, "inside")
    #     inside_file1 = os.path.join(inside, "inside_file1")
    #     create_file(inside_file1, 'Test content')

    #     self.source.p4cmd('add', inside_file1)
    #     desc = 'inside_file1 added'
    #     self.source.p4cmd('submit', '-d', desc)
    #     self.run_GitP4Transfer()
    #     self.assertCounters(1, 1)
    #     changes = self.target.p4cmd('changes', '-l', '-m1')
    #     self.assertRegex(changes[0]['desc'], "%s\n\nTransferred from p4://rsh:.*@1\n$" % desc)

    #     options = self.getDefaultOptions()
    #     options["change_description_format"] = "Originally $sourceChange by $sourceUser"
    #     self.createConfigFile(options=options)
    #     self.source.p4cmd('edit', inside_file1)
    #     desc = 'inside_file1 edited'
    #     self.source.p4cmd('submit', '-d', desc)
    #     self.run_GitP4Transfer()
    #     self.assertCounters(2, 2)
    #     changes = self.target.p4cmd('changes', '-l', '-m1')
    #     self.assertRegex(changes[0]['desc'], "Originally 2 by %s" % P4USER)

    #     options = self.getDefaultOptions()
    #     options["change_description_format"] = "Was $sourceChange by $sourceUser $fred\n$sourceDescription"
    #     self.createConfigFile(options=options)
    #     self.source.p4cmd('edit', inside_file1)
    #     desc = 'inside_file1 edited again'
    #     self.source.p4cmd('submit', '-d', desc)
    #     self.run_GitP4Transfer()
    #     self.assertCounters(3, 3)
    #     changes = self.target.p4cmd('changes', '-l', '-m1')
    #     self.assertEqual(changes[0]['desc'], "Was 3 by %s $fred\n%s\n" % (P4USER, desc))

    # def testBatchSize(self):
    #     "Set batch size appropriately - make sure logging switches"
    #     self.setupTransfer()
    #     inside = localDirectory(self.source.client_root, "inside")
    #     inside_file1 = os.path.join(inside, "inside_file1")
    #     create_file(inside_file1, 'Test content')

    #     self.source.p4cmd('add', inside_file1)
    #     self.source.p4cmd('submit', '-d', 'file added')

    #     for _ in range(1, 10):
    #         self.source.p4cmd('edit', inside_file1)
    #         append_to_file(inside_file1, "more")
    #         self.source.p4cmd('submit', '-d', 'edited')

    #     changes = self.source.p4cmd('changes', '-l', '-m1')
    #     self.assertEqual(changes[0]['change'], '10')

    #     options = self.getDefaultOptions()
    #     options["change_batch_size"] = "4"
    #     self.createConfigFile(options=options)

    #     self.run_GitP4Transfer()
    #     self.assertCounters(10, 10)

    #     logoutput = saved_stdoutput.getvalue()
    #     matches = re.findall("INFO: Logging to file:", logoutput)
    #     self.assertEqual(len(matches), 3)
            
    def testCommitDiffs(self):
        "Basic git info including file diffs"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        file2 = os.path.join(inside, "file2")
        create_file(file1, 'Test content')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')

        self.source.run_cmd('git checkout -b branch1')
        create_file(file2, 'Test content2')
        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "2nd change"')

        gitinfo = GitP4Transfer.GitInfo(test_logger)
        branches = ['main', 'branch1']
        commitList, commits = gitinfo.getCommitDiffs(branches)
        self.assertEqual(2, len(commitList))
        test_logger.debug("commits: %s" % ' '.join(commitList))
        self.assertEqual('2nd change', commits[commitList[0]].description)
        self.assertEqual('first change', commits[commitList[1]].description)
        fc = commits[commitList[0]].fileChanges
        self.assertEqual(1, len(fc))
        self.assertEqual('file2', fc[0].filenames[0])
        self.assertEqual('A', fc[0].changeTypes)
        self.assertEqual('first change', commits[commitList[1]].description)
        fc = commits[commitList[1]].fileChanges
        self.assertEqual(2, len(fc))
        self.assertEqual(commits[commitList[1]].commitID, commits[commitList[0]].parents[0])
        self.assertEqual('.p4config', fc[0].filenames[0])
        self.assertEqual('file1', fc[1].filenames[0])

    def testAdd(self):
        "Basic file add"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        create_file(file1, 'Test content')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')

        self.run_GitP4Transfer()

        changes = self.target.p4cmd('changes')
        self.assertEqual(1, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(1, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])

        self.assertCounters(1, 1)

    def testAddUnicode(self):
        "Basic file add with unicode chars"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1®")
        create_file(file1, 'Test content')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')

        self.run_GitP4Transfer()

        changes = self.target.p4cmd('changes')
        self.assertEqual(1, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(1, len(files))
        self.assertEqual('//depot/import/file1®', files[0]['depotFile'])

        self.assertCounters(1, 1)

    def testAdd2(self):
        "Basic 2 add commits"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        file2 = os.path.join(inside, "file2")
        create_file(file1, 'Test content')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')
        create_file(file2, 'Test content2')
        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "second change"')

        self.run_GitP4Transfer()

        changes = self.target.p4cmd('changes')
        self.assertEqual(2, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(2, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])
        self.assertEqual('//depot/import/file2', files[1]['depotFile'])

        self.assertCounters(2, 2)

    def testAdd2Seperate(self):
        "Basic 2 add commits in seperate runs"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        file2 = os.path.join(inside, "file2")
        create_file(file1, 'Test content')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')

        self.run_GitP4Transfer()
        self.assertCounters(1, 1)

        changes = self.target.p4cmd('changes')
        self.assertEqual(1, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(1, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])

        self.source.run_cmd('git checkout main')
        create_file(file2, 'Test content2')
        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "second change"')

        self.run_GitP4Transfer()
        self.assertCounters(2, 2)

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(2, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])
        self.assertEqual('//depot/import/file2', files[1]['depotFile'])
        self.run_GitP4Transfer()

        changes = self.target.p4cmd('changes')
        self.assertEqual(2, len(changes))

    def testAddEditDelete(self):
        "Basic add/edit and delete commits"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        file2 = os.path.join(inside, "file2")
        create_file(file1, 'Test content')
        create_file(file2, 'Test content2')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')

        append_to_file(file1, "\nMore stuff")
        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "edit change"')

        self.source.run_cmd('git rm file2')
        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "delete change"')

        self.run_GitP4Transfer()

        changes = self.target.p4cmd('changes')
        self.assertEqual(3, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(2, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])
        self.assertEqual('//depot/import/file2', files[1]['depotFile'])

        self.assertCounters(3, 3)

        filelogs = self.target.p4cmd('filelog', '//depot/...')
        self.assertEqual(2, len(filelogs))
        self.assertEqual('edit', filelogs[0]['action'][0])
        self.assertEqual('add', filelogs[0]['action'][1])
        self.assertEqual('delete', filelogs[1]['action'][0])
        self.assertEqual('add', filelogs[1]['action'][1])

        result = self.target.p4cmd('print', '//depot/import/file1')
        self.assertEqual(b'Test content\nMore stuff', result[1])

    def testAddSimpleBranch(self):
        "Basic simple branch and merge of a file"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        create_file(file1, 'Test content\n')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "first change"')

        self.source.run_cmd('git checkout -b branch1')
        append_to_file(file1, "branch change\n")
        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "edit change"')

        self.source.run_cmd('git checkout main')
        self.source.run_cmd('git merge --no-edit branch1')
        self.source.run_cmd('git commit -m "merged change"')
        # The above becomes a fast-forward

        self.run_GitP4Transfer()

        changes = self.target.p4cmd('changes')
        self.assertEqual(2, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(1, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])

        filelogs = self.target.p4cmd('filelog', '//depot/...')
        self.assertEqual(1, len(filelogs))
        self.assertEqual('edit', filelogs[0]['action'][0])
        self.assertEqual('add', filelogs[0]['action'][1])

        result = self.target.p4cmd('print', '//depot/import/file1')
        self.assertEqual(b'Test content\nbranch change\n', result[1])

    # def testSimpleMerge(self):
    #     "Basic simple branch and merge - only taking main changes"
    #     self.setupTransfer()

    #     inside = self.source.repo_root
    #     file1 = os.path.join(inside, "file1")
    #     file2 = os.path.join(inside, "file2")
    #     create_file(file1, 'Test content\n')

    #     self.source.run_cmd('git add .')
    #     self.source.run_cmd('git commit -m "1: first change"')

    #     self.source.run_cmd('git checkout -b branch1')
    #     append_to_file(file1, "branch change\n")
    #     self.source.run_cmd('git add .')
    #     self.source.run_cmd('git commit -m "2: branch edit change"')

    #     self.source.run_cmd('git checkout main')
    #     create_file(file2, 'Test content2\n')
    #     self.source.run_cmd('git add .')
    #     self.source.run_cmd('git commit -m "3: new file on main"')

    #     self.source.run_cmd('git merge --no-edit branch1')
    #     self.source.run_cmd('git commit -m "4: merged change"')
    #     self.source.run_cmd('git log --graph --abbrev-commit --oneline')

    #     # Validate the parsing of the branches
    #     gitinfo = GitP4Transfer.GitInfo(test_logger)
    #     branchRefs = ['main']
    #     commitList, commits = gitinfo.getCommitDiffs(branchRefs)
    #     self.assertEqual(3, len(commitList))
    #     gitinfo.updateBranchInfo(branchRefs, commitList, commits)
    #     self.assertEqual('main', commits[commitList[0]].branch)
    #     self.assertEqual('main', commits[commitList[1]].branch)
    #     # self.assertEqual('_anon0001', commits[commitList[2]].branch)
    #     self.assertEqual('main', commits[commitList[2]].branch)

    #     self.run_GitP4Transfer()

    #     changes = self.target.p4cmd('changes')
    #     self.assertEqual(3, len(changes))

    #     files = self.target.p4cmd('files', '//depot/...')
    #     self.assertEqual(2, len(files))
    #     # self.assertEqual('//depot/anon_branches/_anon0001/file1', files[0]['depotFile'])
    #     self.assertEqual('//depot/import/file1', files[0]['depotFile'])
    #     self.assertEqual('//depot/import/file2', files[1]['depotFile'])

    #     filelogs = self.target.p4cmd('filelog', '//depot/...')
    #     self.assertEqual(2, len(filelogs))
    #     self.assertEqual('edit', filelogs[0]['action'][0])
    #     self.assertEqual('add', filelogs[1]['action'][0])
    #     # self.assertEqual('integrate', filelogs[1]['action'][0])
    #     self.assertEqual('add', filelogs[1]['action'][0])

    #     result = self.target.p4cmd('print', '//depot/import/file1')
    #     self.assertEqual(b'Test content\nbranch change\n', result[1])

    def testRename(self):
        "Basic rename"
        self.setupTransfer()

        inside = self.source.repo_root
        file1 = os.path.join(inside, "file1")
        file2 = os.path.join(inside, "file2")
        create_file(file1, 'Test content\n')

        self.source.run_cmd('git add .')
        self.source.run_cmd('git commit -m "1: first change"')

        self.source.run_cmd('git mv %s %s' % (file1, file2))
        self.source.run_cmd('git commit -m "2: rename"')

        self.run_GitP4Transfer()
        self.assertCounters(2, 2)

        changes = self.target.p4cmd('changes')
        self.assertEqual(2, len(changes))

        files = self.target.p4cmd('files', '//depot/...')
        self.assertEqual(2, len(files))
        self.assertEqual('//depot/import/file1', files[0]['depotFile'])
        self.assertEqual('//depot/import/file2', files[1]['depotFile'])

        filelogs = self.target.p4cmd('filelog', '//depot/...')
        self.assertEqual(3, len(filelogs))
        self.assertEqual('move/delete', filelogs[0]['action'][0])
        self.assertEqual('move/add', filelogs[1]['action'][0])

        result = self.target.p4cmd('print', '//depot/import/file2')
        self.assertEqual(b'Test content\n', result[1])

    # def testMultipleBranches(self):
    #     "Multiple branches with merge"
    #     self.setupTransfer()

    #     inside = self.source.repo_root
    #     file1 = os.path.join(inside, "file1")
    #     contents = ['0'] * 12
    #     create_file(file1, "\n".join(contents) + "\n")

    #     self.source.run_cmd('git add .')
    #     self.source.run_cmd('git commit -m "first change"')
    #     self.source.run_cmd('git branch branch1')
    #     self.source.run_cmd('git branch branch2')
    #     self.source.run_cmd('git branch branch3')

    #     contents[0] = 'main'
    #     create_file(file1, "\n".join(contents) + "\n")
    #     self.source.run_cmd('git commit -am "change on main"')

    #     self.source.run_cmd('git checkout branch1')
    #     contents = ['0'] * 12
    #     contents[3] = 'branch1'
    #     create_file(file1, "\n".join(contents) + "\n")
    #     self.source.run_cmd('git commit -am "branch1"')

    #     self.source.run_cmd('git checkout branch2')
    #     contents = ['0'] * 12
    #     contents[6] = 'branch2'
    #     create_file(file1, "\n".join(contents) + "\n")
    #     self.source.run_cmd('git commit -am "branch2"')

    #     self.source.run_cmd('git checkout main')
    #     self.source.run_cmd('git merge --no-edit branch2')

    #     self.source.run_cmd('git checkout branch3')
    #     contents = ['0'] * 12
    #     contents[11] = 'branch3'
    #     create_file(file1, "\n".join(contents) + "\n")
    #     self.source.run_cmd('git commit -am "branch3"')

    #     self.source.run_cmd('git checkout main')
    #     self.source.run_cmd('git merge --no-edit branch1')
    #     self.source.run_cmd('git merge --no-edit branch3')
    #     self.source.run_cmd('cat file1')
    #     self.source.run_cmd('git log --graph --abbrev-commit --oneline')

    #     # Validate the parsing of the branches
    #     gitinfo = GitP4Transfer.GitInfo(test_logger)
    #     branchRefs = ['main']
    #     commitList, commits = gitinfo.getCommitDiffs(branchRefs)
    #     self.assertEqual(5, len(commitList))
    #     gitinfo.updateBranchInfo(branchRefs, commitList, commits)
    #     self.assertEqual('main', commits[commitList[0]].branch)
    #     self.assertEqual('main', commits[commitList[1]].branch)
    #     # self.assertEqual('_anon0001', commits[commitList[2]].branch)
    #     self.assertEqual('main', commits[commitList[3]].branch)

    #     self.run_GitP4Transfer()

    #     changes = self.target.p4cmd('changes')
    #     self.assertEqual(5, len(changes))

    #     files = self.target.p4cmd('files', '//depot/...')
    #     self.assertEqual(1, len(files))
    #     # self.assertEqual('//depot/anon_branches/_anon0001/file1', files[0]['depotFile'])
    #     # self.assertEqual('//depot/anon_branches/_anon0002/file1', files[1]['depotFile'])
    #     # self.assertEqual('//depot/anon_branches/_anon0003/file1', files[2]['depotFile'])
    #     self.assertEqual('//depot/import/file1', files[0]['depotFile'])

    #     filelogs = self.target.p4cmd('filelog', '//depot/...')
    #     self.assertEqual(1, len(filelogs))
    #     self.assertEqual('edit', filelogs[0]['action'][0])

    #     result = self.target.p4cmd('print', '//depot/import/file1#2')
    #     contents = ['0'] * 12
    #     contents[0] = 'main'
    #     tcontents = '\n'.join(contents) + '\n'
    #     self.assertEqual(tcontents.encode(), result[1])

    #     result = self.target.p4cmd('print', '//depot/import/file1#3')
    #     contents = ['0'] * 12
    #     contents[0] = 'main'
    #     contents[6] = 'branch2'
    #     tcontents = '\n'.join(contents) + '\n'
    #     self.assertEqual(tcontents.encode(), result[1])

    #     result = self.target.p4cmd('print', '//depot/import/file1#4')
    #     contents = ['0'] * 12
    #     contents[0] = 'main'
    #     contents[4] = 'branch1'
    #     contents[6] = 'branch2'
    #     tcontents = '\n'.join(contents) + '\n'
    #     self.assertEqual(tcontents.encode(), result[1])

    #     result = self.target.p4cmd('print', '//depot/import/file1')
    #     contents = ['0'] * 12
    #     contents[0] = 'main'
    #     contents[4] = 'branch1'
    #     contents[6] = 'branch2'
    #     contents[11] = 'branch3'
    #     tcontents = '\n'.join(contents) + '\n'
    #     self.assertEqual(tcontents.encode(), result[1])

    # def testNonSuperUser(self):
    #     "Test when not a superuser - who can't update"
    #     self.setupTransfer()
    #     # Setup default transfer user as super user
    #     # All other users will just have write access
    #     username = "newuser"
    #     p = self.target.p4.fetch_protect()
    #     p['Protections'].append("review user %s * //..." % username)
    #     self.target.p4.save_protect(p)

    #     options = self.getDefaultOptions()
    #     options["superuser"] = "n"
    #     targOptions = {"p4user": username}
    #     self.createConfigFile(options=options, targOptions=targOptions)

    #     inside = localDirectory(self.source.client_root, "inside")
    #     inside_file1 = os.path.join(inside, "inside_file1")
    #     create_file(inside_file1, 'Test content')

    #     self.source.p4cmd('add', inside_file1)
    #     self.source.p4cmd('submit', '-d', 'inside_file1 added')

    #     self.run_GitP4Transfer()

    #     changes = self.target.p4cmd('changes')
    #     self.assertEqual(len(changes), 1, "Target does not have exactly one change")
    #     self.assertEqual(changes[0]['change'], "1")

    #     files = self.target.p4cmd('files', '//depot/...')
    #     self.assertEqual(len(files), 1)
    #     self.assertEqual(files[0]['depotFile'], '//depot/import/inside_file1')

    #     self.assertCounters(1, 1)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--p4d', default=P4D)
    parser.add_argument('unittest_args', nargs='*')

    args = parser.parse_args()
    if args.p4d != P4D:
        P4D = args.p4d

    # Now set the sys.argv to the unittest_args (leaving sys.argv[0] alone)
    unit_argv = [sys.argv[0]] + args.unittest_args
    unittest.main(argv=unit_argv)
