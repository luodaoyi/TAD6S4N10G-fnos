'use strict';

// 一次性校验脚本（不进包、不进测试套件）：用 jsdom 加载真实的 index.html，
// 再把 app.js 里真实的 backupKeepAnchorID / syncBackupKeepRow 源码跑在这个 DOM 上，
// 确认「归档保留份数」那一行确实会落到选中的归档档位正下方、其它档位收起。

const fs = require('node:fs');
const path = require('node:path');
const { JSDOM } = require('jsdom');

const root = path.resolve(__dirname, '..');
const html = fs.readFileSync(path.join(root, 'app/ui/static/index.html'), 'utf8');
const appJs = fs.readFileSync(path.join(root, 'app/ui/static/app.js'), 'utf8');

function grab(name) {
  const start = appJs.indexOf(`function ${name}(`);
  if (start < 0) throw new Error(`app.js 里找不到 ${name}`);
  // 从函数头开始做花括号配平，取出整段源码
  let depth = 0;
  let i = appJs.indexOf('{', start);
  for (let j = i; j < appJs.length; j += 1) {
    if (appJs[j] === '{') depth += 1;
    else if (appJs[j] === '}') {
      depth -= 1;
      if (depth === 0) return appJs.slice(start, j + 1);
    }
  }
  throw new Error(`${name} 的函数体没有闭合`);
}

const dom = new JSDOM(html, { runScripts: 'outside-only' });
const { window } = dom;
window.$ = (id) => window.document.getElementById(id);
window.eval(grab('backupKeepAnchorID'));
window.eval(grab('syncBackupKeepRow'));

const { document } = window;
let failures = 0;
function check(label, actual, expected) {
  const ok = actual === expected;
  if (!ok) failures += 1;
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${label}${ok ? '' : `  实际=${actual} 期望=${expected}`}`);
}

// 该行当前是不是紧跟在某个档位后面（同一个父节点里，档位的下一个兄弟就是它）
function follows(row, anchor) {
  return anchor.parentElement === row.parentElement && anchor.nextElementSibling === row;
}

function setMode(name, value) {
  document.querySelectorAll(`input[name="${name}"]`).forEach((radio) => {
    radio.checked = radio.value === value;
  });
}

for (const [label, prefix] of [['备份任务', 'backup-job'], ['自动备份', 'auto-backup']]) {
  const pick = prefix === 'backup-job' ? 'backup-mode' : 'auto-backup-mode';
  const rowID = `${prefix}-keep-row`;
  const row = document.getElementById(rowID);
  const sync = window.syncBackupKeepRow;
  const anchorOf = window.backupKeepAnchorID;

  check(`${label}：初始就是收起的`, row.hasAttribute('hidden'), true);

  setMode(pick, 'archive');
  sync(rowID, anchorOf('archive', prefix));
  const archive = document.getElementById(`${prefix}-mode-archive`);
  check(`${label}：选增量归档 → 行显示`, row.hasAttribute('hidden'), false);
  check(`${label}：贴在「按日期增量归档」正下方`, follows(row, archive), true);

  setMode(pick, 'move_archive');
  sync(rowID, anchorOf('move_archive', prefix));
  const moveArchive = document.getElementById(`${prefix}-mode-move-archive`);
  check(`${label}：选转移归档 → 行仍显示`, row.hasAttribute('hidden'), false);
  check(`${label}：贴到「按日期转移归档」正下方`, follows(row, moveArchive), true);
  check(`${label}：不再挂在增量归档下面`, follows(row, archive), false);

  // 再切回增量归档：应该能挪回去，而不是赖着不动
  setMode(pick, 'archive');
  sync(rowID, anchorOf('archive', prefix));
  check(`${label}：切回增量归档会挪回去`, follows(row, archive), true);
  check(`${label}：切换过程中没被复制出第二份`, document.querySelectorAll(`#${rowID}`).length, 1);
  check(`${label}：仍然只属于那个 fieldset`, row.closest('fieldset').querySelector('legend').textContent, '复制方式');

  setMode(pick, 'sync');
  sync(rowID, anchorOf('sync', prefix));
  check(`${label}：选同步文件 → 整行收起`, row.hasAttribute('hidden'), true);

  setMode(pick, 'copy');
  sync(rowID, anchorOf('copy', prefix));
  check(`${label}：选直接复制覆盖 → 仍收起`, row.hasAttribute('hidden'), true);

  setMode(pick, 'move_overwrite');
  sync(rowID, anchorOf('move_overwrite', prefix));
  check(`${label}：选移动覆盖 → 仍收起`, row.hasAttribute('hidden'), true);
}

// 「源介质标识」整组排在开关下方（DOM 顺序）
const markerField = document.getElementById('backup-job-marker').closest('.field');
const order = Array.from(markerField.children).map((node) => {
  const inner = node.firstElementChild && node.firstElementChild.id ? `#${node.firstElementChild.id}` : '';
  return node.tagName + (node.id ? `#${node.id}` : '') + inner;
});
check('标识那一组：开关 → 标签 → 输入框', order.join(' | '), 'LABEL#backup-job-marker-write | SPAN | INPUT#backup-job-marker');
check('开关里的 checkbox 是同一个 id', document.querySelector('#backup-job-marker-write').type, 'checkbox');
check('保留份数那一行与输入框同一行（样式表里是 flex）', /\.backup-mode-keep \{[\s\S]*?display: flex;/.test(fs.readFileSync(path.join(root, 'app/ui/static/styles.css'), 'utf8')), true);

console.log(failures === 0 ? '\n全部通过' : `\n${failures} 项不通过`);
process.exit(failures === 0 ? 0 : 1);
