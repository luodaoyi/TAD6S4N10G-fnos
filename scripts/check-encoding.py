#!/usr/bin/env python3
"""Check tracked text/names and new commit messages without rewriting bytes."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

BINARY_SUFFIXES = {'.png', '.jpg', '.jpeg', '.gif', '.webp', '.ico', '.pdf',
                   '.zip', '.gz', '.tgz', '.fpk', '.spk', '.exe', '.bin',
                   '.woff', '.woff2', '.ttf', '.otf'}
SHA = re.compile(r'[0-9a-f]{40}')
NON_ASCII = re.compile(r'[^\x00-\x7f]+')
LATIN_MOJIBAKE = re.compile(r'(?:[\u00c2\u00c3][\u0080-\u00bf]|\u00ef\u00bf\u00bd|\u00e2[\u0080-\u00bf]{2}|\u00e2\u20ac[\u2013\u2014\u02dc\u2122\u0153\u009d\u00a6])')
ALLOW = re.compile(r'encoding-check: allow -- \S')


def undo_cp936(value):
    """Windows CP936's historical euro/PUA mappings can hide valid UTF-8."""
    try:
        data = b''.join(b'\x80' if char == '\u20ac' else char.encode('gb18030')
                        for char in value)
        recovered = data.decode('utf-8')
    except UnicodeError:
        return None
    if (recovered != value and len(recovered) < len(value)
            and len(re.findall(r'[\u4e00-\u9fff]', recovered)) >= 2):
        return recovered
    return None


def text_issues(text, allow_fixtures=False):
    issues = []
    for number, line in enumerate(text.splitlines(), 1):
        if allow_fixtures and ALLOW.search(line):
            continue
        if '\x00' in line:
            issues.append((number, 'NUL in text'))
        if '\ufffd' in line:
            issues.append((number, 'Unicode replacement character'))
        if LATIN_MOJIBAKE.search(line):
            issues.append((number, 'possible UTF-8/Latin mojibake'))
        if any(undo_cp936(match.group()) for match in NON_ASCII.finditer(line)):
            issues.append((number, 'reversible UTF-8/CP936 mojibake'))
    return issues


def git(root, *args):
    return subprocess.check_output(['git', '-C', str(root), *args])


def validate_range(value):
    parts = value.split('..')
    if len(parts) != 2 or not all(SHA.fullmatch(part) for part in parts):
        raise ValueError('commit range must contain two full 40-character SHAs')
    return value


def event_range(event):
    if 'pull_request' in event:
        pr = event['pull_request']
        return validate_range(pr['base']['sha'] + '..' + pr['head']['sha'])
    # Tags/initial branch creation have no comparable previous commit.
    before, after = event.get('before'), event.get('after')
    if (not event.get('ref', '').startswith('refs/tags/') and before and after
            and before != '0' * 40 and after != '0' * 40):
        return validate_range(before + '..' + after)
    return None


def check(root, commit_range=None):
    errors = []
    checked = 0
    for raw in git(root, 'ls-files', '-z').split(b'\0'):
        if not raw:
            continue
        try:
            name = raw.decode('utf-8')
        except UnicodeDecodeError:
            errors.append('tracked filename is not UTF-8: ' + raw.hex())
            continue
        for _, reason in text_issues(name):
            errors.append(name + ': filename: ' + reason)
        path = root / name
        if path.suffix.lower() in BINARY_SUFFIXES or path.is_dir():
            continue
        try:
            # Read a symlink's tracked target, never follow it into unrelated files.
            data = os.fsencode(os.readlink(path)) if path.is_symlink() else path.read_bytes()
            if not path.is_symlink() and data.startswith((b'\x7fELF', b'\x89PNG\r\n\x1a\n', b'\xff\xd8\xff')):
                continue
            text = data.decode('utf-8-sig')
        except UnicodeDecodeError as exc:
            errors.append(f'{name}: invalid UTF-8 at byte {exc.start}')
            continue
        except OSError as exc:
            errors.append(f'{name}: cannot read tracked text ({exc.__class__.__name__})')
            continue
        checked += 1
        errors.extend(f'{name}:{line}: {reason}'
                      for line, reason in text_issues(text, allow_fixtures=True))
    commits = (git(root, 'rev-list', validate_range(commit_range)).decode().splitlines()
               if commit_range else [git(root, 'rev-parse', 'HEAD').decode().strip()])
    for sha in commits:
        raw = git(root, 'cat-file', 'commit', sha).partition(b'\n\n')[2]
        try:
            message = raw.decode('utf-8')
        except UnicodeDecodeError:
            errors.append(sha + ': commit message is not UTF-8')
            continue
        errors.extend(f'{sha}: message line {line}: {reason}'
                      for line, reason in text_issues(message))
    return errors, checked, len(commits)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path.cwd())
    parser.add_argument('--commit-range', help='full-base-SHA..full-head-SHA')
    args = parser.parse_args()
    try:
        selected = args.commit_range
        if selected is None and os.environ.get('GITHUB_EVENT_PATH'):
            event = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text(encoding='utf-8'))
            selected = event_range(event)
        errors, files, commits = check(args.root, selected)
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as exc:
        print(f'Encoding check could not complete ({exc.__class__.__name__}).', file=sys.stderr)
        return 2
    for error in errors:
        # Print locations and reasons, not potentially sensitive text contents.
        print(error, file=sys.stderr)
    if errors:
        return 1
    print(f'Encoding check passed: {files} tracked text files, {commits} commit messages.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
