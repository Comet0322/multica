#!/usr/bin/env python3
"""Claude Code PreToolUse hook: keep the agent away from frozen paths.

Frozen paths, in a repo that has company/ci/frozen-files.txt:
  - everything under company/ci/ (pipeline, fingerprints, flaky list)
  - company/customizations.yaml and .gitlab-ci.yml
  - every path listed in company/ci/frozen-files.txt (acceptance tests, test configs)

Blocks, with exit code 2 and the reason on stderr:
  - Edit / Write / MultiEdit / NotebookEdit on a frozen path
  - Bash commands that write a frozen path they name (sed -i, cp/mv/rm, redirects,
    git checkout/restore/rm/mv, interpreters given a frozen path, ...)
  - git commit while a frozen path differs from HEAD, and git push of commits
    that touch one: the net for writes the first two cannot see

Anywhere else the hook allows everything. A person who has approved a change
starts the session with MULTICA_FROZEN_UNLOCK=1.
"""
import fnmatch
import json
import os
import re
import shlex
import subprocess
import sys

STATIC_DIRS = ('company/ci/',)
STATIC_FILES = ('company/customizations.yaml', '.gitlab-ci.yml',
                # the project settings that register this hook, and the local
                # file where `disableAllHooks` would switch it off
                '.claude/settings.json', '.claude/settings.local.json')
SETTINGS_NAMES = ('settings.json', 'settings.local.json')
REASON = ('{what}: {path} is frozen (CI configuration, an acceptance test, or the '
          'Claude Code settings that hold this guard). '
          'It changes only with human approval. Leave it as it is and put the '
          'change you need, as a diff with its reason, in the port report or '
          'the MR description.')

# Commands that write the paths they are given.
WRITE_ANY_ARG = {'rm', 'mv', 'tee', 'truncate', 'chmod', 'chown', 'touch', 'unlink',
                 'shred', 'ed', 'ex', 'vi', 'vim', 'nano'}
INTERPRETERS = {'python', 'python3', 'node', 'perl', 'ruby'}  # a frozen path anywhere in their args
SHELLS = {'bash', 'sh', 'zsh'}                              # `-c` code is checked as a command
WRITE_LAST_ARG = {'cp', 'install', 'ln', 'rsync'}
GIT_WRITE = {'checkout', 'restore', 'rm', 'mv', 'reset', 'stash', 'clean'}


def git(root, *args):
    r = subprocess.run(['git', '-C', root, *args], capture_output=True, text=True)
    return r.stdout if r.returncode == 0 else ''


def block(what, path):
    print(REASON.format(what=what, path=path), file=sys.stderr)
    sys.exit(2)


def main():
    if os.environ.get('MULTICA_FROZEN_UNLOCK') == '1':
        return
    data = json.load(sys.stdin)
    cwd = data.get('cwd') or os.getcwd()
    root = git(cwd, 'rev-parse', '--show-toplevel').strip()
    listing = os.path.join(root, 'company/ci/frozen-files.txt') if root else ''
    if not root or not os.path.isfile(listing):
        return
    root = os.path.realpath(root)
    listed = [l.strip() for l in open(listing) if l.strip() and not l.startswith('#')]

    def rel(path):
        p = os.path.realpath(os.path.join(cwd, os.path.expanduser(path)))
        r = os.path.relpath(p, root)
        return None if r.startswith('..') else r

    def settings(path):
        # Claude Code settings anywhere (user, project, managed): writing one
        # could remove or disable this hook.
        p = os.path.realpath(os.path.join(cwd, os.path.expanduser(path)))
        if (os.path.basename(p) in SETTINGS_NAMES and os.path.basename(os.path.dirname(p)) == '.claude') \
                or p.startswith('/etc/claude-code/'):
            return p
        return None

    def guarded(path):
        r = rel(path)
        return r if frozen(r) else settings(path)

    def frozen(r):
        if r is None:
            return False
        if r in STATIC_FILES or r in listed or any(r.startswith(d) or r + '/' == d for d in STATIC_DIRS):
            return True
        if any(ch in r for ch in '*?['):  # a glob naming frozen files
            return any(fnmatch.fnmatch(x, r) for x in listed) or any(d.startswith(r.rstrip('*')) for d in STATIC_DIRS)
        return False

    tool, ti = data.get('tool_name'), data.get('tool_input') or {}

    if tool in ('Edit', 'Write', 'MultiEdit', 'NotebookEdit'):
        path = ti.get('file_path') or ti.get('notebook_path') or ''
        g = guarded(path)
        if g:
            block(tool, g)
        return

    if tool != 'Bash':
        return
    cmd = ti.get('command') or ''

    # 1. Each simple command that writes a frozen path it names.
    def check(cmd, depth=0):
        for seg in re.split(r'&&|\|\||[;|\n]', cmd):
            for m in re.finditer(r'\d?>{1,2}\s*([^\s;&|<>]+)', seg):  # redirect targets
                g = guarded(m.group(1).strip('\'"'))
                if g:
                    block('Redirect', g)
            try:
                words = shlex.split(seg)
            except ValueError:
                words = seg.split()
            while words and (('=' in words[0] and not words[0].startswith('-'))
                             or words[0] in ('sudo', 'env', 'command', 'exec', 'xargs', 'time', 'nohup')):
                words = words[1:]
            if not words:
                continue
            prog = os.path.basename(words[0])
            args = [w for w in words[1:] if not w.startswith('-')]
            if prog in SHELLS:
                if '-c' in words and depth < 3:
                    i = words.index('-c')
                    if i + 1 < len(words):
                        check(words[i + 1], depth + 1)
                continue
            if prog in INTERPRETERS:
                for w in words[1:]:
                    for x in listed + list(STATIC_FILES) + list(STATIC_DIRS) + ['.claude/settings', '/etc/claude-code']:
                        if x in w:
                            block(f'Bash ({prog})', x)
                continue
            if prog in ('sed', 'gawk', 'awk') and any(
                    w.startswith('-i') or w in ('--in-place', 'inplace') for w in words[1:]):
                targets = args
            elif prog in WRITE_ANY_ARG:
                targets = args
            elif prog in WRITE_LAST_ARG:
                targets = args[-1:]
            elif prog == 'git':
                sub = next((w for w in words[1:] if not w.startswith('-')), '')
                targets = args[1:] if sub in GIT_WRITE else []
            else:
                targets = []
            for t in targets:
                g = guarded(t)
                if g:
                    block(f'Bash ({prog})', g)

    check(cmd)

    # 2. The net: what a commit or push would carry.
    if re.search(r'\bgit\b[^;&|\n]*\bcommit\b', cmd):
        changed = [l[3:].split(' -> ')[-1] for l in git(root, 'status', '--porcelain').splitlines()]
        bad = [c for c in changed if frozen(c)]
        if bad:
            block('git commit', ', '.join(bad))
    if re.search(r'\bgit\b[^;&|\n]*\bpush\b', cmd):
        changed = set(git(root, 'log', '--name-only', '--format=', 'HEAD', '--not', '--remotes').split())
        bad = sorted(c for c in changed if frozen(c))
        if bad:
            block('git push', ', '.join(bad))


if __name__ == '__main__':
    main()
