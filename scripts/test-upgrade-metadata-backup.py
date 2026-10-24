#!/usr/bin/env python3
"""Offline regression for the metadata-backup gate only, never the installer.

Run: python3 scripts/test-upgrade-metadata-backup.py [script-under-test]
The exact source block is run with a fake cp and exclusively temporary paths.
No service, installation, device, network or real upgrade operation is invoked.
"""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(sys.argv.pop(1)) if len(sys.argv) > 1 else Path(__file__).with_name('tad-upgrade-manual.sh')
text = SOURCE.read_text(encoding='utf-8')
start = text.index('METABAK_TMP="$APPMETA.bak-meta-')
end = text.index('cp -a "$STAGE/manifest"', start)
BLOCK = text[start:end]

# Each failure leaves all existence checks satisfied, reproducing partial-copy
# failures rather than only the easier missing-source-file case.
FAKE_CP = '''#!/bin/sh
for dest; do :; done
case "$*" in
  *"/cmd "*)
    if [ "$CASE" = cmd_partial ]; then
      mkdir -p "$dest/cmd" "$dest/wizard"
      printf truncated > "$dest/cmd/main"
      exit 1
    fi
    exec "$REAL_CP" "$@" ;;
  *)
    if [ "$CASE" = metadata_partial ]; then
      : > "$dest/manifest"
      : > "$dest/ICON.PNG"
      : > "$dest/ICON_256.PNG"
      exit 1
    fi
    exec "$REAL_CP" "$@" ;;
esac
'''

class BackupGateTests(unittest.TestCase):
    def run_case(self, case, succeeds):
        with tempfile.TemporaryDirectory(prefix='upgrade-metadata-test-') as tmp:
            root = Path(tmp)
            app = root / 'appmeta'
            fakebin = root / 'bin'
            fakebin.mkdir()
            original = {
                'cmd/main': b'original service script\n',
                'cmd/extra': b'original helper\n',
                'wizard/keep': b'original wizard\n',
                'manifest': b'version=0.10.78\n',
                'ICON.PNG': b'original icon',
                'ICON_256.PNG': b'original large icon',
            }
            for name, data in original.items():
                dest = app / name
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(data)
            fake = fakebin / 'cp'
            fake.write_text(FAKE_CP)
            fake.chmod(0o755)
            env = dict(os.environ, PATH=str(fakebin) + ':/usr/bin:/bin',
                       REAL_CP='/bin/cp', CASE=case, APPMETA=str(app),
                       ACCEPTED=str(root / 'accepted'))
            # Marker represents crossing the gate, not actual replacement.
            run = subprocess.run(['/bin/sh', '-c', 'set -eu\n' + BLOCK +
                                  '\nprintf "%s" "$METABAK" > "$ACCEPTED"\n'],
                                 env=env, text=True, capture_output=True, timeout=10)
            self.assertEqual(run.returncode == 0, succeeds, run.stdout + run.stderr)
            self.assertEqual((root / 'accepted').exists(), succeeds)
            for name, data in original.items():
                self.assertEqual((app / name).read_bytes(), data, name)
            backups = list(root.glob('appmeta.bak-meta-*'))
            if succeeds:
                self.assertEqual(len(backups), 1)
                for name, data in original.items():
                    self.assertEqual((backups[0] / name).read_bytes(), data, name)
            else:
                self.assertEqual(backups, [])
                self.assertIn('已中止升级', run.stderr)
                self.assertNotIn('已回滚', run.stdout + run.stderr)

    def test_cmd_partial_failure(self):
        self.run_case('cmd_partial', False)

    def test_metadata_partial_failure(self):
        self.run_case('metadata_partial', False)

    def test_complete_success(self):
        self.run_case('success', True)

if __name__ == '__main__':
    unittest.main()
