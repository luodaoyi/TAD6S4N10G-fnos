#!/usr/bin/env python3
"""Offline tests for the encoding gate; damaged examples are built at runtime."""
import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('encoding_check', Path(__file__).with_name('check-encoding.py'))
encoding = importlib.util.module_from_spec(spec)
spec.loader.exec_module(encoding)


class EncodingCheck(unittest.TestCase):
    def test_normal_unicode_and_legitimate_placeholders(self):
        text = '中文文件名、温度 45°C、€100、São Paulo、😀、' + chr(0xe001) + '\nconst value = x ?? 0; const status = "????";'
        self.assertEqual(encoding.text_issues(text), [])

    def test_reproduces_pr33_subject_and_recovers_original(self):
        original = '图形化备份、三曲线风扇模式、移动设备面板与传感器默认名'
        # Exact e26bbed subject fragment, escaped so the source file stays clean.
        damaged = '\u9365\u60e7\u8230\u9356\u6827\ue62c\u6d60\u59d0\u20ac\u4f77\u7b01\u93c7\u832c\u568e\u690b\u5ea2\u5896\u59af\u2033\u7d21\u9286\u4f7a\u0429\u9354\u3128\ue195\u6fb6\u56ec\u6f70\u93c9\u5938\u7b0c\u6d7c\u72b3\u5285\u9363\u3129\u7caf\u7481\u3085\u6095'
        self.assertEqual(encoding.undo_cp936(damaged), original)
        self.assertTrue(encoding.text_issues('feat: ' + damaged + ' (#33)'))

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

    def test_tracked_bytes_names_binary_and_symlink(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            def git(*args):
                return subprocess.check_output(['git', '-C', tmp, *args], stderr=subprocess.DEVNULL)
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
