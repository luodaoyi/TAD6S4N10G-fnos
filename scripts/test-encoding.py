#!/usr/bin/env python3
"""Offline tests for the encoding gate; damaged examples are built at runtime."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('encoding_check', Path(__file__).with_name('check-encoding.py'))
encoding = importlib.util.module_from_spec(spec)
spec.loader.exec_module(encoding)


class EncodingCheck(unittest.TestCase):
    def test_index_paths_do_not_follow_parent_directory_symlinks(self):
        with tempfile.TemporaryDirectory() as tmp:
            root, outside = Path(tmp) / 'repo', Path(tmp) / 'outside'
            root.mkdir()
            encoding.git(root, 'init', '-q')
            (root / 'directory').mkdir()
            (root / 'directory/file.txt').write_text('正常中文', encoding='utf-8')
            encoding.git(root, 'add', '.')
            encoding.git(root, '-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: nested file')
            alternate = root / '.git/alternate-index'
            alternate.write_bytes((root / '.git/index').read_bytes())
            (root / 'directory').rename(outside)
            (outside / 'file.txt').write_text(chr(0xfffd), encoding='utf-8')
            (root / 'directory').symlink_to(outside, target_is_directory=True)
            original_read = Path.read_bytes
            def read_without_traversal(path):
                self.assertNotEqual(path, root / 'directory/file.txt', 'must reject before reading through a directory symlink')
                return original_read(path)
            with patch.object(Path, 'read_bytes', read_without_traversal):
                for index in [None, Path('.git/alternate-index')]:
                    errors = encoding.check(root, index_file=index)[0]
                    self.assertTrue(any('directory/file.txt:' in issue and 'directory symlink' in issue for issue in errors))

    def test_explicit_alternate_index_includes_its_tracked_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            encoding.git(root, 'init', '-q')
            (root / 'normal.txt').write_text('正常中文', encoding='utf-8')
            encoding.git(root, 'add', '.')
            encoding.git(root, '-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: default index')
            default_index = (root / '.git/index').read_bytes()
            alternate = root / '.git/alternate-index'
            alternate.write_bytes(default_index)
            (root / 'alternate-only.txt').write_text(chr(0xfffd), encoding='utf-8')
            env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
            env['GIT_INDEX_FILE'] = str(alternate)
            subprocess.check_output(['git', '-C', tmp, 'add', 'alternate-only.txt'], env=env)
            self.assertEqual(encoding.check(root)[0], [])
            errors = encoding.check(root, index_file=Path('.git/alternate-index'))[0]
            self.assertTrue(any('alternate-only.txt:' in issue and 'replacement' in issue for issue in errors))
            self.assertEqual((root / '.git/index').read_bytes(), default_index)
            with self.assertRaises(OSError):
                encoding.check(root, index_file=Path('.git/missing-index'))
            env.pop('GITHUB_EVENT_PATH', None)
            command = [sys.executable, encoding.__file__, '--root', tmp, '--index-file']
            result = subprocess.run(command + ['.git/alternate-index'], env=env, capture_output=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn(b'alternate-only.txt:', result.stderr)
            result = subprocess.run(command + ['.git/missing-index'], env=env, capture_output=True)
            self.assertEqual(result.returncode, 2)

    def test_git_commands_ignore_inherited_repository_environment(self):
        with tempfile.TemporaryDirectory() as tmp:
            caller, target = Path(tmp) / 'caller', Path(tmp) / 'target'
            caller.mkdir()
            target.mkdir()
            encoding.git(caller, 'init', '-q')
            (caller / 'caller.txt').write_text('caller', encoding='utf-8')
            encoding.git(caller, 'add', '.')
            encoding.git(caller, '-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: caller')
            before_head = encoding.git(caller, 'rev-parse', 'HEAD')
            before_index = (caller / '.git/index').read_bytes()
            inherited = {'GIT_DIR': str(caller / '.git'),
                         'GIT_WORK_TREE': str(caller),
                         'GIT_INDEX_FILE': str(caller / '.git/index'),
                         'GIT_OBJECT_DIRECTORY': str(caller / '.git/objects'),
                         'GIT_COMMON_DIR': str(caller / '.git')}
            with patch.dict(os.environ, inherited):
                encoding.git(target, 'init', '-q')
                self.assertTrue((target / '.git').is_dir())
                (target / 'target.txt').write_text('正常中文', encoding='utf-8')
                encoding.git(target, 'add', '.')
                encoding.git(target, '-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: target')
                self.assertEqual(encoding.git(target, 'ls-files'), b'target.txt\n')
                self.assertEqual(encoding.check(target)[0], [])
            self.assertEqual(encoding.git(caller, 'rev-parse', 'HEAD'), before_head)
            self.assertEqual((caller / '.git/index').read_bytes(), before_index)

    def test_normal_unicode_and_legitimate_placeholders(self):
        text = '中文文件名、温度 45°C、€100、São Paulo、😀、' + chr(0xe001) + '\nconst value = x ?? 0; const status = "????";'
        self.assertEqual(encoding.text_issues(text), [])

    def test_reproduces_pr33_subject_and_recovers_original(self):
        original = '图形化备份、三曲线风扇模式、移动设备面板与传感器默认名'
        # Exact e26bbed subject fragment, escaped so the source file stays clean.
        damaged = '\u9365\u60e7\u8230\u9356\u6827\ue62c\u6d60\u59d0\u20ac\u4f77\u7b01\u93c7\u832c\u568e\u690b\u5ea2\u5896\u59af\u2033\u7d21\u9286\u4f7a\u0429\u9354\u3128\ue195\u6fb6\u56ec\u6f70\u93c9\u5938\u7b0c\u6d7c\u72b3\u5285\u9363\u3129\u7caf\u7481\u3085\u6095'
        self.assertEqual(encoding.undo_cp936(damaged), original)
        self.assertTrue(encoding.text_issues('feat: ' + damaged + ' (#33)'))

    def test_mojibake_surrounded_by_normal_chinese_and_punctuation(self):
        original = '图形化备份、三曲线风扇模式、移动设备面板与传感器默认名'
        damaged = original.encode('utf-8').decode('gb18030', errors='replace').replace(chr(0xfffd), chr(0x20ac))
        for prefix, suffix in [('', ''), ('标题：', ''), ('', '。'),
                               ('标题：', '。'), ('正常说明（', '）结束'),
                               ('中文', '中文')]:
            with self.subTest(prefix=prefix, suffix=suffix):
                mixed = prefix + damaged + suffix
                self.assertTrue(encoding.text_issues(mixed))
                fragment = encoding.find_cp936_fragment(mixed)
                self.assertIsNotNone(fragment)
                self.assertEqual(fragment[2], original)
        short = '文件'.encode('utf-8').decode('gb18030')
        self.assertTrue(encoding.text_issues('说明：' + short + '。'))

    def test_normal_chinese_surroundings_are_not_damage(self):
        for text in ['标题：图形化备份、三曲线风扇模式。', '正常说明（文件）结束',
                     '中文文件中文', '文件名、目录、提交、标签、版本、默认值。',
                     '编码检查不会修改正常中文或合法标点：。；，（）【】。']:
            self.assertEqual(encoding.text_issues(text), [])

    def test_replacement_and_latin_mojibake(self):
        for value in [chr(0xfffd), 'caf' + chr(0xc3) + chr(0xa9)]:
            self.assertTrue(encoding.text_issues(value))

    def test_intentional_fixture_needs_explicit_reason(self):
        value = chr(0xfffd)
        self.assertTrue(encoding.text_issues(value + ' # encoding-check: allow', True))
        self.assertEqual(encoding.text_issues(value + ' # encoding-check: allow -- parser fixture', True), [])
        self.assertTrue(encoding.text_issues(value + ' # encoding-check: allow -- parser fixture'))

    def test_event_ranges(self):
        first, second = 'a' * 40, 'b' * 40
        self.assertEqual(encoding.event_range({'pull_request': {'base': {'sha': first}, 'head': {'sha': second}}}), first + '..' + second)
        self.assertEqual(encoding.event_range({'ref': 'refs/heads/main', 'before': first, 'after': second}), first + '..' + second)
        self.assertIsNone(encoding.event_range({'ref': 'refs/tags/v1.0.2', 'before': first, 'after': second}))
        self.assertIsNone(encoding.event_range({'before': '0' * 40, 'after': second}))
        with self.assertRaises(ValueError):
            encoding.validate_range('--all')

    def test_wasm_magic_does_not_exempt_text_by_extension(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def git(*args):
                return encoding.git(root, *args)
            git('init', '-q')
            # Complete v1 preamble is itself a valid empty WebAssembly module.
            header = bytes([0, 97, 115, 109, 1, 0, 0, 0])
            (root / 'module.wasm').write_bytes(header)
            (root / 'module-no-extension').write_bytes(header)
            (root / 'text.wasm').write_text('正常文本', encoding='utf-8')
            git('add', '.')
            git('-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: wasm fixture')
            errors, checked, _ = encoding.check(root)
            self.assertEqual(errors, [])
            self.assertEqual(checked, 1, 'text with a wasm suffix must still be inspected')
            (root / 'text.wasm').write_text(chr(0xfffd), encoding='utf-8')
            self.assertTrue(any('text.wasm' in issue and 'replacement' in issue for issue in encoding.check(root)[0]))
            (root / 'text.wasm').write_bytes(bytes([0xff]))
            self.assertTrue(any('text.wasm' in issue and 'invalid UTF-8' in issue for issue in encoding.check(root)[0]))
            (root / 'text.wasm').write_text('正常文本', encoding='utf-8')
            # A truncated magic / unknown version is not a recognized binary.
            for data in [header[:4], header[:7], bytes([0, 97, 115, 109, 2, 0, 0, 0])]:
                (root / 'module.wasm').write_bytes(data)
                self.assertTrue(any('module.wasm' in issue for issue in encoding.check(root)[0]))

    def test_symlink_targets_are_text_regardless_of_name_or_destination(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def git(*args):
                return encoding.git(root, *args)
            git('init', '-q')
            os.symlink(b'bad-' + bytes([0xff]), os.fsencode(root / 'link.png'))
            damaged_directory = 'directory-' + chr(0xfffd)
            (root / damaged_directory).mkdir()
            (root / 'directory-link').symlink_to(damaged_directory)
            (root / 'normal-directory').mkdir()
            (root / 'normal-directory-link').symlink_to('normal-directory')
            (root / 'normal.png').symlink_to('outside-this-repository')
            git('add', '.')
            git('-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: symlink targets')
            errors, checked, _ = encoding.check(root)
            self.assertEqual(len(errors), 2)
            self.assertTrue(any('link.png: invalid UTF-8' in issue for issue in errors))
            self.assertTrue(any('directory-link:' in issue and 'replacement' in issue for issue in errors))
            self.assertEqual(checked, 3, 'valid UTF-8 link targets are checked without following them')

    def test_tracked_bytes_names_binary_and_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def git(*args):
                return encoding.git(root, *args)
            git('init', '-q')
            good = root / '正常.md'
            good.write_text('正常中文\n', encoding='utf-8')
            (root / 'image.png').write_bytes(bytes([0xff, 0, 0xfe]))
            (root / 'native-tool').write_bytes(b'\x7fELF' + bytes([0xff, 0, 0xfe]))
            (root / 'link').symlink_to('outside-this-repository')
            git('add', '.')
            git('-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'test: normal Unicode')
            first = git('rev-parse', 'HEAD').decode().strip()
            self.assertEqual(encoding.check(root)[0], [])
            good.write_bytes(bytes([0xff]))
            self.assertTrue(any('invalid UTF-8' in issue for issue in encoding.check(root)[0]))
            good.write_text('正常中文\n', encoding='utf-8')
            bad = root / ('bad-' + chr(0xfffd) + '.md')
            bad.write_text('正常', encoding='utf-8')
            git('add', '.')
            self.assertTrue(any('filename' in issue for issue in encoding.check(root)[0]))
            bad.unlink()
            git('add', '-u')
            good.write_text('新增正常中文\n', encoding='utf-8')
            git('add', '.')
            message = root / 'message.txt'
            message.write_text('test: ' + chr(0xfffd), encoding='utf-8')
            git('-c', 'user.name=Encoding Test', '-c', 'user.email=test@example.invalid', 'commit', '-qF', str(message))
            second = git('rev-parse', 'HEAD').decode().strip()
            self.assertTrue(any('message line' in issue for issue in encoding.check(root, first + '..' + second)[0]))


if __name__ == '__main__':
    unittest.main()
