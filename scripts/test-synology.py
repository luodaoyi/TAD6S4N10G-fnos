#!/usr/bin/env python3
"""Isolated packaging/startup regressions; never start a daemon or touch DSM."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class SynologyScripts(unittest.TestCase):
    def test_credentials_private_at_creation_and_upstream_refresh(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            source = (ROOT / 'synology/scripts/real-start-stop-status').read_text()
            # Load definitions only: no lifecycle action, module loading or hardware.
            definitions = source.split('\ncase "${1:-}" in\n')[0]
            (work / 'functions').write_text(definitions)
            (work / 'etc').mkdir()
            body = r'''
. "$TEST_DIR/functions"
PKG_ETC="$TEST_DIR/etc"
AUTH="$PKG_ETC/web-auth.json"
PROXY_TOKEN="$PKG_ETC/proxy-token"
NGINX_CONF_DIR="$TEST_DIR/nginx"
NGINX_CONF="$NGINX_CONF_DIR/dsm.tad-module.conf"
# Observe modes BEFORE the production chmod; a post-write chmod cannot hide a leak.
chmod() {
  shift
  for secret in "$@"; do
    test "$(stat -c %a "$secret")" = 600 || exit 71
  done
}
nginx() { test "$1" = -t; }
systemctl() { echo reload >> "$TEST_DIR/reloads"; }
umask 022
ensure_web_auth
ensure_proxy_token
ensure_nginx
test "$(umask)" = 0022
# Unchanged config must avoid a redundant reload.
ensure_nginx
test "$(wc -l < "$TEST_DIR/reloads")" -eq 1
# Both ordinary and IPv6 upstreams must replace an otherwise matching v5/token config.
HTTP_ADDR=127.0.0.1:9000
ensure_nginx
grep -Fqx '    proxy_pass http://127.0.0.1:9000;' "$NGINX_CONF"
HTTP_ADDR='[::1]:9001'
ensure_nginx
grep -Fqx '    proxy_pass http://[::1]:9001;' "$NGINX_CONF"
test "$(wc -l < "$TEST_DIR/reloads")" -eq 3
test "$(umask)" = 0022
'''
            env = dict(os.environ, TEST_DIR=tmp, SYNOPKG_PKGDEST=tmp,
                       SYNOPKG_PKGETC=str(work / 'etc'), SYNOPKG_PKGVAR=tmp)
            subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)

    def test_nginx_failures_keep_token_and_retry_reload(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            source = (ROOT / 'synology/scripts/real-start-stop-status').read_text()
            (work / 'functions').write_text(source.split('\ncase "${1:-}" in\n')[0])
            (work / 'etc').mkdir()
            env = dict(os.environ, TEST_DIR=tmp, SYNOPKG_PKGDEST=tmp,
                       SYNOPKG_PKGETC=str(work / 'etc'), SYNOPKG_PKGVAR=tmp)
            body = r'''
. "$TEST_DIR/functions"
NGINX_CONF_DIR="$TEST_DIR/nginx"
NGINX_CONF="$NGINX_CONF_DIR/dsm.tad-module.conf"
mkdir() { return 1; }
if ensure_nginx; then exit 70; fi
test -s "$PROXY_TOKEN"
unset -f mkdir
nginx() { return 0; }
systemctl() { echo reload >> "$TEST_DIR/reloads"; return 1; }
if ensure_nginx; then exit 71; fi
test ! -f "$NGINX_CONF"
systemctl() { echo reload >> "$TEST_DIR/reloads"; return 0; }
ensure_nginx
test -f "$NGINX_CONF"
test "$(wc -l < "$TEST_DIR/reloads")" -eq 2
'''
            subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)

    def test_pid_identity_rejects_stale_or_invalid_targets(self):
        for scenario in ['zero', 'negative', 'text', 'legacy', 'reused', 'reboot',
                         'other-exe', 'other-command', 'exit-reuse', 'valid']:
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                (work / 'var').mkdir()
                (work / 'bin').mkdir()
                binary = work / 'bin/tad-module'
                binary.write_text('isolated binary fixture')
                proc = work / 'proc/123'
                proc.mkdir(parents=True)
                # stat: comm may contain spaces/parentheses; starttime is field 22.
                (proc / 'stat').write_text('123 (tad (module)) S ' + '0 ' * 18 + '456 0\n')
                (proc / 'cmdline').write_bytes(str(binary).encode() + b'\0' +
                                              (b'init' if scenario == 'other-command' else b'serve') + b'\0')
                (proc / 'exe').symlink_to('/bin/sh' if scenario == 'other-exe' else binary)
                boot = work / 'proc/sys/kernel/random'
                boot.mkdir(parents=True)
                (boot / 'boot_id').write_text('boot-A\n')
                pid_line = {'zero': '0 456 boot-A', 'negative': '-1 456 boot-A',
                            'text': 'oops 456 boot-A', 'legacy': '123',
                            'reused': '123 999 boot-A', 'reboot': '123 456 boot-B'}.get(scenario, '123 456 boot-A')
                (work / 'var/tad-module.pid').write_text(pid_line + '\n')
                source = (ROOT / 'synology/scripts/real-start-stop-status').read_text()
                # Test-only replacement: all process files point into this temporary tree.
                source = source.replace('/proc/', str(work / 'proc') + '/')
                (work / 'service').write_text(source)
                env = dict(os.environ, TEST_DIR=tmp, SCENARIO=scenario, SYNOPKG_PKGDEST=tmp,
                           SYNOPKG_PKGETC=tmp, SYNOPKG_PKGVAR=str(work / 'var'))
                body = r'''
kill() {
  echo "$*" >> "$TEST_DIR/signals"
  if [ "$SCENARIO" = exit-reuse ] && [ "$*" = 123 ]; then
    sed -i 's/456/999/' "$TEST_DIR/proc/123/stat"
  fi
  return 0
}
sleep() { :; }
set -- stop
. "$TEST_DIR/service"
'''
                subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)
                signals = (work / 'signals').read_text().splitlines() if (work / 'signals').exists() else []
                if scenario == 'valid':
                    self.assertIn('123', signals)
                    self.assertIn('-9 123', signals)
                elif scenario == 'exit-reuse':
                    self.assertIn('123', signals)
                    self.assertNotIn('-9 123', signals, 'reused PID must not receive KILL')
                else:
                    self.assertFalse(signals, f'invalid PID identity received signal: {signals}')

    def test_docker_target_and_reject_wrong_architecture(self):
        for machine, ident in [(b'\xb7\0', b'\x7fELF\x02\x01'),
                               (b'\x3e\0', b'\x7fELF\x01\x01')]:
            with self.subTest(machine=machine, ident=ident), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                (work / 'scripts').mkdir()
                (work / 'manifest').write_text('version="1.0.1"\n')
                script = (ROOT / 'scripts/build-spk.sh').read_text()
                # Force only the Docker branch even on a developer machine with Go.
                script = script.replace('if command -v go >/dev/null 2>&1; then', 'if false; then')
                (work / 'scripts/build-spk.sh').write_text(script)
                tools = work / 'tools'
                tools.mkdir()
                # Fake Docker writes an invalid target so validation must stop before packaging.
                stub = tools / 'docker'
                stub.write_text('#!/bin/sh\n' + r'''
for required in GOOS=linux GOARCH=amd64 CGO_ENABLED=0; do
  found=no
  for arg in "$@"; do [ "$arg" != "$required" ] || found=yes; done
  [ "$found" = yes ] || exit 72
done
cp "$FAKE_ELF" app/bin/tad-module
''')
                stub.chmod(0o755)
                data = bytearray(64)
                data[:6] = ident
                data[18:20] = machine
                elf = work / 'wrong-arch'
                elf.write_bytes(data)
                result = subprocess.run(['bash', 'scripts/build-spk.sh'], cwd=work,
                                        env=dict(os.environ, PATH=str(tools) + ':' + os.environ['PATH'], FAKE_ELF=str(elf)),
                                        capture_output=True, text=True, timeout=15)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('x86_64', result.stderr)
                self.assertFalse(list(work.glob('*.spk')))

    # ---- luodaoyi review #40: 地址感知端口预检/STARTUP_LOG 回退/rc 捕获/
    # ---- 向导端口规范化/暂存消费/桌面窗口 config 透传的隔离回归。

    REAL = ROOT / 'synology/scripts/real-start-stop-status'

    @staticmethod
    def _definitions(source_text):
        # Load definitions only: no lifecycle action, module loading or hardware.
        return source_text.split('\ncase "${1:-}" in\n')[0]

    @staticmethod
    def _shpath(p):
        # Windows 本地跑 Git Bash sh 时,`*/@appstore` 这类尾缀匹配只认 "/"。
        # Linux CI 上是无操作。
        return str(p).replace('\\', '/')

    def test_startup_log_resolves_after_appdata_fallback(self):
        # STARTUP_LOG 必须在 PKG_VAR 的 @appdata 回退之后赋值,否则手动调用
        # (无 SYNOPKG_PKGVAR)时日志写不到最终运行目录。
        for scenario in ['env-var', 'appdata-fallback', 'no-appdata']:
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                appstore = work / '@appstore/tad-module'
                appstore.mkdir(parents=True)
                (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
                env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                           SYNOPKG_PKGDEST=self._shpath(appstore))
                if scenario == 'env-var':
                    env['SYNOPKG_PKGVAR'] = self._shpath(work / 'explicit-var')
                elif scenario == 'appdata-fallback':
                    (work / '@appdata/tad-module').mkdir(parents=True)
                expected = {'env-var': work / 'explicit-var/startup-debug.log',
                            'appdata-fallback': work / '@appdata/tad-module/startup-debug.log',
                            'no-appdata': appstore / 'var/startup-debug.log'}[scenario]
                body = r'''
. "$TEST_DIR/functions"
printf '%s' "$STARTUP_LOG"
'''
                result = subprocess.run(['sh', '-ec', body], env=env, capture_output=True,
                                        text=True, timeout=15)
                self.assertEqual(
                    result.returncode, 0,
                    f'sourcing definitions failed: {result.stderr[-800:]}')
                # Windows 本地 sh 输出正斜杠路径,Linux CI 一致;按 / 归一比较。
                self.assertEqual(result.stdout, str(expected).replace('\\', '/'))

    def test_port_precheck_is_address_aware(self):
        # 仅同址或任一侧通配监听才算冲突;其他进程只绑 LAN IP 的同端口时,
        # 回环监听可共存(luodaoyi review #40 第 3 条)。netstat 用 shell
        # 函数覆盖(优先于 PATH),输出行经由环境变量注入。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            env = dict(os.environ, TEST_DIR=tmp)
            source = self._definitions(self.REAL.read_text())
            (work / 'functions').write_text(source)
            cases = [  # (netstat 输出, 探测目标, 是否应判定冲突)
                ('tcp   0  0 192.168.1.10:8123  0.0.0.0:*  LISTEN', '127.0.0.1:8123', False),
                ('tcp   0  0 0.0.0.0:8123       0.0.0.0:*  LISTEN', '127.0.0.1:8123', True),
                ('tcp   0  0 127.0.0.1:8123     0.0.0.0:*  LISTEN', '127.0.0.1:8123', True),
                ('tcp   0  0 192.168.1.10:8123  0.0.0.0:*  LISTEN', '0.0.0.0:8123', True),
                ('tcp6  0  0 :::8123            :::*      LISTEN', '127.0.0.1:8123', True),
                ('tcp6  0  0 ::1:8123           :::*      LISTEN', '127.0.0.1:8123', False),
                ('tcp   0  0 192.168.1.10:8123  0.0.0.0:*  LISTEN', '192.168.1.10:8123', True),
                ('tcp   0  0 ::ffff:192.168.1.10:8123 0.0.0.0:* LISTEN', '192.168.1.10:8123', True),
                ('tcp   0  0 0.0.0.0:8231       0.0.0.0:*  LISTEN', '127.0.0.1:8123', False),
                ('tcp   0  0 192.168.1.10:8123  0.0.0.0:*  ESTABLISHED', '127.0.0.1:8123', False),
            ]
            blocks = []
            # netstat 用 shell 函数覆盖(优先于 PATH;Windows 本地会命中
            # NETSTAT.EXE,大写 TCP 令 awk 匹配不上),输出行经环境变量注入。
            blocks.append("netstat() { printf '%s\\n' \"${TAD_NETSTAT_OUT}\"; }\n")
            for idx, (out, probe, conflict) in enumerate(cases):
                want = 0 if conflict else 1
                # set -e 下预检的"可用"返回 1 也会终止脚本:条件语境捕获返回码
                blocks.append(
                    f"TAD_NETSTAT_OUT='{out}'\n"
                    f"rc=0; port_in_use '{probe}' || rc=$?\n"
                    f"test \"$rc\" -eq {want} || exit 7{idx:02d}\n")
            # netstat 缺失 → 视为可用,退化为主流程自身 bind 判定
            blocks.append("netstat() { return 127; }\n"
                          "rc=0; port_in_use '127.0.0.1:8123' || rc=$?\n"
                          "test \"$rc\" -eq 1 || exit 799\n")
            body = '. "$TEST_DIR/functions"\n' + '\n'.join(blocks)
            subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)

    def test_postinst_wizard_port_normalization(self):
        # 端口矩阵:前导零/超长/0/65535/65536/非数字/静默(luodaoyi review
        # #40 第 5 条;CodeRabbit: 向导 regex 已收紧,这里验证 postinst 兜底)。
        cases = [
            ('true', 'false', '8231', '127.0.0.1:8231'),
            ('false', 'true', '9001', '0.0.0.0:9001'),
            ('true', 'false', '65535', '127.0.0.1:65535'),
            ('true', 'false', '65536', '127.0.0.1:8123'),
            ('true', 'false', '0', '127.0.0.1:8123'),
            ('true', 'false', '0009001', '127.0.0.1:9001'),
            ('true', 'false', '999999', '127.0.0.1:8123'),
            ('true', 'false', '000000000000000000009999', '127.0.0.1:9999'),
            ('true', 'false', 'abc', '127.0.0.1:8123'),
            ('', '', '', '127.0.0.1:8123'),
            ('true', 'false', '00000', '127.0.0.1:8123'),
        ]
        for embed, standalone, port, want_addr in cases:
            with self.subTest(port=port or '(empty)'), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                (work / 'etc').mkdir()
                (work / 'var').mkdir()
                env = dict(os.environ, SYNOPKG_PKGDEST=tmp, SYNOPKG_PKGETC=str(work / 'etc'),
                           SYNOPKG_PKGVAR=str(work / 'var'),
                           wizard_embed_mode=embed, wizard_standalone_mode=standalone,
                           wizard_port=port)
                subprocess.run(['sh', str(ROOT / 'synology/scripts/postinst')], env=env,
                               check=True, timeout=30, capture_output=True)
                cfg = (work / 'etc/tad-module.cfg').read_text()
                self.assertIn(f'HTTP_ADDR="{want_addr}"', cfg)

    def test_postinst_keeps_existing_cfg_on_upgrade(self):
        # 已有 cfg(升级/重装)一律不覆盖,向导变量为空也不得回退端口。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            etc.mkdir()
            (work / 'var').mkdir()
            (etc / 'tad-module.cfg').write_text('HTTP_ADDR="127.0.0.1:9999"\nIT87_FORCE_ID=""\n')
            env = dict(os.environ, SYNOPKG_PKGDEST=tmp, SYNOPKG_PKGETC=str(etc),
                       SYNOPKG_PKGVAR=str(work / 'var'), wizard_port='8231',
                       wizard_standalone_mode='true')
            subprocess.run(['sh', str(ROOT / 'synology/scripts/postinst')], env=env,
                           check=True, timeout=30, capture_output=True)
            self.assertIn('HTTP_ADDR="127.0.0.1:9999"', (etc / 'tad-module.cfg').read_text())

    def test_wizard_choice_stashed_when_etc_unwritable_then_consumed(self):
        # 向导选值暂存(var/wizard-install.env)→ root 启动 ensure_cfg 消费
        # 后删除——etc 被 root 属主时选值不能静默丢。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            var = work / 'var'
            etc.mkdir()
            var.mkdir()
            env = dict(os.environ, TEST_DIR=self._shpath(tmp), SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(etc),
                       SYNOPKG_PKGVAR=self._shpath(var), wizard_port='8231')
            # 阻断 cfg 写入:把 etc 目标路径变成目录。
            (etc / 'tad-module.cfg').mkdir()
            subprocess.run(['sh', str(ROOT / 'synology/scripts/postinst')], env=env,
                           check=True, timeout=30, capture_output=True)
            stash = var / 'wizard-install.env'
            self.assertTrue(stash.is_file(), 'etc 不可写时向导选值必须暂存 var')
            self.assertIn('HTTP_ADDR="127.0.0.1:8231"', stash.read_text())
            # root 语境自愈:消费暂存 → cfg 落盘 → 暂存删除。
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            body = r'''
. "$TEST_DIR/functions"
# root 语境恢复可写(占位目录移除)后自愈:消费暂存 → cfg 落盘 → 暂存删除
rm -rf "$TEST_DIR/etc/tad-module.cfg"
PKG_VAR="$TEST_DIR/var"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
HTTP_ADDR="127.0.0.1:8123"
ensure_cfg
grep -Fqx 'HTTP_ADDR="127.0.0.1:8231"' "$CFG_FILE"
test ! -e "$TEST_DIR/var/wizard-install.env"
test "$HTTP_ADDR" = "127.0.0.1:8231"
'''
            subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)

    def test_wrapper_captures_spm_exec_rejection(self):
        # set -e 下 spm-exec 非零退出必须仍记录返回码,退出码原样透传
        # (luodaoyi review #40 第 2 条)。
        if hasattr(os, 'geteuid') and os.geteuid() == 0:
            self.skipTest('uid=0 走直接执行分支,不经过 spm-exec')
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            var = work / 'var'
            var.mkdir()
            fake = work / 'fake-spm-exec'
            fake.write_text('#!/bin/sh\necho "rejected: package not enabled" >&2\nexit 42\n')
            fake.chmod(0o755)
            env = dict(os.environ, SYNOPKG_PKGDEST=tmp, SYNOPKG_PKGVAR=str(var),
                       TAD_SPM_EXEC=str(fake))
            result = subprocess.run(['sh', str(ROOT / 'synology/scripts/start-stop-status'), 'status'],
                                    env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 42, result.stderr)
            log = (var / 'startup-debug.log').read_text()
            self.assertIn('经 spm-exec 提权转发', log)
            self.assertIn('spm-exec 返回 42', log)

    @unittest.skipUnless(shutil.which('node'), 'node unavailable')
    def test_desktop_window_config_passthrough(self):
        # 桌面窗口 config 透传(luodaoyi review #40):AppWindow 构造函数必须
        # 保留 DSM 传入的身份字段(appInstance 等)并透传父类。
        subprocess.run(['node', str(ROOT / 'scripts/test-tadmodule-window.js')],
                       check=True, timeout=30)


if __name__ == '__main__':
    unittest.main()
