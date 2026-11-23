#!/usr/bin/env python3
"""Isolated packaging/startup regressions; never start a daemon or touch DSM."""
import os
from pathlib import Path
import shutil
import sys
import subprocess
import tempfile
import time
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
    POSTINST = ROOT / 'synology/scripts/postinst'

    @staticmethod
    def _definitions(source_text):
        # Load definitions only: no lifecycle action, module loading or hardware.
        return source_text.split('\ncase "${1:-}" in\n')[0]

    _publish_bin = None  # 类级缓存:真实构建的 tad-module(linux/amd64)

    @classmethod
    def _ensure_publish_binary(cls):
        # luodaoyi R12:发布走后端 publish 子命令(link(2)),测试必须用
        # 实际构建的二进制而非旧 ln 回退——构建一次缓存复用;本机无 Go 时
        # 返回 None(调用方 skipTest,ln 回退已删除)。
        import subprocess as sp
        # 仅 linux 宿主可构建可执行的目标平台二进制(ln 回退已删除,发布
        # 测试只能真跑 link(2));Windows/MSYS 无法执行 linux ELF——本机
        # 一律 skip,CI Linux 实跑(luodaoyi R12 也只认真实二进制)。
        if sys.platform != 'linux':
            return None
        if cls._publish_bin and Path(cls._publish_bin).exists():
            return cls._publish_bin
        try:
            r = sp.run(['go', 'build', '-o',
                        Path(tempfile.gettempdir()) / 'tad-module-publish-test-bin',
                        './backend/powerguard'],
                       capture_output=True, text=True,
                       cwd=str(ROOT), timeout=300)
            if r.returncode != 0:
                # 构建失败必须可见:静默 None 会让下游测试以"cfg 未落盘"
                # 的误导性错误失败(luodaoyi R12)
                raise RuntimeError(f'publish test binary build failed: {r.stderr[-500:]}')
            cls._publish_bin = str(Path(tempfile.gettempdir()) / 'tad-module-publish-test-bin')
            return cls._publish_bin
        except (OSError, sp.TimeoutExpired) as e:
            raise RuntimeError(f'publish test binary unavailable: {e}')

    @classmethod
    def _postinst_env(cls, work):
        # luodaoyi review #40 二轮第 2 条:postinst 测试绝不继承宿主 PATH——
        # 沙箱 bin-stubs/ 里放记录型替身(modprobe/grep/tr/head),被调用即
        # 留痕,绝不碰宿主内核模块或 /proc、/dev/urandom。
        stubs = work / 'bin-stubs'
        stubs.mkdir(parents=True, exist_ok=True)
        log = work / 'host-calls.log'
        modprobe_lines = [
            '#!/bin/sh',
            "printf 'modprobe %s\\n' \"$*\" >> '" + str(log) + "'",
        ]
        (stubs / 'modprobe').write_text('\n'.join(modprobe_lines) + '\n')
        (stubs / 'modprobe').chmod(0o755)
        # grep/tr/head 是脚本自身逻辑需要的基元:钉死宿主绝对路径绕开 PATH
        # 继承,仅 modprobe 是危险命令,必须替身。wrapper 引号必须包住路径
        # (Windows 的 Git 安装在 Program Files,含空格;路径归一为 / 形式,
        # MSYS sh 才认)。
        for tool in ('grep', 'tr', 'head'):
            src = shutil.which(tool)
            if src:
                dst = stubs / tool
                if not dst.exists():
                    try:
                        quoted = "'" + str(Path(src).resolve()).replace(chr(92), '/') + "'"
                        wrapper = '#!/bin/sh\nexec ' + quoted + ' "$@"\n'
                        dst.write_text(wrapper)
                        dst.chmod(0o755)
                    except OSError:
                        pass
        # R12:发布走后端 publish 子命令——真实构建的 linux/amd64 二进制放
        # PKGDEST/bin(BIN 解析位置);非 linux 宿主无法执行目标平台 ELF,
        # 调用方按 sys.platform 门控 skip(CI Linux 实跑)。
        binpath = cls._ensure_publish_binary()
        if binpath:
            bin_dst = work / 'bin/tad-module'
            bin_dst.parent.mkdir(parents=True, exist_ok=True)
            if not bin_dst.exists():
                shutil.copyfile(binpath, bin_dst)
                bin_dst.chmod(0o755)
        return {
            'PATH': str(stubs) + ':' + '/bin:/usr/bin',
            'TAD_HOST_CALLS': str(log),
        }

    def _stage_publish_binary(self, work):
        # 把真实构建的 tad-module 放进沙箱 PKGDEST/bin(BIN 解析位置),
        # 让 publish_no_replace 走新入口;构建不可用时返回 False(调用方
        # skipTest——ln 回退已删除,无二进制的发布测试无从谈起)。
        binpath = self._ensure_publish_binary()
        if not binpath:
            return False
        dst = work / 'bin/tad-module'
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(binpath, dst)
        dst.chmod(0o755)
        return True

    def _run_postinst(self, work, extra_env):
        env = dict(os.environ)
        env.update(self._postinst_env(work))
        env.update({k: str(v) for k, v in extra_env.items()})
        return subprocess.run(['sh', str(self.POSTINST)], env=env,
                              check=True, timeout=30, capture_output=True)

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
                # 回退用例不得继承宿主的 SYNOPKG_PKGVAR(否则脚本用显式路径、
                # 测试却期望回退路径,产生误报);仅 env-var 场景显式设置
                env.pop('SYNOPKG_PKGVAR', None)
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

    def test_port_precheck_is_address_family_aware(self):
        # 地址族感知(luodaoyi review #40 二轮第 1 条):跨地址族永可共存
        # (0.0.0.0 通配不妨碍 [::1] 监听,反之亦然);同族内同址或任一侧
        # 通配才冲突;IPv6 通配对 IPv4 目标的占位有 bindv6only 歧义——不
        # 预判失败,只写 AMBIGUOUS 诊断行。netstat 用 shell 函数覆盖(优先
        # 于 PATH),输出行经环境变量注入。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            var_dir = work / 'var'
            var_dir.mkdir()
            env = dict(os.environ, TEST_DIR=tmp,
                       SYNOPKG_PKGDEST=tmp, SYNOPKG_PKGETC=self._shpath(work / 'etc'),
                       SYNOPKG_PKGVAR=self._shpath(var_dir))
            source = self._definitions(self.REAL.read_text())
            (work / 'functions').write_text(source)
            cases = [  # (协议列, 监听地址:端口, 探测目标, 是否应判定冲突)
                # 跨地址族:无论通配与否,永可共存
                ('tcp',  '0.0.0.0:8123',              '[::1]:8123',          False),
                ('tcp6', ':::8123',                   '127.0.0.1:8123',      False),
                ('tcp',  '127.0.0.1:8123',            '[::1]:8123',          False),
                ('tcp6', '[::1]:8123',                '127.0.0.1:8123',      False),
                # 同族 v4:同址/任一侧通配=冲突;不同具体地址=共存
                ('tcp',  '0.0.0.0:8123',              '0.0.0.0:8123',        True),
                ('tcp',  '0.0.0.0:8123',              '192.168.1.5:8123',    True),
                ('tcp',  '127.0.0.1:8123',            '127.0.0.1:8123',      True),
                ('tcp',  '192.168.1.5:8123',          '127.0.0.1:8123',      False),
                # 同族 v6:通配占具体;通配对通配;具体对具体
                ('tcp6', ':::8123',                   '[::1]:8123',          True),
                ('tcp6', ':::8123',                   '[2001:db8::1]:8123',  True),
                ('tcp6', ':::8123',                   '[::]:8123',           True),
                ('tcp6', '[::1]:8123',                '[2001:db8::1]:8123',  False),
                # v4-mapped(::ffff:)归一为 v4 族
                ('tcp6', '::ffff:192.168.1.5:8123',   '192.168.1.5:8123',    True),
                ('tcp6', '::ffff:192.168.1.5:8123',   '[::1]:8123',          False),
                # 非 LISTEN 状态不参与
                ('tcp',  '0.0.0.0:8123-ESTABLISHED',  '0.0.0.0:8123',        False),
                # 其他端口不参与
                ('tcp',  '0.0.0.0:8231',              '127.0.0.1:8123',      False),
            ]
            blocks = [
                # netstat 用 shell 函数覆盖(优先于 PATH;Windows 本地会命中
                # NETSTAT.EXE,大写 TCP 令 awk 匹配不上)。
                "netstat() { printf '%s\\n' \"${TAD_NETSTAT_OUT}\"; }\n",
            ]
            for idx, (proto, la, probe, conflict) in enumerate(cases):
                want = 0 if conflict else 1
                # set -e 下预检的"可用"返回 1 也会终止脚本:条件语境捕获返回码
                la_field = la.replace('-ESTABLISHED', '') + ('  ESTABLISHED' if 'ESTABLISHED' in la else '  LISTEN')
                blocks.append(
                    f"TAD_NETSTAT_OUT='{proto}  0  0 {la_field}  0.0.0.0:*'\n"
                    f"rc=0; port_in_use '{probe}' || rc=$?\n"
                    f"test \"$rc\" -eq {want} || exit 7{idx:02d}\n")
            # netstat 缺失 → 视为可用,退化为主流程自身 bind 判定
            blocks.append("netstat() { return 127; }\n"
                          "rc=0; port_in_use '127.0.0.1:8123' || rc=$?\n"
                          "test \"$rc\" -eq 1 || exit 799\n")
            body = '. "$TEST_DIR/functions"\n' + '\n'.join(blocks)
            subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)
            # v6 通配 → v4 目标:AMBIGUOUS 只诊断不拦截(上面跨族用例已证
            # rc=1),此处验证诊断确实落盘 STARTUP_LOG
            blocks2 = [
                "netstat() { printf '%s\\n' \"${TAD_NETSTAT_OUT}\"; }\n",
                "TAD_NETSTAT_OUT='tcp6  0  0 :::8123  0.0.0.0:*  LISTEN'\n",
                "rc=0; port_in_use '127.0.0.1:8123' || rc=$?\n",
                "test \"$rc\" -eq 1 || exit 801\n",
            ]
            (var_dir / 'startup-debug.log').unlink(missing_ok=True)
            subprocess.run(['sh', '-ec', '. "$TEST_DIR/functions"\n' + '\n'.join(blocks2)],
                           env=env, check=True, timeout=15)
            log = (var_dir / 'startup-debug.log').read_text()
            self.assertIn('AMBIGUOUS :::8123', log)
            self.assertIn('bindv6only', log)

    def test_start_migrates_legacy_addr_before_precheck(self):
        # luodaoyi 三轮 + CodeRabbit R10 Minor:旧 cfg ":8123" 的迁移必须
        # 发生在端口预检之前——直接执行生产脚本的 start 分支(替身隔离
        # netstat/硬件/守护进程),断言到达 serve 时 --http 收到的地址,
        # 而非复刻迁移与预检逻辑。mock daemon 常驻(就绪窗口内 kill -0
        # 真实通过),body 结束后统一清理。
        scenarios = [
            # (cfg 里 HTTP_ADDR, netstat 输出, 期望放行, 期望 serve 地址)
            (':8123',
             'tcp  0  0 192.168.1.5:8123  0.0.0.0:*  LISTEN',
             True, '127.0.0.1:8123'),   # 旧默认+LAN 监听:迁移后共存放行
            (':8123',
             'tcp  0  0 127.0.0.1:8123  0.0.0.0:*  LISTEN',
             False, '127.0.0.1:8123'),  # 迁移后回环真冲突:仍拦截
            ('0.0.0.0:8123',
             'tcp  0  0 127.0.0.1:8123  0.0.0.0:*  LISTEN',
             False, '0.0.0.0:8123'),    # 显式通配保持:迁移不动它,回环监听占位=通配冲突
        ]
        # start 分支的 process_identity 读 /proc/<pid>/stat——MSYS/Windows
        # 无 /proc 进程模型,整个测试只能在 Linux(CI)运行
        if not Path('/proc/self/stat').exists():
            self.skipTest('/proc 不可用(Windows 本机),CI Linux 覆盖')
        for idx, (cfg_addr, netstat_line, expect_pass, final_addr) in enumerate(scenarios):
            with self.subTest(cfg=cfg_addr, netstat=netstat_line), \
                 tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                (work / 'etc').mkdir()
                (work / 'var').mkdir()
                pkg_bin = work / 'bin'
                pkg_bin.mkdir()
                cfg_text = 'HTTP_ADDR="' + cfg_addr + '"\nIT87_FORCE_ID=""\n'
                (work / 'etc/tad-module.cfg').write_text(cfg_text)
                # luodaoyi R14:生产脚本目录常量必须固定——测试用脚本副本,
                # 只 sed 替换 NGINX_CONF_DIR 唯一常量行到私有临时目录并校验
                # 恰替换一处;其余逻辑仍走真实 start,生产文件不动。
                real_text = self.REAL.read_text()
                old_line = 'NGINX_CONF_DIR="/usr/local/etc/nginx/conf.d"'
                new_line = 'NGINX_CONF_DIR="' + self._shpath(work / 'nginx-conf.d') + '"'
                self.assertEqual(real_text.count(old_line), 1,
                                 '生产脚本 NGINX_CONF_DIR 常量行必须恰有一处')
                script_copy = work / 'real-start-stop-status-test'
                script_copy.write_text(real_text.replace(old_line, new_line, 1))
                script_copy.chmod(0o755)
                # 替换校验:副本里不得再残留生产目录常量,且新常量恰一处
                copy_text = script_copy.read_text()
                self.assertNotIn(old_line, copy_text)
                self.assertEqual(copy_text.count(new_line), 1)
                (work / 'functions').write_text(self._definitions(real_text))
                # 假守护进程必须是真实 ELF:daemon_pid 校验 /proc/<pid>/exe
                # 等于 BIN 且 cmdline 第 2 段为 serve——sh 脚本的 exe 是解释
                # 器,过不了校验(此前就绪探测从未真实通过)。记录 --http 参数
                # 后常驻;退出机制(luodaoyi R15):轮询本用例私有目录的退出
                # 通知文件($QUIT),看到即删文件作回执并退出——finally 只发
                # 通知+有界等待回执,不向任何 PID 发信号、不扫进程表;
                # alarm(120) 仅兜底(测试崩溃等极端情形)。
                mock_src = work / 'mock-daemon.c'
                mock_src.write_text(r'''
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
int main(int argc, char **argv) {
    alarm(120);
    const char *cap = getenv("CAPTURE");
    const char *quit = getenv("QUIT");
    for (int i = 1; i < argc - 1; i++) {
        if (strcmp(argv[i], "--http") == 0 && cap) {
            FILE *f = fopen(cap, "w");
            if (f) { fputs(argv[i + 1], f); fclose(f); }
        }
    }
    for (;;) {
        if (quit && access(quit, F_OK) == 0) {
            unlink(quit);   /* 删除通知文件 = 退出回执 */
            return 0;
        }
        usleep(50000);      /* 50ms 轮询 */
    }
}
''')
                mock_bin = pkg_bin / 'tad-module'
                cc = shutil.which('cc') or shutil.which('gcc')
                if not cc:
                    self.skipTest('容器无 C 编译器,mock daemon 需要 ELF')
                import subprocess as _sp
                _r = _sp.run([cc, '-o', str(mock_bin), str(mock_src)],
                             capture_output=True, text=True, timeout=60)
                if _r.returncode != 0:
                    self.fail(f'mock daemon 编译失败: {_r.stderr[-300:]}')
                mock_bin.chmod(0o755)
                env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                           SYNOPKG_PKGDEST=self._shpath(tmp),
                           SYNOPKG_PKGETC=self._shpath(work / 'etc'),
                           SYNOPKG_PKGVAR=self._shpath(work / 'var'),
                           TAD_NETSTAT_OUT=netstat_line,
                           CAPTURE=str(work / 'captured-http'),
                           QUIT=str(work / 'mock-daemon.quit'))
                stub_env = self._postinst_env(work)
                # netstat/nginx 替身:副本脚本按 PATH 调 netstat -tln 与
                # nginx(ensure_nginx 的 -t/reload);输出由环境变量注入。
                # luodaoyi R14:nginx 配置目录已由脚本副本的常量替换指进
                # 私有临时目录,nginx 本体替身化,宿主系统副作用全隔离
                stubs = work / 'bin-stubs'
                netstat_stub = stubs / 'netstat'
                netstat_stub.write_text('#!/bin/sh\ncase "$1" in -tlnp|-tln) printf \'%s\\n\' "$TAD_NETSTAT_OUT" ;; esac\nexit 0\n')
                netstat_stub.chmod(0o755)
                nginx_stub = stubs / 'nginx'
                nginx_stub.write_text('#!/bin/sh\nprintf \'nginx %s\\n\' "$*" >> "$TAD_HOST_CALLS"\nexit 0\n')
                nginx_stub.chmod(0o755)
                # ensure_nginx 的 reload 走 systemctl:容器无 systemd,替身放行
                systemctl_stub = stubs / 'systemctl'
                systemctl_stub.write_text('#!/bin/sh\nexit 0\n')
                systemctl_stub.chmod(0o755)
                run_env = {**env, **stub_env}
                # 直接调用副本脚本(经脚本自身的 start 分支);mock daemon
                # 自限寿命(alarm 120s)+finally 退出通知协作清理
                pidfile = work / 'var' / 'tad-module.pid'
                proc = None
                try:
                    proc = subprocess.run(['sh', str(script_copy), 'start'], env=run_env,
                                          capture_output=True, text=True, timeout=30)
                    capture = (work / 'captured-http').read_text() if (work / 'captured-http').exists() else ''
                    if expect_pass:
                        if proc.returncode != 0:
                            startup_log = (work / 'var/startup-debug.log').read_text() if (work / 'var/startup-debug.log').exists() else ''
                            self.fail(f'场景{idx}: 正向场景应放行,rc={proc.returncode} stderr={proc.stderr!r} log={startup_log[-500:]!r}')
                        self.assertEqual(capture, final_addr,
                                         f'场景{idx}: serve 应收到迁移后地址 {final_addr},实际 {capture!r}')
                        self.assertTrue(
                            (work / 'etc/tad-module.cfg').read_text().startswith('HTTP_ADDR="' + final_addr + '"'),
                            f'场景{idx}: 迁移应写回 cfg')
                    else:
                        # luodaoyi R13:负向场景三重断言——退出码 1、诊断落盘、
                        # 无守护进程启动(不能只靠"没捕获到参数")
                        self.assertEqual(proc.returncode, 1,
                                         f'场景{idx}: 预检应拦截 exit 1,实际 rc={proc.returncode} capture={capture!r}')
                        self.assertEqual(capture, '',
                                         f'场景{idx}: 预检应拦截,daemon 不得收到 --http,实际 {capture!r}')
                        startup_log = (work / 'var/startup-debug.log').read_text() if (work / 'var/startup-debug.log').exists() else ''
                        self.assertIn('已被其他进程占用', startup_log,
                                      f'场景{idx}: 预检诊断必须落盘,日志={startup_log!r}')
                        self.assertIn(final_addr, startup_log,
                                      f'场景{idx}: 诊断须含冲突地址 {final_addr}')
                        self.assertFalse(pidfile.exists(),
                                         f'场景{idx}: 拦截场景不得留下 PID 文件')
                        self.assertFalse((work / 'var' / 'tad-module.log').exists(),
                                         f'场景{idx}: 拦截场景守护进程不得启动')
                finally:
                    # luodaoyi R15:退出通知协作清理——向本用例私有目录写
                    # 通知文件,mock 看到即删文件作回执并退出;finally 有界
                    # 等待回执,不向任何 PID 发信号、不扫全机进程表(裸 PID
                    # 复用/子串匹配都可能误伤宿主进程)。alarm(120) 仅兜底
                    # 测试崩溃等极端情形。仅正向场景等待(负向场景 daemon
                    # 未启动,无回执可等;alarm 兜底覆盖一切意外)。
                    quit_file = work / 'mock-daemon.quit'
                    try:
                        if expect_pass and pidfile.exists():
                            quit_file.write_text('')
                            # 有界等待退出回执(通知文件被 mock 删除)
                            deadline = time.monotonic() + 5.0
                            while quit_file.exists() and time.monotonic() < deadline:
                                time.sleep(0.05)
                            self.assertFalse(quit_file.exists(),
                                             'mock daemon 未在 5 秒内确认退出回执(alarm 兜底仍会自杀)')
                    except OSError:
                        pass

    def test_postinst_wizard_port_normalization(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
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
                self._run_postinst(work, {
                    'SYNOPKG_PKGDEST': tmp, 'SYNOPKG_PKGETC': work / 'etc',
                    'SYNOPKG_PKGVAR': work / 'var',
                    'wizard_embed_mode': embed, 'wizard_standalone_mode': standalone,
                    'wizard_port': port,
                })
                cfg = (work / 'etc/tad-module.cfg').read_text()
                self.assertIn(f'HTTP_ADDR="{want_addr}"', cfg)
                self._assert_stub_modprobe_only(work)

    def _assert_stub_modprobe_only(self, work):
        # luodaoyi review #40 二轮第 2 条:postinst 测试绝不触碰宿主内核——
        # modprobe 一律是记录型替身;断言"若发生调用,必是替身留痕"(即真实
        # modprobe 从未被 PATH 放行)。留痕内容同时证明替身链路可用。
        calls = work / 'host-calls.log'
        if calls.exists():
            for line in calls.read_text().splitlines():
                self.assertTrue(line.startswith('modprobe '),
                                f'替身日志出现非 modprobe 调用: {line}')

    def test_postinst_keeps_existing_cfg_on_upgrade(self):
        # 已有 cfg(升级/重装)一律不覆盖,向导变量为空也不得回退端口。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            etc.mkdir()
            (work / 'var').mkdir()
            (etc / 'tad-module.cfg').write_text('HTTP_ADDR="127.0.0.1:9999"\nIT87_FORCE_ID=""\n')
            self._run_postinst(work, {
                'SYNOPKG_PKGDEST': tmp, 'SYNOPKG_PKGETC': etc,
                'SYNOPKG_PKGVAR': work / 'var', 'wizard_port': '8231',
                'wizard_standalone_mode': 'true',
            })
            self.assertIn('HTTP_ADDR="127.0.0.1:9999"', (etc / 'tad-module.cfg').read_text())
            self._assert_stub_modprobe_only(work)

    def test_wizard_choice_stashed_when_etc_unwritable_then_consumed(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
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
            stub_env = self._postinst_env(work)
            subprocess.run(['sh', str(self.POSTINST)],
                           env={**env, **stub_env},
                           check=True, timeout=30, capture_output=True)
            stash = var / 'wizard-install.env'
            self.assertTrue(stash.is_file(), 'etc 不可写时向导选值必须暂存 var')
            self.assertIn('HTTP_ADDR="127.0.0.1:8231"', stash.read_text())
            # root 语境自愈:消费暂存 → cfg 落盘 → 暂存删除。
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            body = r'''
. "$TEST_DIR/functions"
# root 语境恢复可写(占位目录移除)后自愈:暂存值以纯数据提取 → 原子落盘
# → 暂存删除;HTTP_ADDR 变量由 start 分支重读 cfg 获得,ensure_cfg 不再
# source 暂存(root 提权面),故此处只断言 cfg 内容与暂存消费
rm -rf "$TEST_DIR/etc/tad-module.cfg"
PKG_VAR="$TEST_DIR/var"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
grep -Fqx 'HTTP_ADDR="127.0.0.1:8231"' "$CFG_FILE"
test ! -e "$TEST_DIR/var/wizard-install.env"
'''
            subprocess.run(['sh', '-ec', body], env=env, check=True, timeout=15)

    def test_ensure_cfg_rejects_malicious_stash_values(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # luodaoyi 合并门槛:向导暂存值当纯数据读,shell 元字符/畸形值必须
        # 被拒——植入的注入形态绝不执行;且暂存必须是"恰好一条合法监听
        # 记录"(向导地址形式+1-65535),重复/额外记录/越界端口整文件作废。
        # 哨兵路径全部落在用例自己的临时目录(luodaoyi 四轮:测试标记只写
        # 自己的 tmp),绝不碰全局 /tmp。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            (work / 'etc').mkdir()
            (work / 'var').mkdir()
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            sentinel = work / 'pwned-sentinel'
            stash = work / 'var/wizard-install.env'
            stash_lines = [
                f'HTTP_ADDR="127.0.0.1:8231; rm -rf {sentinel}"',
                'evil_fn() { :; }',
                f'HTTP_ADDR="$(touch {sentinel})"',
                'HTTP_ADDR="127.0.0.1:8232"',   # 唯一合法行,应被采纳
                'HTTP_ADDR="127.0.0.1:8233',    # 未闭合引号:拒
                'HTTP_ADDR=":8234"',            # 空主机:拒
            ]
            stash.write_text('\n'.join(stash_lines) + '\n')
            env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                       SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(work / 'etc'),
                       SYNOPKG_PKGVAR=self._shpath(work / 'var'))
            body = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
test ! -e "$TEST_DIR/pwned-sentinel" || exit 81
# 单完整记录语义:多记录+额外行=非本套件 postinst 签名,整文件作废,
# 落默认 8123;不可信文件不消费(保留),便于排查
grep -Fqx 'HTTP_ADDR="127.0.0.1:8123"' "$CFG_FILE" || exit 82
test -f "$TEST_DIR/var/wizard-install.env" || exit 83
"""
            stub_env = self._postinst_env(work)
            body_env = {**env, **stub_env}
            body_env.setdefault('TEST_DIR', env.get('TEST_DIR', self._shpath(tmp)))
            subprocess.run(['sh', '-ec', body], env=body_env, check=True, timeout=15)
            self.assertFalse(sentinel.exists(), '注入内容被执行')
            log = (work / 'var/startup-debug.log').read_text()
            self.assertIn('整文件作废', log)
            self._assert_stub_modprobe_only(work)

    def test_ensure_cfg_rejects_duplicate_extra_and_bad_ports(self):
        # luodaoyi 四轮:暂存只接受恰好一条完整有效监听记录(向导地址形式
        # +1-65535);重复、额外记录、越界端口一律整文件作废回落默认。
        cases = [
            ('HTTP_ADDR="127.0.0.1:9001"\nHTTP_ADDR="127.0.0.1:9002"\n',
             '127.0.0.1:8123', '重复记录:作废'),
            ('HTTP_ADDR="127.0.0.1:9001"\nevil_fn() { :; }\n',
             '127.0.0.1:8123', '额外记录:作废'),
            ('HTTP_ADDR="127.0.0.1:65536"\n',
             '127.0.0.1:8123', '端口越界 65536:拒'),
            ('HTTP_ADDR="127.0.0.1:0"\n',
             '127.0.0.1:8123', '端口 0:拒'),
            ('HTTP_ADDR=":8123"\n',
             '127.0.0.1:8123', '空主机:拒'),
            ('HTTP_ADDR="10.0.0.5:8123"\n',
             '127.0.0.1:8123', '非向导地址形式:拒'),
            ('HTTP_ADDR="127.2.3.4:8123"\n',
             '127.0.0.1:8123', '宽泛 127.* 段:拒(向导只产 127.0.0.1)'),
            ('HTTP_ADDR="0.0.0.0:9001"\n',
             '0.0.0.0:9001', '独立模式通配:采纳'),
            ('HTTP_ADDR="127.0.0.1:65535"\n',
             '127.0.0.1:65535', '端口上限 65535:采纳'),
        ]
        for content, want_addr, name in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                if not self._stage_publish_binary(work):
                    self.skipTest('go 构建不可用,ln 回退已删除')
                if not self._stage_publish_binary(work):
                    self.skipTest('go 构建不可用,ln 回退已删除')
                if not self._stage_publish_binary(work):
                    self.skipTest('go 构建不可用,ln 回退已删除')
                (work / 'etc').mkdir()
                (work / 'var').mkdir()
                (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
                (work / 'var/wizard-install.env').write_text(content)
                env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                           SYNOPKG_PKGDEST=self._shpath(tmp),
                           SYNOPKG_PKGETC=self._shpath(work / 'etc'),
                           SYNOPKG_PKGVAR=self._shpath(work / 'var'))
                body = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
grep -Fqx 'HTTP_ADDR="__ADDR__"' "$CFG_FILE" || exit 84
"""
                subprocess.run(['sh', '-ec', body.replace('__ADDR__', want_addr)],
                               env=env, check=True, timeout=15)

    def test_ensure_cfg_atomic_retry_preserves_stash_and_existing_cfg(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # luodaoyi 四轮:①首次提交失败(etc 不可写)→ 暂存保留,已有有效配置
        # 不被破坏;②恢复可写后重试 → 按暂存端口落盘并消费暂存;③已有有效
        # 配置(用户改过)一律不动。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            if not self._stage_publish_binary(work):
                self.skipTest('go 构建不可用,ln 回退已删除')
            etc = work / 'etc'
            var = work / 'var'
            etc.mkdir()
            var.mkdir()
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                       SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(etc),
                       SYNOPKG_PKGVAR=self._shpath(var))
            # ① 提交失败:cfg 路径被目录占位 → 跳过自愈,暂存保留
            (etc / 'tad-module.cfg').mkdir()
            (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9101"\n')
            body_fail = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
# 无预检查语义:ln 对目录目标"移入"或失败,总之 CFG_FILE 不是普通文件
test ! -f "$CFG_FILE" || exit 85
test -f "$TEST_DIR/var/wizard-install.env" || exit 86
"""
            subprocess.run(['sh', '-ec', body_fail], env=env, check=True, timeout=15)
            # ② 恢复可写后重试:按暂存端口落盘,暂存消费(清占位目录及其内容)
            if (etc / 'tad-module.cfg').is_dir():
                shutil.rmtree(etc / 'tad-module.cfg')
            else:
                (etc / 'tad-module.cfg').unlink(missing_ok=True)
            body_retry = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
grep -Fqx 'HTTP_ADDR="127.0.0.1:9101"' "$CFG_FILE" || exit 87
test ! -e "$TEST_DIR/var/wizard-install.env" || exit 88
# 无残 tmp(ensure_cfg 在 etc 下创建 tad-module.cfg.tmp.*,提交后已消费;
# glob 无匹配返回非 0=通过)
for _f in "$TEST_DIR"/etc/tad-module.cfg.tmp.*; do
  [ -e "$_f" ] && exit 89
done
exit 0
"""
            subprocess.run(['sh', '-ec', body_retry], env=env, check=True, timeout=15)
            # ③ 已有有效配置不变:ensure_cfg 直接短路,连暂存都不消费
            (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9102"\n')
            body_existing = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
grep -Fqx 'HTTP_ADDR="127.0.0.1:9101"' "$CFG_FILE" || exit 90
test -f "$TEST_DIR/var/wizard-install.env" || exit 91
"""
            subprocess.run(['sh', '-ec', body_existing], env=env, check=True, timeout=15)
            # ②' 的 tmp glob 已随临时文件位置改为 etc(受信目标目录)
            # ④ 临时文件创建失败:PATH 沙箱里 chmod/创建替身——noclobber
    # 撞名模拟(预先占住三个候选名),ensure_cfg 必须放弃本轮:cfg 未落盘、
    # 暂存保留、日志记真实结果(luodaoyi 五轮:创建失败不静默丢选值)
    def test_ensure_cfg_tmp_creation_failure_keeps_stash(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            var = work / 'var'
            etc.mkdir()
            var.mkdir()
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9201"\n')
            env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                       SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(etc),
                       SYNOPKG_PKGVAR=self._shpath(var))
            body = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
# 模拟 noclobber 碰撞:预占三个候选临时文件名
for _i in 1 2 3; do : > "${CFG_FILE}.tmp.${$}-${_i}"; done
ensure_cfg
test ! -e "$CFG_FILE" || exit 92
test -f "$TEST_DIR/var/wizard-install.env" || exit 93
grep -q "临时文件创建失败" "$TEST_DIR/var/startup-debug.log" || exit 94
# 清掉预占文件供断言收尾
for _i in 1 2 3; do rm -f "${CFG_FILE}.tmp.${$}-${_i}"; done
exit 0
"""
            stub_env = self._postinst_env(work)
            body_env = {**env, **stub_env}
            body_env.setdefault('TEST_DIR', env.get('TEST_DIR', self._shpath(tmp)))
            subprocess.run(['sh', '-ec', body], env=body_env, check=True, timeout=15)
            # 恢复后重试:按暂存端口 9201 落盘(失败路径不永久吞掉选值)
            body_retry = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
grep -Fqx 'HTTP_ADDR="127.0.0.1:9201"' "$CFG_FILE" || exit 95
test ! -e "$TEST_DIR/var/wizard-install.env" || exit 96
exit 0
"""
            subprocess.run(['sh', '-ec', body_retry], env=env, check=True, timeout=15)

    # ⑤ 写入或提交失败(mv 替身失败):暂存保留、残 tmp 清理、日志记真实
    # 结果;恢复后重试同样保端口(luodaoyi 五轮:真实走到失败路径)
    def test_ensure_cfg_commit_failure_retries_and_preserves_stash(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        for scenario in ('write-fail', 'mv-fail'):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                if not self._stage_publish_binary(work):
                    self.fail('发布二进制构建失败(ln 回退已删除,无 helper 无从验证)')
                etc = work / 'etc'
                var = work / 'var'
                etc.mkdir()
                var.mkdir()
                (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
                (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9301"\n')
                # luodaoyi R13:失败注入用目标过滤的 publish 包装器——仅
                # 配置提交失败一次,其余转交真实二进制,证明「publish 已被
                # 调用却失败后暂存保留」;恢复段先还原真身再重试
                restore = self._wrap_publish_filter(work, 'tad-module.cfg')
                env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                           SYNOPKG_PKGDEST=self._shpath(tmp),
                           SYNOPKG_PKGETC=self._shpath(etc),
                           SYNOPKG_PKGVAR=self._shpath(var))
                stub_env = self._postinst_env(work)
                body = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
test ! -e "$CFG_FILE" || exit 97
test -f "$TEST_DIR/var/wizard-install.env" || exit 98
test ! -e "$TEST_DIR/pwned-sentinel" || exit 99
exit 0
"""
                body_env = {**env, **stub_env}
                subprocess.run(['sh', '-ec', body], env=body_env, check=True, timeout=15)
                # 恢复真实二进制重试:按暂存端口落盘并消费
                restore()
                # 恢复真实 PATH 重试:按暂存端口落盘并消费
                body_retry = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
grep -Fqx 'HTTP_ADDR="127.0.0.1:9301"' "$CFG_FILE" || exit 100
test ! -e "$TEST_DIR/var/wizard-install.env" || exit 101
exit 0
"""
                retry_env = {k: v for k, v in env.items() if k != 'PATH'}
                retry_env['PATH'] = '/bin:/usr/bin'
                retry_env.setdefault('TEST_DIR', self._shpath(tmp))
                retry_env['SYNOPKG_PKGDEST'] = self._shpath(tmp)
                retry_env['SYNOPKG_PKGETC'] = self._shpath(etc)
                retry_env['SYNOPKG_PKGVAR'] = self._shpath(var)
                subprocess.run(['sh', '-ec', body_retry], env=retry_env, check=True, timeout=15)

    def _inject_postinst_stub(self, work, tool, behavior):
        # 向 _postinst_env 的 stubs 目录写入指定行为的命令替身(函数内 return
        # 语义);与隔离沙箱同一目录,天然排在宿主 PATH 之前
        stubs = work / 'bin-stubs'
        stubs.mkdir(exist_ok=True)
        stub = stubs / tool
        stub.write_text(f'#!/bin/sh\n{behavior}\n')
        stub.chmod(0o755)

    def _wrap_publish_filter(self, work, dest_suffix):
        # luodaoyi R13:失败注入不再拦 chmod(那只证明权限失败,证明不了
        # 「publish 已被调用却失败后暂存仍保留」)——把 PKGDEST/bin 下的
        # 真实二进制改名 .real,原位放同路径包装器:仅当 publish 的目标
        # 以 dest_suffix 结尾时失败一次(模拟 link(2) EEXIST),其余调用
        # 全部转交真实二进制。返回恢复函数(测完/重试前还原真身)。
        bin_dst = work / 'bin/tad-module'
        real = work / 'bin/.tad-module.real'
        shutil.move(str(bin_dst), str(real))
        qreal = "'" + self._shpath(real) + "'"
        wrapper = (
            '#!/bin/sh\n'
            'if [ "$1" = "publish" ] && [ $# -ge 3 ]; then\n'
            '  case "$3" in\n'
            '    *' + dest_suffix + ') exit 1 ;;\n'
            '  esac\n'
            'fi\n'
            f'exec {qreal} "$@"\n'
        )
        bin_dst.write_text(wrapper)
        bin_dst.chmod(0o755)
        return lambda: (bin_dst.unlink(missing_ok=True), real.exists() and shutil.move(str(real), str(bin_dst)))

    def test_postinst_failure_paths_preserve_cfg_and_stash_wizard(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # luodaoyi 六轮:postinst 的每个失败步骤(临时文件创建失败/权限不可达/
        # 提交 mv 失败)都必须——不留半份正式 cfg、向导选值原子暂存、下次启动
        # 可恢复;全部用替身真实走到失败分支,不只测已有暂存文件的恢复。
        scenarios = [
            ('tmp-create-fail', 'noclobber-collision'),
            ('chmod-fail', 'chmod-reject'),
            ('mv-fail', 'mv-reject'),
            # 真写入失败:grep 校验替身恒失败(写入产物不完整)——同一
            # 「写入或校验失败」分支,committed=0→暂存
            ('write-fail', 'grep-reject'),
        ]
        for name, stub_kind in scenarios:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp)
                if not self._stage_publish_binary(work):
                    self.skipTest('go 构建不可用,ln 回退已删除')
                etc = work / 'etc'
                var = work / 'var'
                etc.mkdir()
                var.mkdir()
                env = dict(os.environ, SYNOPKG_PKGDEST=self._shpath(tmp),
                           SYNOPKG_PKGETC=self._shpath(etc),
                           SYNOPKG_PKGVAR=self._shpath(var),
                           wizard_port='9401')
                if stub_kind == 'noclobber-collision':
                    # 预占三个候选临时名:通过 TAD_CFG_TMP_RACE 钉死 postinst
                    # 的候选名(生产恒 $$ 唯一),noclobber 循环全失败
                    env['TAD_CFG_TMP_RACE'] = '990001'
                    pre = 'for _i in 1 2 3; do : > "$SYNOPKG_PKGETC/tad-module.cfg.tmp.990001-${_i}" 2>/dev/null; done'
                    subprocess.run(['sh', '-ec', pre], env=env, check=True, timeout=15)
                # 统一从 _postinst_env 起步(luodaoyi 七轮:隔离工具目录永远
                # 前置,宿主 PATH 不得出现在替身之前);故障替身写入同一个
                # stubs 目录,无继承 PATH 参与调用
                stub_env = self._postinst_env(work)
                if stub_kind == 'chmod-reject':
                    self._inject_postinst_stub(work, 'chmod', 'return 1')
                elif stub_kind == 'grep-reject':
                    # 校验替身只拦配置 tmp 的校验(模拟配置写入产物不完整),
                    # 暂存文件的校验放行
                    self._inject_postinst_stub(work, 'grep',
                        'case "$3" in *tad-module.cfg.tmp*) exit 1 ;; esac\n'
                        'exit 0')
                elif stub_kind == 'mv-reject':
                    # luodaoyi R13:拦 chmod 只证明权限失败;这里用目标过滤
                    # 的 publish 包装器——仅配置提交(目标 tad-module.cfg)
                    # 失败一次,其余调用(含暂存发布)转交真实二进制,证明
                    # 「publish 已被调用却失败后暂存仍保留」
                    restore = self._wrap_publish_filter(work, 'tad-module.cfg')
                    self._publish_restore = restore
                run_env = {**env, **stub_env}
                subprocess.run(['sh', str(self.POSTINST)], env=run_env,
                               check=True, timeout=30, capture_output=True)
                # PATH 隔离证明:modprobe 调用若发生,必是沙箱替身留痕
                # (grep 替身场景会令 it87 探测误判"已加载"而跳过 modprobe,
                # 属替身副作用而非隔离失效——不要求命中,只要求零真身调用)
                calls = work / 'host-calls.log'
                if calls.exists():
                    self._assert_stub_modprobe_only(work)
                # 原配置不变:正式 cfg 不存在(绝不留半份)
                self.assertFalse((etc / 'tad-module.cfg').exists(),
                                 f'{name}: 不得留下正式 cfg')
                # 向导选值原子暂存,下次启动可恢复。
                # chmod 替身场景:替身连暂存的 chmod 600 也拒,按权限前置
                # 检查语义暂存同样不可达——无 cfg、无暂存、日志如实记录;
                # mv-reject(publish 被调用但 EEXIST 失败)与 grep 拆分场景
                # 暂存必须完整——这正是「发布失败不丢选值」要证明的
                stash = var / 'wizard-install.env'
                if stub_kind == 'chmod-reject':
                    self.assertFalse(stash.exists(),
                                     f'{name}: chmod 替身下暂存同样不可达')
                else:
                    self.assertTrue(stash.is_file(), f'{name}: 向导选值必须暂存')
                    self.assertIn('HTTP_ADDR="127.0.0.1:9401"', stash.read_text())
                # 无残 tmp(noclobber-collision 的预占文件属测试夹具,先清)
                if stub_kind == 'noclobber-collision':
                    for f in etc.glob('tad-module.cfg.tmp.990001-*'):
                        f.unlink()
                leftovers = [p.name for p in etc.iterdir() if '.tmp.' in p.name]
                self.assertEqual(leftovers, [], f'{name}: 残留临时文件 {leftovers}')
                # 下次启动恢复:ensure_cfg 按暂存端口落盘并消费
                # (chmod-reject 场景暂存未写成,跳过恢复断言;
                #  mv-reject 恢复前先还原真实二进制,否则发布仍被拦)
                (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
                if stub_kind == 'mv-reject':
                    self._publish_restore()
                if stub_kind != 'chmod-reject':
                    body = (
                        '. "$TEST_DIR/functions"\n'
                        'CFG_FILE="$TEST_DIR/etc/tad-module.cfg"\n'
                        'STARTUP_LOG="$TEST_DIR/var/startup-debug.log"\n'
                        'ensure_cfg\n'
                        'grep -Fqx \'HTTP_ADDR="127.0.0.1:9401"\' "$CFG_FILE" || exit 102\n'
                        'test ! -e "$TEST_DIR/var/wizard-install.env" || exit 103\n'
                        'exit 0\n'
                    )
                    # 恢复段 PATH 不得含故障替身目录(stubs):否则 ensure_cfg
                    # 的 mv/chmod 也被拦,测的就不是恢复本身;TEST_DIR 为 shell
                    # 体依赖(postinst 阶段不需要故 env 未含)
                    clean_env = {k: v for k, v in env.items() if k != 'PATH'}
                    clean_env['PATH'] = '/bin:/usr/bin'
                    clean_env['TEST_DIR'] = self._shpath(tmp)
                    subprocess.run(['sh', '-ec', body], env=clean_env, check=True, timeout=15)

    def test_competing_writer_wins_publication(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # luodaoyi 八轮:最终提交必须原子 no-replace——竞争写入者恰在提交
        # 时刻落盘的胜出配置字节必须原样保留。竞争注入:grep 替身在
        # "stage 写入完成、ln 提交前一刻"钩中,先让竞争者落盘再放行校验,
        # 其后 ln 必然 no-replace 失败。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            var = work / 'var'
            etc.mkdir()
            var.mkdir()
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            winner_path = etc / 'winner.cfg'
            winner = ('HTTP_ADDR="127.0.0.1:60001"\n'
                      '# competing writer won the race\n'
                      'IT87_FORCE_ID="IT8620E"\n')
            winner_path.write_text(winner)
            (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9501"\n')
            # grep 替身:$3 是 stage 文件时,先把胜出配置竞争落盘到正式路径,
            # 再 exec 真身 grep 做原本的校验(校验自身必须通过,竞争发生在
            # 校验之后、ln 之前——正是被诟病的窗口)
            real_grep = shutil.which('grep')
            qg = "'" + str(Path(real_grep).resolve()).replace(chr(92), '/') + "'"
            qwin = "'" + self._shpath(winner_path) + "'"
            qcfg = "'" + self._shpath(etc / 'tad-module.cfg') + "'"
            stub_body = ('case "$3" in *tad-module.cfg.tmp*) '
                         f'cat {qwin} > {qcfg} ;; esac' + chr(10) +
                         f'exec {qg} "$@"')
            self._inject_postinst_stub(work, 'grep', stub_body)
            env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                       SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(etc),
                       SYNOPKG_PKGVAR=self._shpath(var))
            stub_env = self._postinst_env(work)
            body = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
# 胜出配置字节原样保留
grep -Fqx 'HTTP_ADDR="127.0.0.1:60001"' "$CFG_FILE" || exit 104
grep -Fqx '# competing writer won the race' "$CFG_FILE" || exit 105
grep -Fqx 'IT87_FORCE_ID="IT8620E"' "$CFG_FILE" || exit 106
# 本方未覆盖:暂存不消费,待下次启动重新评估
test -f "$TEST_DIR/var/wizard-install.env" || exit 107
# 本方临时文件已清理(ln 是硬链接,提交失败也须 unlink)
for _f in "$TEST_DIR"/etc/tad-module.cfg.tmp.*; do
  [ -e "$_f" ] && exit 108
done
exit 0
"""
            ensure_env = {**env, **stub_env}
            ensure_env['TEST_DIR'] = self._shpath(tmp)
            subprocess.run(['sh', '-ec', body], env=ensure_env, check=True, timeout=15)
            # 日志记真实结果(竞争失败不静默)
            log = (var / 'startup-debug.log').read_text()
            self.assertIn('不覆盖', log)

    def test_competing_dir_creation_and_file_race(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # CodeRabbit R9:发布原语必须对「目标为目录被并发创建」与「普通文件
        # 竞争」两种占位都原子失败,不依赖预检查。
        # ① 目标目录并发创建:入口检查时 cfg 不存在,ensure_cfg 运行途中
        #    (grep 钩子触发点=stage 已写入、ln 未执行)竞争者 mkdir 占位——
        #    ln"移入"目录或失败,结果验证 [ -f ] 判提交失败,目录字节保留
        # ② 普通文件竞争:由 test_competing_writer_wins_publication 覆盖
        #    (grep 钩子 cat 胜出配置),此处断言其仍存在且通过
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            var = work / 'var'
            etc.mkdir()
            var.mkdir()
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9701"\n')
            # grep 替身钩子:stage 写完、ln 未执行时竞争 mkdir 目录占位
            stub_body = ('case "$3" in *tad-module.cfg.tmp*) '
                         'mkdir -p "$TEST_DIR/etc/tad-module.cfg" ;; esac' + chr(10) +
                         f'exec {chr(39)}{str(Path(shutil.which("grep")).resolve()).replace(chr(92), "/")}{chr(39)} "$@"')
            self._inject_postinst_stub(work, 'grep', stub_body)
            env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                       SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(etc),
                       SYNOPKG_PKGVAR=self._shpath(var))
            stub_env = self._postinst_env(work)
            ensure_env = {**env, **stub_env}
            ensure_env['TEST_DIR'] = self._shpath(tmp)
            body = r"""
. "$TEST_DIR/functions"
CFG_FILE="$TEST_DIR/etc/tad-module.cfg"
STARTUP_LOG="$TEST_DIR/var/startup-debug.log"
ensure_cfg
# 目录占位不是普通文件:提交必须失败(不覆盖目录)
test ! -f "$CFG_FILE" || exit 111
test -d "$CFG_FILE" || exit 112
# 暂存不消费(重试语义)
test -f "$TEST_DIR/var/wizard-install.env" || exit 113
exit 0
"""
            subprocess.run(['sh', '-ec', body], env=ensure_env, check=True, timeout=15)
            log = (var / 'startup-debug.log').read_text()
            self.assertIn('不覆盖', log)
            # 竞争写入者(普通文件)测试仍在且通过
            self.assertTrue(hasattr(type(self), 'test_competing_writer_wins_publication'))

    def test_partial_write_leaves_stage_recoverable(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # luodaoyi 八轮第二项:真正的部分写入(stage 文件在校验前被截断,
        # 模拟写盘中断)——关键行校验必失败 → committed=0 → 向导选值原子
        # 暂存、无半份正式 cfg,下次启动重试可完整恢复。
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            etc = work / 'etc'
            var = work / 'var'
            etc.mkdir()
            var.mkdir()
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9601"\n')
            # grep 替身:$3 是 stage 文件时先把它截成一半(部分写入),再
            # exec 真身 grep 做原校验——截断后关键行(最后一行)必然缺失
            real_grep = shutil.which('grep')
            qg = "'" + str(Path(real_grep).resolve()).replace(chr(92), '/') + "'"
            stub_body = ('case "$3" in *tad-module.cfg.tmp*) '
                         'n=$(wc -c < "$3"); '
                         'head -c $((n / 2)) "$3" > "$3.part" && mv "$3.part" "$3" ;; esac' + chr(10) +
                         f'exec {qg} "$@"')
            self._inject_postinst_stub(work, 'grep', stub_body)
            env = dict(os.environ, SYNOPKG_PKGDEST=self._shpath(tmp),
                       SYNOPKG_PKGETC=self._shpath(etc),
                       SYNOPKG_PKGVAR=self._shpath(var),
                       wizard_port='9601')
            stub_env = self._postinst_env(work)
            run_env = {**env, **stub_env}
            subprocess.run(['sh', str(self.POSTINST)], env=run_env,
                           check=True, timeout=30, capture_output=True)
            # 无半份正式 cfg
            self.assertFalse((etc / 'tad-module.cfg').exists(),
                             '部分写入不得留下正式 cfg')
            # 向导选值原子暂存(可恢复)
            stash = var / 'wizard-install.env'
            self.assertTrue(stash.is_file(), '部分写入后向导选值必须暂存')
            self.assertIn('HTTP_ADDR="127.0.0.1:9601"', stash.read_text())
            # stage 残件已清理
            leftovers = [p.name for p in etc.iterdir() if '.tmp.' in p.name]
            self.assertEqual(leftovers, [], f'残留临时文件 {leftovers}')
            # 下次启动重试:按暂存端口完整落盘
            (work / 'functions').write_text(self._definitions(self.REAL.read_text()))
            clean_env = {k: v for k, v in env.items() if k != 'PATH'}
            clean_env['PATH'] = '/bin:/usr/bin'
            clean_env['TEST_DIR'] = self._shpath(tmp)
            body = (
                '. "$TEST_DIR/functions"\n'
                'CFG_FILE="$TEST_DIR/etc/tad-module.cfg"\n'
                'STARTUP_LOG="$TEST_DIR/var/startup-debug.log"\n'
                'ensure_cfg\n'
                'grep -Fqx \'HTTP_ADDR="127.0.0.1:9601"\' "$CFG_FILE" || exit 109\n'
                'test ! -e "$TEST_DIR/var/wizard-install.env" || exit 110\n'
                'exit 0\n'
            )
            subprocess.run(['sh', '-ec', body], env=clean_env, check=True, timeout=15)

    def test_publish_targets_dir_and_symlink_dir_both_lifecycles(self):
        # 发布走后端 publish 子命令(ln 回退已删),linux 二进制是前提:
        # 非 linux 宿主无法执行目标平台二进制,整测跳过(CI Linux 实跑)
        if sys.platform != 'linux':
            self.skipTest('需要 linux 宿主(publish 子命令),CI 覆盖')
        # CodeRabbit R10:三处发布统一 publish_no_replace 后,两条生命周期
        # (ensure_cfg 启动恢复 / postinst 安装暂存)对「目录占位」与
        # 「symlink-to-dir 占位」都必须——发布失败、无嵌套条目残留、暂存
        # 不丢;普通文件竞争仍失败(既有用例覆盖)。
        # luodaoyi R13:helper 与断言必须在每个 subTest 的 with 块内实际
        # 执行;CI 上构建失败要明确失败,不得静默跳过。
        for lifecycle in ('ensure-cfg', 'postinst'):
            for occupier in ('dir', 'symlink-dir'):
                with self.subTest(lifecycle=lifecycle, occupier=occupier), \
                     tempfile.TemporaryDirectory() as tmp:
                    work = Path(tmp)
                    if not self._stage_publish_binary(work):
                        self.fail('发布二进制构建失败(ln 回退已删除,无 helper 无从验证)')
                    etc = work / 'etc'
                    var = work / 'var'
                    etc.mkdir()
                    var.mkdir()
                    (work / 'functions').write_text(
                        self._definitions(self.REAL.read_text()))
                    # 暂存路径不预建(luodaoyi R11):postinst 生命周期必须证明
                    # 安装脚本能把本次向导值发布到原本不存在的暂存路径;
                    # ensure-cfg 生命周期则预建暂存(验证恢复语义)。
                    if lifecycle == 'ensure-cfg':
                        (var / 'wizard-install.env').write_text('HTTP_ADDR="127.0.0.1:9801"\n')
                    env = dict(os.environ, TEST_DIR=self._shpath(tmp),
                               SYNOPKG_PKGDEST=self._shpath(tmp),
                               SYNOPKG_PKGETC=self._shpath(etc),
                               SYNOPKG_PKGVAR=self._shpath(var),
                               wizard_port='9801')
                    stub_env = self._postinst_env(work)
                    if lifecycle == 'ensure-cfg':
                        body = (
                            '. "$TEST_DIR/functions"\n'
                            'CFG_FILE="$TEST_DIR/etc/tad-module.cfg"\n'
                            'STARTUP_LOG="$TEST_DIR/var/startup-debug.log"\n'
                            'ensure_cfg\n'
                            'test ! -f "$CFG_FILE" || exit 114\n'
                            'test -f "$TEST_DIR/var/wizard-install.env" || exit 115\n'
                            'exit 0\n'
                        )
                        # 占位在 ensure_cfg 运行途中创建:grep 替身钩子在
                        # stage 写完、发布未执行时竞争占位
                        real_grep = shutil.which('grep')
                        qg = "'" + str(Path(real_grep).resolve()).replace(chr(92), '/') + "'"
                        if occupier == 'dir':
                            race = 'mkdir -p "$TEST_DIR/etc/tad-module.cfg"'
                        else:
                            race = ('mkdir -p "$TEST_DIR/etc/real.d"; '
                                    # 防重入:ensure_cfg 有两次校验 grep 调用,
                                    # 第二次钩子若重复 ln -s 会把已存在的
                                    # symlink 移入 real.d 产生 real.d/real.d
                                    # ——污染的是竞争夹具而非发布原语
                                    '[ -e "$TEST_DIR/etc/tad-module.cfg" ] || '
                                    'ln -s "$TEST_DIR/etc/real.d" "$TEST_DIR/etc/tad-module.cfg"')
                        stub_body = ('case "$3" in *tad-module.cfg.tmp*) ' + race + ' ;; esac' + chr(10) +
                                     f'exec {qg} "$@"')
                        self._inject_postinst_stub(work, 'grep', stub_body)
                        ensure_env = {**env, **stub_env}
                        ensure_env['TEST_DIR'] = env['TEST_DIR']
                        subprocess.run(['sh', '-ec', body], env=ensure_env,
                                       check=True, timeout=15)
                        # 无嵌套条目:占位目录内不得有误入的 stage 链接
                        if occupier == 'dir':
                            nested = list((etc / 'tad-module.cfg').iterdir())
                        else:
                            nested = list((etc / 'real.d').iterdir())
                        self.assertEqual(nested, [],
                                         f'{lifecycle}/{occupier}: 嵌套条目 {nested}')
                        # 暂存不消费(发布失败,重试语义)
                        self.assertTrue((var / 'wizard-install.env').is_file(),
                                        f'{lifecycle}/{occupier}: 暂存必须保留')
                    else:
                        # postinst 生命周期:占位先建好,postinst 内部 publish
                        # 时 link(2) 必失败;用 PATH 内真身(无替身)验证原语本身
                        if occupier == 'dir':
                            (etc / 'tad-module.cfg').mkdir()
                        else:
                            (etc / 'real.d').mkdir()
                            try:
                                (etc / 'tad-module.cfg').symlink_to(etc / 'real.d',
                                                                    target_is_directory=True)
                            except OSError:
                                # Windows 无 symlink 特权:CI Linux 覆盖本场景
                                self.skipTest('symlink 需要特权(Windows 限制)')
                        run_env = {**env, **stub_env}
                        subprocess.run(['sh', str(self.POSTINST)], env=run_env,
                                       check=True, timeout=30, capture_output=True)
                        # 无半份正式 cfg(目录/symlink 占位不被覆盖成文件)
                        self.assertFalse((etc / 'tad-module.cfg').is_file(),
                                         f'{lifecycle}/{occupier}: 占位不得被覆盖')
                        # 无嵌套条目
                        if occupier == 'dir':
                            nested = list((etc / 'tad-module.cfg').iterdir())
                        else:
                            nested = list((etc / 'real.d').iterdir())
                        self.assertEqual(nested, [],
                                         f'{lifecycle}/{occupier}: 嵌套条目 {nested}')
                        # 暂存原子提交成功(publish_no_replace 对全新暂存目标)
                        stash = var / 'wizard-install.env'
                        self.assertTrue(stash.is_file(),
                                        f'{lifecycle}/{occupier}: 暂存必须存在')
                        self.assertIn('HTTP_ADDR="127.0.0.1:9801"', stash.read_text())

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
