const $ = (id) => document.getElementById(id);
// 页面错误采集：白屏等运行期问题在真机无控制台可看，捕获后写入
// localStorage（跨刷新保留最近 20 条），生成调试报告时带出并清空。
const PAGE_ERROR_STORE_KEY = 'tad-page-errors';
try {
  const recordPageError = (detail) => {
    try {
      const entry = `${new Date().toISOString()} ${String(detail).slice(0, 280)}`;
      const stored = JSON.parse(window.localStorage.getItem(PAGE_ERROR_STORE_KEY) || '[]');
      stored.push(entry);
      window.localStorage.setItem(PAGE_ERROR_STORE_KEY, JSON.stringify(stored.slice(-20)));
    } catch (storageError) { /* localStorage 不可用时丢弃 */ }
  };
  window.addEventListener('error', (event) => {
    recordPageError(`${event.message} @${String(event.filename || '').split('/').pop()}:${event.lineno}`);
  });
  window.addEventListener('unhandledrejection', (event) => {
    const reason = event.reason && event.reason.message ? event.reason.message : event.reason;
    recordPageError(`rejection: ${reason}`);
  });
} catch (collectorError) { /* 采集器自身失败不影响页面 */ }
// 默认曲线：第一档就等于默认最低转速（40%），0.10.37 起由 60% 下调
const DEFAULT_CPU_CURVE = [
  { temp_c: 40, pwm_percent: 40 },
  { temp_c: 55, pwm_percent: 70 },
  { temp_c: 70, pwm_percent: 85 },
  { temp_c: 80, pwm_percent: 100 },
];
const DEFAULT_STORAGE_CURVE = [
  { temp_c: 25, pwm_percent: 40 },
  { temp_c: 35, pwm_percent: 85 },
  { temp_c: 50, pwm_percent: 100 },
];
const DEFAULT_MIN_PWM_PERCENT = 40;
const GPIO_ACTIONS = [
  ['none', '无动作'],
  ['log', '仅记录日志'],
  ['refresh_storage', '刷新硬盘仓位'],
  ['smart_check', '刷新仓位并检查 SMART'],
  ['reapply_plugin', '重新应用插件配置'],
  ['refresh_sdcard', '刷新SD卡（重新识别USB读卡器）'],
];
const GITHUB_LATEST_RELEASE_API = 'https://api.github.com/repos/luodaoyi/TAD6S4N10G-fnos/releases/latest';
const GPIO_SCRIPT_MAX_COUNT = 32;
const GPIO_SCRIPT_MAX_BODY_BYTES = 65536;
const STORAGE_STATE_LABELS = {
  empty: '空置', present: '已插入', used: '已使用', warning: '告警', unknown: '未知',
};
const STORAGE_ACTIVITY_LABELS = {
  idle: '空闲', light: '轻载', medium: '中载', heavy: '高载', busy: '繁忙', full: '满载',
  sleeping: '休眠', unknown: '未知',
};
const STORAGE_HOT_C = { front: 55, m2: 70 };
const CURVE_MIN_POINTS = 2;
const CURVE_MAX_POINTS = 8;
const CHART = { left: 48, right: 600, top: 28, bottom: 218 };
// 曲线编辑器标签顺序 = 标签栏顺序 = HDD/SATA → NVMe → CPU；默认停在第一张
const CURVE_KINDS = ['hdd', 'nvme', 'cpu'];
const FAN_SLOT_GROUPS = {
  hdd: {
    containerID: 'hdd-slot-options', configField: 'hdd_slot_ids',
    slots: [6, 5, 4, 3, 2, 1].map((slot) => ({ id: `front-${slot}`, label: `SATA ${slot}` })),
  },
  nvme: {
    containerID: 'nvme-slot-options', configField: 'nvme_slot_ids',
    slots: [1, 2, 3, 4].map((slot) => ({ id: `m2-${slot}`, label: `NVMe ${slot}` })),
  },
};
const FAN_CURVE_GROUPS = {
  cpu: { containerID: 'cpu-fan-options', configField: 'cpu_fan_ids' },
  hdd: { containerID: 'hdd-fan-options', configField: 'hdd_fan_ids' },
  nvme: { containerID: 'nvme-fan-options', configField: 'nvme_fan_ids' },
};
const STORAGE_VISUAL_GROUPS = [
  {
    kind: 'front', base: 'images/panel/6SATA面板底图.svg', ids: [6, 5, 4, 3, 2, 1].map((slot) => `front-${slot}`),
    assets: {
      gray: 'images/panel/SATA灰-休眠未使用.svg',
      blue: 'images/panel/SATA蓝-休眠.svg',
      red: 'images/panel/SATA红-报警.svg',
      green: 'images/panel/SATA绿-正常健康.svg',
      empty: 'images/panel/SATA空置.svg',
    },
  },
  {
    kind: 'm2', base: 'images/panel/4M2面板底图.svg', ids: ['m2-4', 'm2-2', 'm2-3', 'm2-1'], flip: new Set(['m2-2', 'm2-1']),
    assets: {
      gray: 'images/panel/M2灰-休眠未使用.svg',
      blue: 'images/panel/M2蓝-休眠.svg',
      red: 'images/panel/M2红-报警.svg',
      green: 'images/panel/M2绿-正常健康.svg',
      empty: 'images/panel/M2空置.svg',
    },
  },
];
const curveEditors = {
  cpu: {
    chartID: 'fan-curve-chart', addID: 'curve-add', removeID: 'curve-remove', selectedID: 'curve-selected',
    curve: DEFAULT_CPU_CURVE.map((point) => ({ ...point })), selectedIndex: 0, draggedIndex: -1, color: '#3f6ff5',
  },
  hdd: {
    chartID: 'disk-curve-chart', addID: 'disk-curve-add', removeID: 'disk-curve-remove', selectedID: 'disk-curve-selected',
    curve: DEFAULT_STORAGE_CURVE.map((point) => ({ ...point })), selectedIndex: 0, draggedIndex: -1, color: '#18a779',
  },
  nvme: {
    chartID: 'nvme-curve-chart', addID: 'nvme-curve-add', removeID: 'nvme-curve-remove', selectedID: 'nvme-curve-selected',
    curve: DEFAULT_STORAGE_CURVE.map((point) => ({ ...point })), selectedIndex: 0, draggedIndex: -1, color: '#a25bd7',
  },
};
let currentStatus = null;
let uiBusy = false;
let gpioScripts = [];
let gpioEditingScriptID = '';
// 备份任务（backup.go）：图形化配置源/目标/复制方式，动作 backup:<id> 引用。
let gpioBackups = [];
// 自动备份规则（backup_auto.go）：按键动作 auto_backup:<id>，触发时扫描打标卷。
let gpioAutos = [];
// 打标列表（GET /api/backup/auto 的 devices）：在线存储设备 + 已打标但离线的记录。
let autoDevices = [];
// 打标文件在卷内的相对路径（后端下发，只用于提示文案）。
let autoMarkerDirFile = '.tad-backup/mark.json';
// 后端记录的一次性默认值补位标记。保存时要原样带回，否则用户手动清空过的
// 档位会在下次升级时被重新填上默认动作。
let gpioMigrations = [];
let backupEditingJobID = '';
// 关掉标识开关时暂存原来的内容：再打开时沿用，不重新生成随机值——否则同一块盘上
// 被别的任务引用的标识会悄悄变掉，那些任务下次就会因为「对不上」被拒绝。
let backupMarkerStash = '';
let autoEditingRuleID = '';
// 「NAS → 打标卷 + 移动类」的确认项是否处于「还没确认就想保存」的告警状态：
// 只有真的拦过一次才高亮，用户勾上或换到别的档位就复位。
let autoMoveConfirmAlert = false;
let backupPollTimer = null;
// 编辑中的源/目标位置（完整结构，输入框里只显示摘要文本）。
const backupEditorLocations = { source: null, target: null };
// 自动备份编辑器里的 NAS 侧固定位置。
const autoEditorLocations = { fixed: null };
// 目录浏览对话框状态；volume 选择状态。
const pathPicker = {
  role: 'source',
  path: '',
  scope: 'places',
  allowFile: true,
  selectedFile: '',
  places: [],
  currentPlace: null,
};
const volumePicker = { role: 'source', selected: null, volumes: [], loading: false };
let storageVisualReady = false;
let fanSelectorSignature = null;
let debugReportText = '';
let serialFormDirty = false; // 串口传感器表单有未保存的修改：轮询渲染不得覆盖

function baseUrl(path) {
  const base = window.location.pathname.endsWith('/') ? window.location.pathname : `${window.location.pathname}/`;
  return new URL(path, `${window.location.origin}${base}`).toString();
}

async function request(path, options = {}) {
  const response = await fetch(baseUrl(path), {
    cache: 'no-store',
    headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    ...options,
  });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error((body && body.error) || `HTTP ${response.status}`);
  // 2xx 但响应体不是合法 JSON（截断/网关改写等）：结果不确定，不得当成成功
  if (body === null) throw new Error(`响应体解析失败（HTTP ${response.status}）`);
  return body;
}

function parseSemanticVersion(value) {
  const match = String(value || '').trim().match(/^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/i);
  return match ? match.slice(1, 4).map(Number) : null;
}

function compareSemanticVersions(left, right) {
  for (let index = 0; index < 3; index += 1) {
    if (left[index] !== right[index]) return left[index] > right[index] ? 1 : -1;
  }
  return 0;
}

async function checkForUpdates() {
  const button = $('check-updates');
  const status = $('update-status');
  const releaseLink = $('update-release-link');
  const currentVersion = parseSemanticVersion(currentStatus?.version);
  releaseLink.hidden = true;
  status.textContent = '正在检查…';
  status.className = 'update-status';
  button.disabled = true;
  try {
    if (!currentVersion) throw new Error('当前运行版本无法比较');
    const response = await fetch(GITHUB_LATEST_RELEASE_API, {
      cache: 'no-store',
      headers: { Accept: 'application/vnd.github+json' },
    });
    if (!response.ok) throw new Error(`GitHub 返回 HTTP ${response.status}`);
    const release = await response.json();
    const latestVersion = parseSemanticVersion(release.tag_name);
    if (!latestVersion) throw new Error('最新 Release 版本格式无法识别');
    if (compareSemanticVersions(latestVersion, currentVersion) > 0) {
      const releaseURL = new URL(release.html_url);
      if (releaseURL.protocol !== 'https:' || releaseURL.hostname !== 'github.com') {
        throw new Error('Release 地址不安全');
      }
      status.textContent = '';
      releaseLink.href = releaseURL.toString();
      releaseLink.textContent = `有新版本 v${latestVersion.join('.')}，前去更新`;
      releaseLink.hidden = false;
    } else {
      status.textContent = `当前 v${currentVersion.join('.')} 已是最新版本`;
      status.className = 'update-status ok';
    }
  } catch (error) {
    status.textContent = `检查失败：${error.message}`;
    status.className = 'update-status error';
  } finally {
    button.disabled = false;
  }
}

function formatTemperature(value, available = true) {
  const number = Number(value);
  return available && Number.isFinite(number) ? `${number.toFixed(1)} °C` : '不可用';
}

function formatUtilization(value) {
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? `${Math.round(number)}%` : '';
}

function isStorageHot(slot = {}) {
  const temperature = Number(slot.temperature_c);
  if (!Number.isFinite(temperature) || temperature <= 0) return false;
  const threshold = STORAGE_HOT_C[slot.kind] || STORAGE_HOT_C.front;
  return temperature >= threshold;
}

function storageChip(text, tone = 'muted', options = {}) {
  const chip = document.createElement(options.detail ? 'button' : 'span');
  if (options.detail) chip.type = 'button';
  chip.className = `storage-chip tone-${tone}${options.detail ? ' is-action' : ''}`;
  chip.textContent = text;
  if (options.detail) {
    chip.setAttribute('aria-haspopup', 'dialog');
    chip.title = '点击查看详情';
    chip.addEventListener('click', (event) => {
      event.preventDefault();
      event.stopPropagation();
      openAppModal(options.title || '详细信息', options.detail);
    });
  }
  return chip;
}

function storageHealthLabel(slot = {}) {
  // 健康状态行优先级：高温 > SMART / RM / purpose 等告警 > 后端 health 字段
  if (isStorageHot(slot)) return '高温';
  if (slot.warning || slot.state === 'warning') return '告警';
  // 后端没给健康结果（如 smartctl 缺失）时如实显示未读取，不能推断成正常
  return slot.health || '未读取';
}

function storageHealthDetail(slot = {}) {
  return String(slot.warning || '').trim();
}

function openAppModal(title, body) {
  const modal = $('app-modal');
  if (!modal) return;
  $('app-modal-title').textContent = title;
  $('app-modal-body').textContent = body;
  modal.hidden = false;
  document.body.classList.add('modal-open');
  modal.dataset.lastFocus = '';
  openAppModal.lastFocus = document.activeElement;
  $('app-modal-close')?.focus();
}

function closeAppModal() {
  const modal = $('app-modal');
  if (!modal || modal.hidden) return;
  modal.hidden = true;
  document.body.classList.remove('modal-open');
  const lastFocus = openAppModal.lastFocus;
  if (lastFocus && typeof lastFocus.focus === 'function') lastFocus.focus();
}

function setupAppModal() {
  const modal = $('app-modal');
  if (!modal) return;
  modal.addEventListener('click', (event) => {
    if (event.target.closest('[data-modal-close]')) closeAppModal();
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') closeAppModal();
  });
}

function storageActivityTone(activity) {
  if (activity === 'sleeping') return 'sleep';
  if (activity === 'idle') return 'idle';
  return 'muted';
}

function storageHealthTone(slot = {}) {
  if (slot.state === 'warning' || slot.warning) return 'danger';
  if (slot.health && slot.health !== '—' && slot.health !== '未读取') return 'ok';
  return 'muted';
}

function storageTempTone(slot = {}) {
  return isStorageHot(slot) ? 'danger' : 'temp';
}

function storageSlotStatusLabel(slot = {}, tone = '') {
  // 活动行规则：
  // - empty → 空置
  // - present → 未使用（已插未挂，确实没负载）
  // - used → 活动词（空闲/休眠/轻载/繁忙等真实负载）
  // - warning/hot → 告警不等于没在干活：后端先记录真实活动再叠加告警，
  //   有活动读数时透传真实活动词，读数缺失才显示「未读取」
  // - unknown → 未读取（状态未知不能推导出未使用）
  if (tone === 'empty') return '空置';
  if (slot.state === 'used') {
    return STORAGE_ACTIVITY_LABELS[slot.activity] || '';
  }
  if (slot.state === 'present') return '未使用';
  return STORAGE_ACTIVITY_LABELS[slot.activity] || '未读取';
}

function connectedFans(fanStatus = {}, limit = 0) {
  // 有转速，或已勾选（瞬时 0 转停转/故障保护仍可见）；未接线且持续 0 RPM 的通道不列出。
  const fans = (fanStatus.fans || []).filter((fan) => Number(fan.rpm) > 0 || fan.selected === true);
  return limit > 0 ? fans.slice(0, limit) : fans;
}

// 选择器签名只由可见风扇集合决定、与转速无关：已勾选风扇瞬时 0 转
// 不会触发列表重建，未保存的勾选因此不会丢。
function fanListSignature(fans = []) {
  return fans.map((fan) => fan.id).join('|');
}

function fanLabel(fan, fans = []) {
  return fans.length === 1 ? 'FAN' : `FAN${fan.channel}`;
}

function renderHealthBadge(label, tone, detail) {
  const badgeClass = `badge ${tone}`;
  ['health', 'about-health'].forEach((id) => {
    const badge = $(id);
    if (!badge) return;
    badge.textContent = label;
    badge.className = badgeClass;
  });
  if ($('health-tooltip')) $('health-tooltip').textContent = detail;
  if ($('about-health-detail')) $('about-health-detail').textContent = detail;
}

function formatSize(bytes) {
  let value = Number(bytes);
  if (!Number.isFinite(value) || value <= 0) return '';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit < 3 ? 0 : 1)} ${units[unit]}`;
}

function sortedStorageSlots(storage = {}) {
  return [...(storage.slots || [])].sort((left, right) => {
    if (left.kind !== right.kind) return left.kind === 'front' ? -1 : 1;
    return left.slot - right.slot;
  });
}

function renderStorageTable(storage = {}) {
  const slots = sortedStorageSlots(storage);
  const body = $('storage-body');
  body.replaceChildren();
  slots.forEach((slot) => {
    const row = document.createElement('tr');
    const empty = slot.state === 'empty';
    row.className = `storage-${slot.state || 'unknown'}${isStorageHot(slot) ? ' storage-hot' : ''}`;
    const label = slot.kind === 'front' ? `前置 ${slot.slot}` : `M.2 ${slot.slot}`;
    const stateLabel = STORAGE_STATE_LABELS[slot.state] || slot.state || '未知';
    const activityLabel = empty ? '—' : (STORAGE_ACTIVITY_LABELS[slot.activity] || slot.activity || '未知');
    const purposeLabel = slot.purpose || (empty ? '空仓位' : '—');
    const healthLabel = storageHealthLabel(slot);
    const healthDetail = storageHealthDetail(slot);
    const temperature = formatTemperature(slot.temperature_c, Number(slot.temperature_c) > 0);
    const model = slot.model || '—';
    const meta = [slot.device, formatSize(slot.size_bytes), slot.serial].filter(Boolean).join(' · ') || '—';
    const extra = [activityLabel, purposeLabel].filter((value) => value && value !== '—').join(' · ') || '—';
    const utilization = formatUtilization(slot.utilization_percent);
    const cells = [
      ['th', '仓位', 'storage-col-slot', label],
      ['td', '状态', 'storage-col-state', stateLabel],
      ['td', '活动', 'storage-col-activity', activityLabel],
      ['td', '繁忙度', 'storage-col-util', utilization || '—'],
      ['td', '设备', 'storage-col-device', null],
      ['td', '用途', 'storage-col-purpose', purposeLabel],
      ['td', '健康', 'storage-col-health', healthLabel],
      ['td', '温度', 'storage-col-temp', temperature],
    ];
    cells.forEach(([tag, name, className, value]) => {
      const cell = document.createElement(tag);
      cell.dataset.label = name;
      cell.className = className;
      if (className === 'storage-col-device') {
        if (empty) {
          cell.textContent = '—';
        } else {
          const wrap = document.createElement('div');
          wrap.className = 'storage-device';
          const title = document.createElement('span');
          title.className = 'storage-model';
          title.textContent = model;
          const detail = document.createElement('small');
          detail.className = 'storage-device-meta';
          detail.textContent = meta;
          const more = document.createElement('small');
          more.className = 'storage-device-extra';
          more.textContent = extra;
          wrap.append(title, detail, more);
          cell.append(wrap);
        }
      } else if (!empty && className === 'storage-col-activity') {
        cell.append(storageChip(activityLabel, storageActivityTone(slot.activity)));
      } else if (!empty && className === 'storage-col-util') {
        cell.append(storageChip(utilization || '—', 'muted'));
      } else if (!empty && className === 'storage-col-health') {
        cell.append(storageChip(healthLabel, storageHealthTone(slot), healthDetail ? {
          title: `${label} 健康详情`,
          detail: healthDetail,
        } : {}));
      } else if (!empty && className === 'storage-col-temp') {
        cell.append(storageChip(temperature, storageTempTone(slot)));
      } else {
        cell.textContent = value;
      }
      row.append(cell);
    });
    const cardHead = document.createElement('td');
    cardHead.className = 'storage-card-head';
    cardHead.dataset.label = '仓位';
    cardHead.textContent = `${label} · ${stateLabel}`;
    row.append(cardHead);
    body.append(row);
  });
  if (!slots.length) {
    const row = document.createElement('tr');
    const cell = document.createElement('td');
    cell.colSpan = 8;
    cell.dataset.label = '状态';
    cell.textContent = '尚未获得仓位信息';
    row.append(cell);
    body.append(row);
  }
  $('storage-updated').textContent = storage.updated_at
    ? `更新于 ${new Date(storage.updated_at).toLocaleTimeString()}` : '等待刷新';
  $('storage-error').textContent = storage.last_error || '';
  $('storage-error').className = `inline-status${storage.last_error ? ' error' : ''}`;
}

function initializeStorageVisual() {
  if (storageVisualReady) return;
  const target = $('storage-visual');
  STORAGE_VISUAL_GROUPS.forEach((group) => {
    const figure = document.createElement('figure');
    figure.className = `storage-visual-group storage-visual-${group.kind}`;
    const frame = document.createElement('div');
    frame.className = 'storage-visual-frame';
    const base = document.createElement('img');
    base.className = 'storage-visual-base';
    base.src = baseUrl(group.base);
    base.alt = group.kind === 'front' ? '六盘位机箱示意图' : '四个 M.2 槽位示意图';
    frame.append(base);
    if (group.kind === 'm2') {
      const caption = document.createElement('div');
      caption.className = 'storage-visual-caption';
      caption.textContent = '内置 M.2 插槽';
      frame.append(caption);
    }
    group.ids.forEach((id) => {
      const marker = document.createElement('div');
      marker.className = 'storage-visual-slot is-empty';
      marker.dataset.slot = id;
      const overlay = document.createElement('img');
      overlay.className = `storage-slot-overlay${group.flip?.has(id) ? ' flipped' : ''}`;
      overlay.alt = '';
      overlay.hidden = true;
      const text = document.createElement('div');
      text.className = 'storage-slot-text';
      const name = document.createElement('b');
      const type = document.createElement('em');
      type.className = 'slot-type';
      type.hidden = true;
      const status = document.createElement('small');
      const health = document.createElement('small');
      health.className = 'health-label';
      health.hidden = true;
      const temperature = document.createElement('span');
      text.append(name, type, status, health, temperature);
      marker.append(overlay, text);
      frame.append(marker);
    });
    figure.append(frame);
    target.append(figure);
    if (group.kind === 'm2') {
      // 「移动设备」区块挂在 M.2 面板正下方：SD 卡 / U 盘不在机箱仓位里，
      // 但用户的心理位置就是内置 M.2 插槽旁边那一块。
      const mobile = $('storage-mobile');
      if (mobile) figure.append(mobile);
    }
    frame.addEventListener('click', (event) => {
      const marker = event.target.closest('.storage-visual-slot');
      if (!marker?.dataset.detail) return;
      openAppModal(marker.dataset.title || '健康详情', marker.dataset.detail);
    });
    Object.values(group.assets).forEach((path) => { const image = new Image(); image.src = baseUrl(path); });
  });
  storageVisualReady = true;
}

function storageVisualTone(slot = {}) {
  if (slot.state === 'empty') return 'empty';
  if (slot.state === 'warning' || isStorageHot(slot)) return 'red';
  if (slot.activity === 'sleeping') return 'blue';
  if (slot.state === 'present' || slot.state === 'unknown') return 'gray';
  return 'green';
}

// 硬盘槽位状态 demo：方便在不插盘时预览 all 状态的暗色主题效果。
// 触发方式（任选其一，动态生效——设完不用刷新，下一次状态轮询就切换）：
//   1) 模块页面里 F12 打开控制台，执行 localStorage.setItem('storage-demo','1')
//   2) 直接访问模块时 URL 加 ?storage-demo=1
// 关闭：localStorage.removeItem('storage-demo')
function storageDemoSlots() {
  let enabled = false;
  try {
    const q = new URLSearchParams(window.location.search);
    enabled = q.get('storage-demo') === '1' || localStorage.getItem('storage-demo') === '1';
  } catch (e) { /* localStorage 不可用时忽略 */ }
  if (!enabled) return null;
  const map = new Map();
  // 6 SATA：空/健康/休眠/未用/报警/空
  [
    ['front-6',  { kind: 'front', slot: 6,  state: 'empty',   activity: 'unknown', temperature_c: 0 }],
    ['front-5',  { kind: 'front', slot: 5,  state: 'present', activity: 'busy',     temperature_c: 38.5, health_status: 'ok' }],
    ['front-4',  { kind: 'front', slot: 4,  state: 'present', activity: 'sleeping', temperature_c: 28.0, health_status: 'ok' }],
    ['front-3',  { kind: 'front', slot: 3,  state: 'present', activity: 'idle',     temperature_c: 32.0, health_status: 'ok' }],
    ['front-2',  { kind: 'front', slot: 2,  state: 'warning', activity: 'heavy',    temperature_c: 62.0, health_status: 'critical' }],
    ['front-1',  { kind: 'front', slot: 1,  state: 'empty',   activity: 'unknown', temperature_c: 0 }],
    // 4 M.2：健康/报警/休眠/未用
    ['m2-4',     { kind: 'm2',    slot: 4,  state: 'present', activity: 'busy',     temperature_c: 45.0, health_status: 'ok' }],
    ['m2-2',     { kind: 'm2',    slot: 2,  state: 'warning', activity: 'busy',     temperature_c: 78.0, health_status: 'critical' }],
    ['m2-3',     { kind: 'm2',    slot: 3,  state: 'present', activity: 'sleeping', temperature_c: 30.0, health_status: 'ok' }],
    ['m2-1',     { kind: 'm2',    slot: 1,  state: 'present', activity: 'unknown',  temperature_c: 35.0, health_status: 'ok' }],
  ].forEach(([id, slot]) => map.set(id, slot));
  return map;
}

function renderStorageVisual(storage = {}) {
  initializeStorageVisual();
  const demo = storageDemoSlots();
  const slots = demo
    ? demo
    : new Map((storage.slots || []).map((slot) => [slot.id || `${slot.kind}-${slot.slot}`, slot]));
  STORAGE_VISUAL_GROUPS.forEach((group) => {
    group.ids.forEach((id) => {
      const slot = slots.get(id) || { id, kind: group.kind, state: 'unknown', activity: 'unknown' };
      const marker = document.querySelector(`.storage-visual-slot[data-slot="${id}"]`);
      const tone = storageVisualTone(slot);
      const healthDetail = storageHealthDetail(slot);
      marker.className = `storage-visual-slot tone-${tone}${slot.state === 'empty' ? ' is-empty' : ''}${healthDetail ? ' is-action' : ''}`;
      if (healthDetail) {
        const slotLabel = slot.kind === 'front' ? `前置 ${slot.slot}` : `M.2 ${slot.slot}`;
        marker.dataset.detail = healthDetail;
        marker.dataset.title = `${slotLabel} 健康详情`;
      } else {
        delete marker.dataset.detail;
        delete marker.dataset.title;
      }
      const overlay = marker.querySelector('.storage-slot-overlay');
      // 空仓也铺"空置"托盘底图：机箱底图自带的空槽比托盘矮，不铺会让
      // 空仓与在位仓的可见高度不一致
      const source = baseUrl(group.assets[tone] || group.assets.gray);
      if (overlay.dataset.source !== source) {
        overlay.src = source;
        overlay.dataset.source = source;
      }
      overlay.hidden = false;
      const temperature = marker.querySelector('.storage-slot-text span');
      const status = marker.querySelector('.storage-slot-text small:not(.health-label)');
      const health = marker.querySelector('.storage-slot-text .health-label');
      const slotType = marker.querySelector('.storage-slot-text .slot-type');
      marker.querySelector('.storage-slot-text b').textContent = id.split('-').pop();
      // 盘类型行仅前置仓显示（2026-10-23 用户要求 M.2 面板退回旧版三行布局，
      // 不再显示盘类型/健康行，暂时维持不变）：front 槽 → SSD / HDD
      if (slotType) {
        if (group.kind === 'front' && tone !== 'empty') {
          slotType.textContent = (slot.device_type || '').toUpperCase() || 'HDD';
          slotType.hidden = false;
        } else {
          slotType.hidden = true;
        }
      }
      // 健康状态行：显示在活动状态和温度之间。同样仅前置仓显示，M.2 面板维持旧版。
      // 优先级：高温 → SMART/RM 等告警 → 后端 health 字段
      if (health) {
        if (group.kind === 'front' && tone !== 'empty') {
          const healthVal = storageHealthLabel(slot); // 已内含高温/告警/正常优先级
          const isHot = isStorageHot(slot);
          const isWarn = !isHot && (slot.state === 'warning' || Boolean(slot.warning) || healthVal === '告警');
          const isBad = isHot || isWarn;
          health.textContent = healthVal;
          health.classList.toggle('is-warning', isBad);
          health.classList.toggle('is-unknown', !isBad && healthVal === '未读取');
          health.classList.toggle('is-hot', isHot);
          health.hidden = false;
        } else {
          health.hidden = true;
        }
      }
      const hasTemperature = Number(slot.temperature_c) > 0;
      if (tone === 'empty' || !hasTemperature) {
        temperature.textContent = '';
      } else if (group.kind === 'front') {
        temperature.textContent = `${Number(slot.temperature_c).toFixed(1)}\n°C`;
      } else {
        temperature.textContent = formatTemperature(slot.temperature_c);
      }
      if (status) {
        status.textContent = storageSlotStatusLabel(slot, tone);
        // 空仓隐藏状态词只应用于前置仓；M.2 面板维持旧版「空置」占位。
        status.hidden = group.kind === 'front' && tone === 'empty';
      }
    });
  });
}

// 移动设备区块：插了几个就画几个图标（SD 卡 / U 盘），悬停显示设备名称、
// 容量与挂载点。容量 0 的空读卡器后端已经过滤掉，这里只负责铺图标。
function mobileDeviceIcon(kind) {
  const svg = svgElement('svg', {
    viewBox: '0 0 24 24',
    class: 'storage-mobile-icon',
    'aria-hidden': 'true',
    fill: 'none',
    stroke: 'currentColor',
    'stroke-width': '1.6',
    'stroke-linecap': 'round',
    'stroke-linejoin': 'round',
  });
  if (kind === 'sd') {
    // 缺角卡片 + 三根触点
    svg.append(svgElement('path', { d: 'M8 3h6.6L18 6.4V20a1 1 0 0 1-1 1H8a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1z' }));
    svg.append(svgElement('path', { d: 'M10.6 8.4v3.2M12.9 8.4v3.2M15.2 8.4v3.2' }));
  } else {
    // U 盘：金属口 + 塑料壳
    svg.append(svgElement('rect', { x: '8', y: '9', width: '8', height: '12', rx: '1.6' }));
    svg.append(svgElement('path', { d: 'M11 9V5h2v4M10.5 13.5h3' }));
  }
  return svg;
}

function renderStorageMobile(storage = {}) {
  const list = $('storage-mobile-list');
  if (!list) return;
  const devices = Array.isArray(storage.mobile) ? storage.mobile : [];
  list.replaceChildren();
  // 没有设备时这里什么都不画：整块只有一行高，空状态由右边的计数说明，
  // 不再单独占一行写长句。
  devices.forEach((device) => {
    const kind = device.kind === 'sd' ? 'sd' : 'usb';
    const item = document.createElement('div');
    item.className = `storage-mobile-item kind-${kind}`;
    item.title = [device.name, device.size_label, device.device, device.mount_point ? `挂载 ${device.mount_point}` : '']
      .filter(Boolean).join(' · ');
    const caption = document.createElement('span');
    caption.textContent = device.kind_label || (kind === 'sd' ? 'SD 卡' : 'U 盘');
    const name = document.createElement('small');
    name.textContent = device.name || '';
    item.append(mobileDeviceIcon(kind), caption, name);
    list.append(item);
  });
  const count = $('storage-mobile-count');
  if (count) {
    count.textContent = devices.length ? `${devices.length} 个设备` : '未检测到';
    count.title = devices.length ? '' : '插好介质后最多 5 秒出现；换卡认不出来时点「刷新 SD 卡」';
  }
}

function renderDiagnosticChips(targetID, items, emptyText = '—') {  const target = $(targetID);
  target.replaceChildren();
  if (!items.length) {
    target.textContent = emptyText;
    return;
  }
  items.forEach((item) => {
    const chip = document.createElement('span');
    chip.className = `diagnostic-chip${item.tone ? ` ${item.tone}` : ''}`;
    if (item.label) {
      const label = document.createElement('b');
      label.textContent = item.label;
      chip.append(label);
    }
    const value = document.createElement('span');
    value.textContent = item.value;
    chip.append(value);
    target.append(chip);
  });
}

function fanSourceLabel(source = '') {
  const labels = { cpu: 'CPU', hdd: 'HDD/SATA', nvme: 'NVMe' };
  const parts = String(source).split('+').filter(Boolean).map((part) => labels[part] || part);
  return parts.length ? parts.join(' + ') : '当前温度';
}

function renderDiagnostics(status, fanStatus) {
  const temperatures = Array.isArray(status.temperatures) ? status.temperatures : [];
  renderDiagnosticChips('temperature-sensors', temperatures.map((item) => ({
    label: item.label || 'coretemp',
    value: formatTemperature(item.celsius),
  })), '未读取到 coretemp');

  const gpuRuntime = Array.isArray(status.gpu_runtime) ? status.gpu_runtime : [];
  renderDiagnosticChips('gpu-runtime', gpuRuntime.map((item) => {
    const value = String(item);
    return {
      value,
      tone: /active|运行|启用/i.test(value) ? 'ok' : (/unsupported|未暴露|不可用/i.test(value) ? 'muted' : ''),
    };
  }), '未暴露');

  if (!fanStatus.active) {
    renderDiagnosticChips('fan-control', [{
      value: fanStatus.driver_detected === false
        ? '未检测到或未加载 IT87 驱动'
        : (fanStatus.available ? '可用，尚未启用' : '驱动已检测，但风扇无有效转速反馈'),
      tone: fanStatus.available ? 'muted' : 'warning',
    }]);
    return;
  }
  const cpuTemperature = Number(fanStatus.cpu_temperature_c ?? fanStatus.temperature_c);
  const hddTemperature = Number(fanStatus.hdd_temperature_c);
  const nvmeTemperature = Number(fanStatus.nvme_temperature_c);
  const chips = [{
    label: 'CPU',
    value: `${formatTemperature(cpuTemperature, cpuTemperature > 0)} → ${fanStatus.cpu_target_pwm_percent || 0}%`,
  }];
  chips.push(hddTemperature > 0
    ? { label: 'HDD/SATA', value: `${formatTemperature(hddTemperature)} → ${fanStatus.hdd_target_pwm_percent || 0}%` }
    : { label: 'HDD/SATA', value: '温度暂不可用', tone: 'muted' });
  chips.push(nvmeTemperature > 0
    ? { label: 'NVMe', value: `${formatTemperature(nvmeTemperature)} → ${fanStatus.nvme_target_pwm_percent || 0}%` }
    : { label: 'NVMe', value: '温度暂不可用', tone: 'muted' });
  chips.push({
    label: '最终',
    value: `${fanSourceLabel(fanStatus.control_source)} · ${fanStatus.target_pwm_percent || 0}%`,
    tone: 'ok',
  });
  renderDiagnosticChips('fan-control', chips);
}

// 「关停」档下 0 转是设置出来的结果，不是接线/驱动故障：文案要说明白并催用户尽快退出
const FAN_OFF_RPM_NOTICE = '因为设置曲线模式为关停，风扇转速反馈为 0 RPM。该模式优先级最高：达到紧急温度也不会自动起转，超过阈值时只会蜂鸣报警；最长 5 分钟后会自动恢复原曲线并起转。请尽快取消此模式以免损坏硬件。';

// 后端 status 里带 off_mode；老状态没这个字段时退回读配置，两种来源都认
function fanOffModeActive(fanStatus = {}) {
  if (typeof fanStatus.off_mode === 'boolean') return fanStatus.off_mode;
  return Boolean(currentStatus?.config?.fan?.off_mode);
}

const FAN_ALARM_MUTE_HELP = '仅屏蔽关停档下每 20 秒一次的蜂鸣；界面报警与日志保留，退出「关停」后自动恢复。';

// 关停档报警临时静音：后端只接受关停档下的请求（其他档位没有 20 秒报警可静音），
// 静音状态是运行时字段、退出关停自动解除，所以每次 render 都以 status 回填。
function syncFanAlarmMute(fanStatus = {}) {
  const input = $('fan-alarm-mute');
  const row = $('fan-alarm-mute-row');
  const target = $('fan-alarm-mute-status');
  if (!input || !row || !target) return;
  const offMode = fanOffModeActive(fanStatus);
  input.checked = Boolean(fanStatus.alarm_muted);
  input.disabled = !offMode;
  row.classList.toggle('is-inactive', !offMode);
  if (!offMode) {
    target.textContent = '仅「关停」档下有效：该档位才有每 20 秒一次的故障/超温蜂鸣。';
    return;
  }
  target.textContent = input.checked
    ? '已临时静音：蜂鸣暂停，界面报警与日志仍保留；退出「关停」后自动恢复。'
    : FAN_ALARM_MUTE_HELP;
}

function healthIssues(status, fanStatus, storageStatus, gpioStatus) {
  const issues = [];
  if (!status.supported) issues.push('当前处理器未通过 TAD6S4N模块兼容性检查');
  if (fanStatus.driver_detected === false) issues.push('未检测到或未加载 IT87 风扇驱动');
  const fans = Array.isArray(fanStatus.fans) ? fanStatus.fans : [];
  if (fanStatus.driver_detected === true && !fans.length) {
    issues.push('IT87 驱动已检测，但未发现可读取 PWM/RPM 的风扇通道');
  } else if (fanStatus.driver_detected === true && fans.length && !fans.some((fan) => Number(fan.rpm) > 0)) {
    issues.push(fanOffModeActive(fanStatus)
      ? FAN_OFF_RPM_NOTICE
      : '已发现风扇通道，但当前转速反馈均为 0 RPM；请检查风扇接线或驱动状态');
  }
  if (status.cpu_temperature?.available === false) {
    issues.push('未读取到 CPU coretemp 温度；请检查 coretemp 内核驱动是否已加载');
  }
  if (status.last_error) issues.push(`模块：${status.last_error}`);
  if (fanStatus.last_error) issues.push(`风扇：${fanStatus.last_error}`);
  const storagePending = !storageStatus.updated_at
    && String(storageStatus.last_error || '').includes('尚未完成首次刷新');
  if (storageStatus.last_error && !storagePending) issues.push(`硬盘检测：${storageStatus.last_error}`);
  const slots = Array.isArray(storageStatus.slots) ? storageStatus.slots : [];
  const detectedSlots = slots.filter((slot) => slot.device
    || ['present', 'used', 'warning'].includes(slot.state));
  if (storageStatus.updated_at && !detectedSlots.length) {
    issues.push('未检测到任何已插入硬盘；若机器实际装有硬盘，请检查 AHCI/NVMe 驱动和槽位映射');
  }
  // 休眠盘不读取温度是设计行为（避免唤醒），不构成温度接口故障
  const missingTemperatureSlots = detectedSlots.filter((slot) => slot.activity !== 'sleeping' && !(Number(slot.temperature_c) > 0));
  if (missingTemperatureSlots.length) {
    const labels = missingTemperatureSlots.map((slot) => (slot.kind === 'front' ? `SATA ${slot.slot}` : `NVMe ${slot.slot}`));
    issues.push(`${labels.join('、')} 未读取到温度；请检查 SMART/NVMe 温度接口与相关驱动`);
  }
  slots.filter((slot) => slot.state === 'warning').forEach((slot) => {
    const label = slot.kind === 'front' ? `SATA ${slot.slot}` : `NVMe ${slot.slot}`;
    issues.push(`${label}：${slot.warning || slot.health || '硬盘健康状态告警'}`);
  });
  slots.filter((slot) => isStorageHot(slot)).forEach((slot) => {
    const label = slot.kind === 'front' ? `SATA ${slot.slot}` : `NVMe ${slot.slot}`;
    const threshold = STORAGE_HOT_C[slot.kind] || STORAGE_HOT_C.front;
    issues.push(`${label}：温度 ${formatTemperature(slot.temperature_c)}，达到高温阈值 ${threshold}°C`);
  });
  if (gpioStatus.enabled && (!gpioStatus.available || gpioStatus.last_error)) {
    issues.push(`按钮控制：${gpioStatus.last_error || 'GPIO 硬件接口不可用'}`);
  }
  return [...new Set(issues)];
}

function renderFanHardwareWarning(fanStatus = {}) {
  const warning = $('fan-driver-warning');
  const fans = Array.isArray(fanStatus.fans) ? fanStatus.fans : [];
  const missingDriver = fanStatus.driver_detected === false;
  const noChannel = fanStatus.driver_detected === true && !fans.length;
  const noRPM = fanStatus.driver_detected === true && fans.length
    && !fans.some((fan) => Number(fan.rpm) > 0);
  const isDSM = currentStatus?.platform === 'dsm';
  warning.hidden = !(missingDriver || noChannel || noRPM);
  if (missingDriver) {
    $('fan-warning-title').textContent = '未检测到或未加载 IT87 风扇驱动';
    // 群晖 DSM 没有 DKMS/gcc/内核头工具链，驱动只能来自引导加载器的驱动扩展
    // （RR 的 extra 驱动等）或系统预装模块，提示要与平台一致
    $('fan-warning-description').textContent = isDSM
      ? '风扇转速和 PWM 控制暂不可用。群晖 DSM 不支持 DKMS 自编译：请通过引导加载器的驱动扩展（如 RR 的 extra 驱动）加载 it87 模块，或确认系统已内置该驱动，然后重启本套件。'
      : '风扇转速和 PWM 控制暂不可用。本插件安装时应已尝试内置 DKMS；请确认 dkms/gcc/make/linux-headers 后重启模块。';
  } else if (noChannel) {
    $('fan-warning-title').textContent = '未发现可控风扇通道';
    $('fan-warning-description').textContent = 'IT87 驱动已检测，但没有同时暴露 fan、pwm 与 pwm_enable 的通道，请检查驱动适配。';
  } else if (noRPM) {
    // 关停档下 0 转是设置出来的结果，不是接线/驱动故障，别再把用户往接线方向带
    $('fan-warning-title').textContent = fanOffModeActive(fanStatus) ? '风扇已按「关停」档停转' : '风扇转速反馈为 0 RPM';
    $('fan-warning-description').textContent = fanOffModeActive(fanStatus)
      ? FAN_OFF_RPM_NOTICE
      : '已发现风扇通道，请检查单风扇接线、BIOS 模式或驱动状态。';
  }
  // 独立 IT87 驱动 FPK 是 fnOS 包，DSM 上不可用，不展示这个兜底链接
  $('fan-warning-driver-link').hidden = !missingDriver || isDSM;
}

function setupFanSlotSelectors() {
  Object.values(FAN_SLOT_GROUPS).forEach((group) => {
    const container = $(group.containerID);
    group.slots.forEach((slot) => {
      const label = document.createElement('label');
      label.className = 'curve-slot-option';
      label.dataset.slotId = slot.id;
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.value = slot.id;
      input.checked = true;
      const name = document.createElement('b');
      name.textContent = slot.label;
      const temperature = document.createElement('small');
      temperature.textContent = '等待温度';
      label.append(input, name, temperature);
      container.append(label);
    });
  });
}

function fillFanSlotSelections(fan = {}) {
  Object.values(FAN_SLOT_GROUPS).forEach((group) => {
    const configured = Array.isArray(fan[group.configField])
      ? fan[group.configField] : group.slots.map((slot) => slot.id);
    const selected = new Set(configured);
    $(group.containerID).querySelectorAll('input[type="checkbox"]').forEach((input) => {
      input.checked = selected.has(input.value);
    });
  });
}

function selectedFanSlotIDs(kind) {
  const group = FAN_SLOT_GROUPS[kind];
  return [...$(group.containerID).querySelectorAll('input[type="checkbox"]:checked')]
    .map((input) => input.value);
}

function renderFanSlotTemperatures(storage = {}) {
  const slots = new Map((storage.slots || []).map((slot) => [slot.id, slot]));
  Object.values(FAN_SLOT_GROUPS).forEach((group) => {
    group.slots.forEach((definition) => {
      const label = $(group.containerID).querySelector(`[data-slot-id="${definition.id}"]`);
      const slot = slots.get(definition.id);
      const state = slot?.state || 'unknown';
      label.className = `curve-slot-option storage-${state}`;
      const temperature = Number(slot?.temperature_c);
      const stateLabel = STORAGE_STATE_LABELS[state] || state;
      label.querySelector('small').textContent = temperature > 0
        ? `${formatTemperature(temperature)} · ${stateLabel}`
        : stateLabel;
    });
  });
}

function gpioScriptAction(scriptID) {
  return `script:${scriptID}`;
}

function gpioBackupAction(jobID) {
  return `backup:${jobID}`;
}

function gpioAutoBackupAction(ruleID) {
  return `auto_backup:${ruleID}`;
}

function setupGPIOActions(scripts = gpioScripts, preserveValues = true, backups = gpioBackups, autos = gpioAutos) {
  document.querySelectorAll('.gpio-action').forEach((select) => {
    const prior = preserveValues ? select.value : 'none';
    select.replaceChildren();
    const builtInGroup = document.createElement('optgroup');
    builtInGroup.label = '内置动作';
    GPIO_ACTIONS.forEach(([value, label]) => {
      const option = document.createElement('option');
      option.value = value;
      option.textContent = label;
      builtInGroup.append(option);
    });
    select.append(builtInGroup);
    if (backups.length) {
      const backupGroup = document.createElement('optgroup');
      backupGroup.label = '备份任务';
      backups.forEach((job) => {
        const option = document.createElement('option');
        option.value = gpioBackupAction(job.id);
        option.textContent = `备份：${job.name}`;
        backupGroup.append(option);
      });
      select.append(backupGroup);
    }
    if (autos.length) {
      const autoGroup = document.createElement('optgroup');
      autoGroup.label = '自动备份';
      autos.forEach((rule) => {
        const option = document.createElement('option');
        option.value = gpioAutoBackupAction(rule.id);
        option.textContent = `自动备份：${rule.name}`;
        autoGroup.append(option);
      });
      select.append(autoGroup);
    }
    if (scripts.length) {
      const scriptGroup = document.createElement('optgroup');
      scriptGroup.label = 'Shell 脚本';
      scripts.forEach((script) => {
        const option = document.createElement('option');
        option.value = gpioScriptAction(script.id);
        option.textContent = `脚本：${script.name}`;
        scriptGroup.append(option);
      });
      select.append(scriptGroup);
    }
    select.value = [...select.options].some((option) => option.value === prior) ? prior : 'none';
  });
}

function showGPIOScriptMessage(message, error = false) {
  $('gpio-script-message').textContent = message;
  $('gpio-script-message').className = `inline-status${error ? ' error' : ''}`;
}

function generateGPIOScriptID() {
  if (window.crypto?.randomUUID) return window.crypto.randomUUID();
  return `script-${Date.now()}-${Math.random().toString(16).slice(2, 10)}`;
}

function renderGPIOScriptList() {
  const list = $('gpio-script-list');
  list.replaceChildren();
  if (!gpioScripts.length) {
    const empty = document.createElement('p');
    empty.className = 'gpio-script-empty';
    empty.textContent = '尚未创建脚本。点击“新增脚本”开始编写。';
    list.append(empty);
    return;
  }
  gpioScripts.forEach((script) => {
    const action = gpioScriptAction(script.id);
    const bindings = [...document.querySelectorAll('.gpio-action')]
      .filter((select) => select.value === action).length;
    const card = document.createElement('article');
    card.className = 'gpio-script-card';
    const name = document.createElement('strong');
    name.textContent = script.name;
    const detail = document.createElement('small');
    detail.textContent = `${new TextEncoder().encode(script.body).length} 字节 · ${bindings ? `已绑定 ${bindings} 项` : '尚未绑定'}`;
    const actions = document.createElement('div');
    actions.className = 'actions';
    const edit = document.createElement('button');
    edit.type = 'button';
    edit.textContent = '编辑';
    edit.addEventListener('click', () => openGPIOScriptEditor(script.id));
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'delete-script';
    remove.textContent = '删除';
    remove.addEventListener('click', () => deleteGPIOScript(script.id));
    actions.append(edit, remove);
    card.append(name, detail, actions);
    list.append(card);
  });
}

function bashTokenClass(token) {
  if (token.startsWith('#')) return 'bash-token-comment';
  if (token.startsWith("'") || token.startsWith('"')) return 'bash-token-string';
  if (token.startsWith('$')) return 'bash-token-variable';
  if (/^\d+$/.test(token)) return 'bash-token-number';
  return 'bash-token-keyword';
}

function renderBashHighlight() {
  const textarea = $('gpio-script-body');
  const code = $('gpio-script-highlight').querySelector('code');
  const source = textarea.value;
  const pattern = /'(?:[^']*)'|"(?:\\.|[^"\\])*"|\$\{[^}\n]*\}|\$[A-Za-z_][A-Za-z0-9_]*|\$[0-9#?*@!_-]|#[^\n]*|\b(?:if|then|else|elif|fi|for|while|until|do|done|case|esac|in|function|select|time|coproc|break|continue|return|exit|local|readonly|declare|typeset|export|unset|shift|source|alias|unalias|set|trap|true|false)\b|\b\d+\b/g;
  code.replaceChildren();
  let offset = 0;
  for (const match of source.matchAll(pattern)) {
    if (match.index > offset) code.append(document.createTextNode(source.slice(offset, match.index)));
    const token = document.createElement('span');
    token.className = bashTokenClass(match[0]);
    token.textContent = match[0];
    code.append(token);
    offset = match.index + match[0].length;
  }
  code.append(document.createTextNode(`${source.slice(offset)}\n`));
  const bytes = new TextEncoder().encode(source).length;
  $('gpio-script-size').textContent = `${bytes} / ${GPIO_SCRIPT_MAX_BODY_BYTES} 字节`;
  $('gpio-script-size').className = bytes > GPIO_SCRIPT_MAX_BODY_BYTES ? 'error' : '';
  syncBashEditorScroll();
}

function syncBashEditorScroll() {
  const textarea = $('gpio-script-body');
  const highlight = $('gpio-script-highlight');
  highlight.scrollTop = textarea.scrollTop;
  highlight.scrollLeft = textarea.scrollLeft;
}

function openGPIOScriptEditor(scriptID = '') {
  const script = gpioScripts.find((item) => item.id === scriptID);
  gpioEditingScriptID = script?.id || generateGPIOScriptID();
  $('gpio-script-id').value = gpioEditingScriptID;
  $('gpio-script-name').value = script?.name || '';
  $('gpio-script-body').value = script?.body || '#!/usr/bin/env bash\nset -euo pipefail\n\n';
  $('gpio-script-editor').hidden = false;
  showGPIOScriptMessage(script ? `正在编辑“${script.name}”` : '正在创建新脚本');
  renderBashHighlight();
  $('gpio-script-name').focus();
}

function closeGPIOScriptEditor() {
  gpioEditingScriptID = '';
  $('gpio-script-editor').hidden = true;
  $('gpio-script-id').value = '';
  $('gpio-script-name').value = '';
  $('gpio-script-body').value = '';
  renderBashHighlight();
}

function commitGPIOScriptEditor() {
  if ($('gpio-script-editor').hidden) return true;
  const id = $('gpio-script-id').value;
  const name = $('gpio-script-name').value.trim();
  const body = $('gpio-script-body').value.replace(/\r\n?/g, '\n');
  const bodyBytes = new TextEncoder().encode(body).length;
  if (!name) {
    showGPIOScriptMessage('脚本名称不能为空。', true);
    $('gpio-script-name').focus();
    return false;
  }
  if (gpioScripts.some((script) => script.id !== id && script.name.trim().toLowerCase() === name.toLowerCase())) {
    showGPIOScriptMessage(`脚本名称“${name}”已经存在。`, true);
    $('gpio-script-name').focus();
    return false;
  }
  if (!body.trim()) {
    showGPIOScriptMessage('脚本内容不能为空。', true);
    $('gpio-script-body').focus();
    return false;
  }
  if (bodyBytes > GPIO_SCRIPT_MAX_BODY_BYTES) {
    showGPIOScriptMessage(`脚本内容不能超过 ${GPIO_SCRIPT_MAX_BODY_BYTES} 字节。`, true);
    return false;
  }
  const existingIndex = gpioScripts.findIndex((script) => script.id === id);
  if (existingIndex < 0 && gpioScripts.length >= GPIO_SCRIPT_MAX_COUNT) {
    showGPIOScriptMessage(`最多只能创建 ${GPIO_SCRIPT_MAX_COUNT} 个脚本。`, true);
    return false;
  }
  const script = { id, name, body };
  if (existingIndex >= 0) gpioScripts[existingIndex] = script;
  else gpioScripts.push(script);
  setupGPIOActions(gpioScripts, true);
  renderGPIOScriptList();
  closeGPIOScriptEditor();
  showGPIOScriptMessage(`脚本“${name}”已加入本页草稿；点击“保存按钮控制”后生效。`);
  return true;
}

async function deleteGPIOScript(scriptID) {
  const script = gpioScripts.find((item) => item.id === scriptID);
  if (!script) return;
  const action = gpioScriptAction(scriptID);
  const binding = [...document.querySelectorAll('.gpio-action')].find((select) => select.value === action);
  if (binding) {
    showGPIOScriptMessage(`脚本“${script.name}”仍被按键动作引用，请先在上方下拉框改成其他动作。`, true);
    binding.focus();
    return;
  }
  const ok = await showAppConfirm({
    title: '删除脚本',
    message: `确定删除脚本“${script.name}”吗？删除后仍需点击“保存按钮控制”才会持久化。`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok) return;
  gpioScripts = gpioScripts.filter((item) => item.id !== scriptID);
  if (gpioEditingScriptID === scriptID) closeGPIOScriptEditor();
  setupGPIOActions(gpioScripts, true);
  renderGPIOScriptList();
  showGPIOScriptMessage(`脚本“${script.name}”已从本页草稿删除。`);
}

// ---------------------------------------------------------------------------
// 备份任务：图形化选择「源 → 目标」，对应后端 backup.go
// ---------------------------------------------------------------------------

const BACKUP_MODE_LABELS = {
  copy: '直接复制覆盖',
  sync: '同步文件',
  archive: '按日期增量归档',
  move_overwrite: '移动覆盖',
  move_archive: '按日期转移归档',
};

const AUTO_DIRECTION_LABELS = {
  from_volume: '打标卷 → NAS',
  to_volume: 'NAS → 打标卷',
};

function normalizeBackupLocation(location) {
  if (!location || typeof location !== 'object') return null;
  if (location.kind === 'volume') {
    const uuid = String(location.uuid || '').trim();
    const label = String(location.label || '').trim();
    if (!uuid && !label) return null;
    return { kind: 'volume', uuid, label, sub: String(location.sub || '').trim() };
  }
  const path = String(location.path || '').trim();
  return path ? { kind: 'path', path } : null;
}

function backupLocationSummary(location) {
  if (!location) return '';
  if (location.kind === 'volume') {
    const name = location.label || location.uuid;
    return location.sub ? `卷 ${name} / ${location.sub}` : `卷 ${name}`;
  }
  return location.path;
}

function formatByteSize(bytes) {
  const value = Number(bytes) || 0;
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let scaled = value / 1024;
  let index = 0;
  while (scaled >= 1024 && index < units.length - 1) { scaled /= 1024; index += 1; }
  return `${scaled >= 100 ? scaled.toFixed(0) : scaled.toFixed(1)} ${units[index]}`;
}

function formatClock(iso) {
  const date = iso ? new Date(iso) : null;
  if (!date || Number.isNaN(date.getTime())) return '—';
  return date.toLocaleTimeString('zh-CN', { hour12: false });
}

function formatDuration(milliseconds) {
  const seconds = Math.max(0, Math.round((Number(milliseconds) || 0) / 1000));
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`;
  return `${Math.floor(minutes / 60)} 小时 ${minutes % 60} 分`;
}

function showBackupStatus(message, error = false) {
  const target = $('backup-status');
  target.textContent = message;
  target.className = `inline-status${error ? ' error' : ''}`;
}

function showBackupJobMessage(message, error = false) {
  const target = $('backup-job-message');
  target.textContent = message;
  target.className = `inline-status${error ? ' error' : ''}`;
}

function generateBackupJobID() {
  if (window.crypto?.randomUUID) return window.crypto.randomUUID();
  return `backup-${Date.now()}-${Math.random().toString(16).slice(2, 10)}`;
}

function backupJobBindings(jobID) {
  const action = gpioBackupAction(jobID);
  return [...document.querySelectorAll('.gpio-action')]
    .filter((select) => select.value === action)
    .map((select) => {
      const row = select.closest('tr');
      const labels = { short: '短按', hold_3s: '长按3秒', hold_9s: '长按9秒', hold_15s: '长按15秒' };
      return `${row?.dataset.button || '按键'}·${labels[select.dataset.stage] || select.dataset.stage}`;
    });
}

function renderBackupJobList() {
  const list = $('backup-job-list');
  list.replaceChildren();
  if (!gpioBackups.length) {
    const empty = document.createElement('p');
    empty.className = 'gpio-script-empty';
    empty.textContent = '尚未创建备份任务。点击「新增备份任务」，选好源和目标即可。';
    list.append(empty);
    return;
  }
  gpioBackups.forEach((job) => {
    const card = document.createElement('article');
    card.className = 'backup-job-card';

    const title = document.createElement('strong');
    title.textContent = job.name;
    card.append(title);

    const flow = document.createElement('p');
    flow.className = 'backup-job-flow';
    flow.textContent = `${backupLocationSummary(job.source)} → ${backupLocationSummary(job.target)}`;
    card.append(flow);

    const meta = document.createElement('small');
    const bindings = backupJobBindings(job.id);
    meta.textContent = [
      BACKUP_MODE_LABELS[job.mode] || job.mode,
      job.mode === 'archive' ? (job.keep > 0 ? `保留 ${job.keep} 份` : '不限份数') : '',
      job.mode !== 'archive' ? (job.overwrite ? '覆盖同名文件' : '跳过已存在') : '',
      bindings.length ? `已绑定：${bindings.join('、')}` : '未绑定按键',
    ].filter(Boolean).join(' · ');
    card.append(meta);

    const actions = document.createElement('div');
    actions.className = 'actions';
    const runButton = document.createElement('button');
    runButton.type = 'button';
    runButton.className = 'run-backup';
    runButton.textContent = '立即运行';
    runButton.addEventListener('click', () => runBackupJob(job.id, false));
    const editButton = document.createElement('button');
    editButton.type = 'button';
    editButton.textContent = '编辑';
    editButton.addEventListener('click', () => openBackupJobEditor(job.id));
    const removeButton = document.createElement('button');
    removeButton.type = 'button';
    removeButton.className = 'delete-script';
    removeButton.textContent = '删除';
    removeButton.addEventListener('click', () => deleteBackupJob(job.id));
    actions.append(runButton, editButton, removeButton);
    card.append(actions);

    list.append(card);
  });
}

function backupModeFromEditor() {
  const checked = document.querySelector('input[name="backup-mode"]:checked');
  return checked ? checked.value : 'copy';
}

// backupKeepAnchorID 归档保留份数该贴在哪一行下面：两个「归档」档位各自一个锚点，
// 其它档位用不到这个输入框（返回空字符串表示整行藏起来）。
function backupKeepAnchorID(mode, prefix) {
  if (mode === 'archive') return `${prefix}-mode-archive`;
  if (mode === 'move_archive') return `${prefix}-mode-move-archive`;
  return '';
}

// syncBackupKeepRow 把保留份数那一行挪到当前选中的归档档位正下方——选中哪个归档档位，
// 它就在哪个下面出现，不选就藏起来。
function syncBackupKeepRow(rowID, anchorID) {
  const row = $(rowID);
  if (!row) return;
  if (!anchorID) {
    row.hidden = true;
    return;
  }
  const anchor = $(anchorID);
  row.hidden = false;
  // 已经在位就不动它，免得每次刷新控件都重建一次 DOM。
  if (anchor && anchor.nextElementSibling !== row) anchor.insertAdjacentElement('afterend', row);
}

function updateBackupModeControls() {
  const mode = backupModeFromEditor();
  const moveMode = mode === 'move_overwrite' || mode === 'move_archive';
  $('backup-job-overwrite').disabled = mode !== 'copy';
  $('backup-job-keep').disabled = mode !== 'archive' && mode !== 'move_archive';
  syncBackupKeepRow('backup-job-keep-row', backupKeepAnchorID(mode, 'backup-job'));
  const hint = $('backup-job-mode-hint');
  if (moveMode) {
    hint.textContent = '移动类模式固定覆盖同名文件，并在复制成功后删除源里的文件（只删已成功复制的，目录结构保留）。源不能是整个「我的文件」「团队文件」或远程挂载根目录，也不能是系统目录。';
  } else {
    hint.textContent = '';
  }
}

function refreshBackupLocationHints() {
  ['source', 'target'].forEach((role) => {
    const hint = $(role === 'source' ? 'backup-job-source-hint' : 'backup-job-target-hint');
    const location = backupEditorLocations[role];
    if (!location) {
      hint.textContent = role === 'source'
        ? '还没有选择源：可以选硬盘上的目录，也可以选一个可移动卷。'
        : '还没有选择目标：备份会放到这个目录里。';
      return;
    }
    if (location.kind === 'volume') {
      hint.textContent = `按卷标/UUID 记住介质本身，换接口或换挂载路径后仍能自动找到。`;
      return;
    }
    hint.textContent = '';
  });
}

// resetBackupMarkerForSourceChange 换了源介质，原标识就作废：清掉内容并关掉开关，
// 免得把 A 盘的标识写进 B 盘——那反而让「防插错盘」失效。
function resetBackupMarkerForSourceChange() {
  const hadMarker = !!backupMarkerStash || !!$('backup-job-marker').value.trim() || $('backup-job-marker-write').checked;
  backupMarkerStash = '';
  $('backup-job-marker').value = '';
  $('backup-job-marker-write').checked = false;
  if (hadMarker) showBackupJobMessage('已切换源介质：原标识作废，需要时请重新打开「在源中创建标识文件」。');
}

function setBackupEditorLocation(role, location) {
  backupEditorLocations[role] = location;
  $(role === 'source' ? 'backup-job-source' : 'backup-job-target').value = backupLocationSummary(location) || '';
  if (role === 'source') resetBackupMarkerForSourceChange();
  refreshBackupLocationHints();
}

function openBackupJobEditor(jobID = '') {
  const job = gpioBackups.find((item) => item.id === jobID);
  backupEditingJobID = job ? job.id : generateBackupJobID();
  $('backup-job-id').value = backupEditingJobID;
  $('backup-job-name').value = job ? job.name : '';
  $('backup-job-marker').value = job ? (job.marker || '') : '';
  backupMarkerStash = job ? String(job.marker || '').trim() : '';
  $('backup-job-marker-write').checked = !!backupMarkerStash;
  $('backup-job-keep').value = job ? String(job.keep || 0) : '0';
  $('backup-job-overwrite').checked = job ? job.overwrite !== false : true;
  document.querySelectorAll('input[name="backup-mode"]').forEach((radio) => {
    radio.checked = radio.value === (job ? job.mode : 'copy');
  });
  backupEditorLocations.source = job ? job.source : null;
  backupEditorLocations.target = job ? job.target : null;
  $('backup-job-source').value = backupLocationSummary(backupEditorLocations.source);
  $('backup-job-target').value = backupLocationSummary(backupEditorLocations.target);
  $('backup-job-editor').hidden = false;
  updateBackupModeControls();
  refreshBackupLocationHints();
  showBackupJobMessage(job ? `正在编辑「${job.name}」` : '正在创建新的备份任务');
  $('backup-job-name').focus();
}

function closeBackupJobEditor() {
  backupEditingJobID = '';
  backupEditorLocations.source = null;
  backupEditorLocations.target = null;
  $('backup-job-editor').hidden = true;
  $('backup-job-id').value = '';
  $('backup-job-name').value = '';
  $('backup-job-source').value = '';
  $('backup-job-target').value = '';
  $('backup-job-marker').value = '';
  backupMarkerStash = '';
  $('backup-job-marker-write').checked = false;
  $('backup-job-keep').value = '0';
  $('backup-job-overwrite').checked = true;
  showBackupJobMessage('');
}

function backupJobFromEditor() {
  const mode = backupModeFromEditor();
  return {
    id: backupEditingJobID || generateBackupJobID(),
    name: $('backup-job-name').value.trim(),
    source: backupEditorLocations.source,
    target: backupEditorLocations.target,
    mode,
    overwrite: mode === 'copy' ? $('backup-job-overwrite').checked : true,
    keep: mode === 'archive' || mode === 'move_archive' ? Math.max(0, Math.round(Number($('backup-job-keep').value) || 0)) : 0,
    // 标识校验由开关决定：关掉 = 留空 = 不校验；开着时内容就是输入框里的值。
    marker: $('backup-job-marker-write').checked ? $('backup-job-marker').value.trim() : '',
  };
}

function validateBackupJobDraft(job) {
  if (!job.name) {
    showBackupJobMessage('任务名称不能为空。', true);
    $('backup-job-name').focus();
    return false;
  }
  if (gpioBackups.some((item) => item.id !== job.id && item.name.trim().toLowerCase() === job.name.toLowerCase())) {
    showBackupJobMessage(`任务名称“${job.name}”已存在。`, true);
    $('backup-job-name').focus();
    return false;
  }
  if (!job.source) {
    showBackupJobMessage('请先选择源（要备份的目录或可移动卷）。', true);
    return false;
  }
  if (!job.target) {
    showBackupJobMessage('请先选择目标（备份放到哪个目录）。', true);
    return false;
  }
  if (job.source.kind === 'path' && job.target.kind === 'path' && job.source.path === job.target.path) {
    showBackupJobMessage('源与目标不能是同一个位置。', true);
    return false;
  }
  // 同步模式会把目标里多余的文件删掉，直接指到整个「我的文件」等于清空个人空间。
  if (job.target.kind === 'path' && /^\/vol[0-9]+\/[0-9]+\/?$/.test(job.target.path)) {
    showBackupJobMessage('目标不能直接是整个「我的文件」根目录，请在它下面新建一个备份目录。', true);
    return false;
  }
  // 开关开着却没有标识内容，说明写入没成功（比如写的时候没选源），别把「半开」的状态存下来。
  if ($('backup-job-marker-write').checked && !job.marker) {
    showBackupJobMessage('标识还没写进源介质：请重新打开「在源中创建标识文件」完成写入。', true);
    return false;
  }
  return true;
}

// onBackupMarkerToggleChange 开关的 change 处理器（返回 promise 便于测试等待）。
function onBackupMarkerToggleChange() {
  return applyBackupMarkerToggle($('backup-job-marker-write').checked);
}

// applyBackupMarkerToggle 「在源中创建标识文件」开关：打开＝把标识写进源介质并把内容
// 回填到输入框（留空时由后端自动生成），关掉＝不校验（清空标识）。
// 刻意做成「用户明确打开才写」：若在执行备份时自动补写，插错盘也会被写成「对的盘」，
// 这项校验就完全失去意义了。
async function applyBackupMarkerToggle(enabled) {
  const toggle = $('backup-job-marker-write');
  if (!enabled) {
    backupMarkerStash = $('backup-job-marker').value.trim();
    $('backup-job-marker').value = '';
    showBackupJobMessage('已关闭标识校验：以后不再核对源介质，源里已有的标识文件保持原样。');
    return;
  }
  const source = backupEditorLocations.source;
  if (!source) {
    toggle.checked = false;
    showBackupJobMessage('请先选择源，再打开「在源中创建标识文件」。', true);
    return;
  }
  // 沿用输入框里的内容；输入框空着就用关掉开关时暂存的那一份（同一块盘＝同一个标识）。
  const marker = $('backup-job-marker').value.trim() || backupMarkerStash;
  toggle.disabled = true;
  showBackupJobMessage('正在写入标识文件…');
  try {
    const result = await request('api/backup/marker', {
      method: 'POST',
      body: JSON.stringify({ job: { source, marker } }),
    });
    backupMarkerStash = result.marker;
    $('backup-job-marker').value = result.marker;
    showBackupJobMessage(`已写入 ${result.path}，标识内容：${result.marker}（记得点「保存任务」持久化）`);
  } catch (error) {
    toggle.checked = false;
    showBackupJobMessage(`写入失败：${error.message}`, true);
  } finally {
    toggle.disabled = false;
  }
}

function commitBackupJobEditor() {
  if ($('backup-job-editor').hidden) return true;
  const job = backupJobFromEditor();
  if (!validateBackupJobDraft(job)) return false;
  const index = gpioBackups.findIndex((item) => item.id === job.id);
  if (index >= 0) gpioBackups[index] = job;
  else gpioBackups.push(job);
  closeBackupJobEditor();
  setupGPIOActions(gpioScripts, true, gpioBackups);
  renderBackupJobList();
  showBackupStatus(`任务“${job.name}”已加入草稿。别忘了点击下方的「保存按钮控制」持久化。`);
  return true;
}

async function deleteBackupJob(jobID) {
  const job = gpioBackups.find((item) => item.id === jobID);
  if (!job) return;
  const bindings = backupJobBindings(jobID);
  if (bindings.length) {
    showBackupStatus(`任务“${job.name}”仍被按键（${bindings.join('、')}）引用，请先在上方下拉框改成其他动作。`, true);
    return;
  }
  const ok = await showAppConfirm({
    title: '删除备份任务',
    message: `确定删除备份任务“${job.name}”吗？删除后仍需点击“保存按钮控制”才会持久化。`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok) return;
  gpioBackups = gpioBackups.filter((item) => item.id !== jobID);
  if (backupEditingJobID === jobID) closeBackupJobEditor();
  setupGPIOActions(gpioScripts, true, gpioBackups);
  renderBackupJobList();
  showBackupStatus(`任务“${job.name}”已从本页草稿删除。`);
}

// ---------------------------------------------------------------------------
// 自动备份（打标卷）：对应后端 backup_auto.go
// ---------------------------------------------------------------------------

function showAutoBackupStatus(message, error = false) {
  const target = $('auto-backup-status');
  target.textContent = message;
  target.className = `inline-status${error ? ' error' : ''}`;
}

function showAutoBackupEditorMessage(message, error = false) {
  const target = $('auto-backup-message');
  target.textContent = message;
  target.className = `inline-status${error ? ' error' : ''}`;
}

function generateAutoBackupRuleID() {
  if (window.crypto?.randomUUID) return window.crypto.randomUUID();
  return `auto-${Date.now()}-${Math.random().toString(16).slice(2, 10)}`;
}

function autoBackupBindings(ruleID) {
  const action = gpioAutoBackupAction(ruleID);
  return [...document.querySelectorAll('.gpio-action')]
    .filter((select) => select.value === action)
    .map((select) => {
      const row = select.closest('tr');
      const labels = { short: '短按', hold_3s: '长按3秒', hold_9s: '长按9秒', hold_15s: '长按15秒' };
      return `${row?.dataset.button || '按键'}·${labels[select.dataset.stage] || select.dataset.stage}`;
    });
}

function renderAutoBackupList() {
  const list = $('auto-backup-list');
  list.replaceChildren();
  if (!gpioAutos.length) {
    const empty = document.createElement('p');
    empty.className = 'gpio-script-empty';
    empty.textContent = '尚未创建自动备份规则。先给 U 盘打标，再点「新增自动备份规则」。';
    list.append(empty);
    return;
  }
  gpioAutos.forEach((rule) => {
    const card = document.createElement('article');
    card.className = 'backup-job-card';

    const title = document.createElement('strong');
    title.textContent = rule.name;
    card.append(title);

    const flow = document.createElement('p');
    flow.className = 'backup-job-flow';
    flow.textContent = rule.direction === 'to_volume'
      ? `${backupLocationSummary(rule.fixed)} → 打标卷${rule.volume_sub ? `／${rule.volume_sub}` : '根目录'}`
      : `打标卷（按打标子目录；整卷打标即整块盘） → ${backupLocationSummary(rule.fixed)}`;
    card.append(flow);

    const meta = document.createElement('small');
    const bindings = autoBackupBindings(rule.id);
    const moveMode = rule.mode === 'move_overwrite' || rule.mode === 'move_archive';
    meta.textContent = [
      AUTO_DIRECTION_LABELS[rule.direction] || rule.direction,
      BACKUP_MODE_LABELS[rule.mode] || rule.mode,
      rule.mode === 'archive' || rule.mode === 'move_archive' ? (rule.keep > 0 ? `保留 ${rule.keep} 份` : '不限份数') : '',
      rule.mode === 'copy' ? (rule.overwrite ? '覆盖同名文件' : '跳过已存在') : '',
      moveMode ? (autoRuleDeletesNasSource(rule) ? '移动模式：会删除 NAS 上的源文件' : '复制成功后删源') : '',
      bindings.length ? `已绑定：${bindings.join('、')}` : '未绑定按键',
    ].filter(Boolean).join(' · ');
    card.append(meta);

    const actions = document.createElement('div');
    actions.className = 'actions';
    const runButton = document.createElement('button');
    runButton.type = 'button';
    runButton.className = 'run-backup';
    runButton.textContent = '立即运行';
    runButton.addEventListener('click', () => runAutoBackup(rule.id, false));
    const editButton = document.createElement('button');
    editButton.type = 'button';
    editButton.textContent = '编辑';
    editButton.addEventListener('click', () => openAutoBackupEditor(rule.id));
    const removeButton = document.createElement('button');
    removeButton.type = 'button';
    removeButton.className = 'delete-script';
    removeButton.textContent = '删除';
    removeButton.addEventListener('click', () => deleteAutoBackupRule(rule.id));
    actions.append(runButton, editButton, removeButton);
    card.append(actions);

    list.append(card);
  });
}

// --- 打标管理（在线 / 离线设备列表）-----------------------------------------

async function refreshAutoBackupDevices() {
  const container = $('auto-backup-devices');
  try {
    const data = await request('api/backup/auto');
    autoDevices = Array.isArray(data.devices) ? data.devices : [];
    if (data.marker_dir) autoMarkerDirFile = String(data.marker_dir);
    renderAutoBackupDevices();
  } catch (error) {
    container.replaceChildren();
    const failed = document.createElement('p');
    failed.className = 'auto-device-empty';
    failed.textContent = `打标设备列表加载失败：${error.message}`;
    container.append(failed);
  }
}

function autoDeviceName(device) {
  return device.alias || device.label || device.identity || device.device || '未知设备';
}

// 内置存储没有别名/卷标可言，identity 往往是 /dev/mapper/trim_…-0 这类内部名字，
// 对用户毫无意义，也永远不能打标 —— 列表里只留一句「内置存储 · 不支持打标」，
// 多块内置卷合并成同一行（这块盘本身不需要被识别，反正不参与自动备份）。
function builtinStorageLabel(devices) {
  const count = devices.filter((device) => device.removable === false).length;
  if (!count) return '';
  return count > 1 ? `内置存储（${count} 块）· 不支持打标` : '内置存储 · 不支持打标';
}

function renderAutoBackupDevices() {
  const container = $('auto-backup-devices');
  container.replaceChildren();
  if (!autoDevices.length) {
    const empty = document.createElement('p');
    empty.className = 'auto-device-empty';
    empty.textContent = '没有检测到存储设备。插上 U 盘 / 移动硬盘后点「刷新列表」。';
    container.append(empty);
    return;
  }
  autoDevices
    .filter((device) => device.removable !== false)
    .forEach((device) => container.append(buildAutoDeviceItem(device)));
  const builtin = builtinStorageLabel(autoDevices);
  if (builtin) {
    const item = document.createElement('article');
    item.className = 'auto-device-item builtin';
    const name = document.createElement('strong');
    name.textContent = builtin;
    item.append(name);
    container.append(item);
  }
}

function buildAutoDeviceItem(device) {
  const item = document.createElement('article');
  item.className = `auto-device-item${device.online ? '' : ' offline'}`;

  const head = document.createElement('div');
  head.className = 'auto-device-head';
  const name = document.createElement('strong');
  name.textContent = autoDeviceName(device);
  head.append(name);

  const badge = document.createElement('span');
  if (!device.online) {
    badge.className = 'auto-device-badge off';
    badge.textContent = device.pending_clear ? '离线 · 已预约清除打标' : '离线';
  } else if (device.removable === false) {
    badge.className = 'auto-device-badge';
    badge.textContent = '内置存储 · 不支持打标';
  } else if (device.marked) {
    badge.className = 'auto-device-badge on';
    badge.textContent = device.legacy ? '已打标（旧格式，下次打标自动升级）' : '已打标';
  } else {
    badge.className = 'auto-device-badge';
    badge.textContent = '未打标';
  }
  head.append(badge);
  if (device.online && device.read_only) {
    const ro = document.createElement('span');
    ro.className = 'auto-device-badge';
    ro.textContent = '只读';
    head.append(ro);
  }
  // 副标题只补充标题里没有的信息，并按介质区分：
  // 设备节点名（/dev/sdc1）只对可移动介质有意义；内置卷的 /dev/mapper/trim_…-0
  // 是内部实现，配上挂载点与标题会重复三遍同一串字，一律不展示。
  const meta = document.createElement('small');
  const where = device.online ? device.mount_point : `${device.mount_point || '—'}（上次挂载）`;
  const title = autoDeviceName(device);
  const parts = [];
  if (device.removable && device.device) parts.push(device.device);
  if (where && where !== title) parts.push(where);
  if (device.identity && device.identity !== title && device.identity !== device.device) {
    parts.push(device.identity);
  }
  meta.textContent = parts.filter(Boolean).join(' · ');
  if (meta.textContent) head.append(meta);
  item.append(head);

  const subsRow = document.createElement('div');
  subsRow.className = 'auto-device-subs';
  const subs = Array.isArray(device.subs) ? device.subs : [];
  if (!device.marked) {
    const hint = document.createElement('span');
    hint.className = 'empty';
    hint.textContent = '打标后这块盘才会参与自动备份（可整块卷，也可只标其中几个子目录）。';
    subsRow.append(hint);
  } else if (!subs.length) {
    const whole = document.createElement('span');
    whole.className = 'empty';
    whole.textContent = '操作范围：整块卷';
    subsRow.append(whole);
  } else {
    const label = document.createElement('span');
    label.className = 'empty';
    label.textContent = '只操作这些子目录：';
    subsRow.append(label);
    subs.forEach((sub) => {
      const chip = document.createElement('span');
      chip.className = 'auto-device-sub';
      chip.append(document.createTextNode(sub));
      if (device.online && device.removable && !device.read_only) {
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.textContent = '×';
        remove.title = `取消 ${sub} 的打标`;
        remove.addEventListener('click', () => autoBackupMarkerAction('sub_remove', {
          path: `${device.mount_point}/${sub}`,
        }));
        chip.append(remove);
      }
      subsRow.append(chip);
    });
  }
  item.append(subsRow);

  const actions = document.createElement('div');
  actions.className = 'auto-device-actions';
  if (!device.online) {
    const forget = document.createElement('button');
    forget.type = 'button';
    forget.className = device.pending_clear ? '' : 'danger';
    forget.textContent = device.pending_clear ? '撤销「下次上线清除」' : '取消打标（设备不在，下次上线自动清除）';
    forget.addEventListener('click', () => autoBackupMarkerAction(
      device.pending_clear ? 'offline_keep' : 'offline_unmark', { identity: device.identity }));
    actions.append(forget);
  } else if (device.read_only) {
    const note = document.createElement('small');
    note.className = 'empty';
    note.textContent = '只读挂载，无法打标；请先以可读写方式重新挂载这块盘。';
    actions.append(note);
  } else if (device.marked) {
    const addSub = document.createElement('button');
    addSub.type = 'button';
    addSub.textContent = '添加子目录打标…';
    addSub.addEventListener('click', () => openPathPicker('submark', { kind: 'path', path: device.mount_point }));
    const rename = document.createElement('button');
    rename.type = 'button';
    rename.textContent = '设置别名';
    rename.addEventListener('click', () => promptAutoBackupAlias(device));
    const unmark = document.createElement('button');
    unmark.type = 'button';
    unmark.className = 'danger';
    unmark.textContent = '取消打标（整块卷）';
    unmark.addEventListener('click', () => autoBackupMarkerAction('unmark', { path: device.mount_point }));
    actions.append(addSub, rename, unmark);
  } else {
    const mark = document.createElement('button');
    mark.type = 'button';
    mark.textContent = '给整块卷打标';
    mark.addEventListener('click', () => promptAutoBackupMark(device.mount_point));
    const markSub = document.createElement('button');
    markSub.type = 'button';
    markSub.textContent = '给子目录打标…';
    markSub.addEventListener('click', () => openPathPicker('submark', { kind: 'path', path: device.mount_point }));
    actions.append(mark, markSub);
  }
  // 内置卷没有任何可执行动作，空行不占位
  if (actions.childElementCount) item.append(actions);
  return item;
}

// autoBackupMarkerAction 调打标接口：成功后直接用后端回传的设备列表重绘，
// 失败时把后端的原话（只读 / 没权限 / 空间不足…）显示出来并重新拉一次列表。
async function autoBackupMarkerAction(action, payload = {}) {
  const pending = {
    mark: '正在打标…',
    sub_add: '正在给子目录打标…',
    sub_remove: '正在取消子目录打标…',
    unmark: '正在取消打标…',
    offline_unmark: '正在登记「下次上线清除」…',
    offline_keep: '正在撤销…',
    rename: '正在保存别名…',
  }[action] || '正在处理…';
  showAutoBackupStatus(pending);
  try {
    const result = await request('api/backup/auto/marker', {
      method: 'POST',
      body: JSON.stringify({ action, ...payload }),
    });
    if (Array.isArray(result.devices)) {
      autoDevices = result.devices;
      renderAutoBackupDevices();
    } else {
      await refreshAutoBackupDevices();
    }
    showAutoBackupStatus(result.message || '已更新打标状态。');
  } catch (error) {
    showAutoBackupStatus(`打标失败：${error.message}`, true);
    await refreshAutoBackupDevices();
  }
}

async function promptAutoBackupMark(path, displayName = '') {
  const alias = await showAppConfirm({
    title: displayName ? `给 ${displayName} 打标` : '给整块卷打标',
    message: `打标信息写在卷内的 ${autoMarkerDirFile}（不会往卷根丢文件），整块盘都会参与自动备份。只备份卡里某几个目录时，改用「给子目录打标…」。`,
    input: {
      label: '别名（可选，列表里一眼认出是哪块盘）',
      value: '',
      placeholder: '例如：相机SD卡',
      maxLength: 32,
    },
    confirmText: '打标',
  });
  if (alias === null || alias === undefined) return;
  await autoBackupMarkerAction('mark', { path, alias });
}

async function promptAutoBackupAlias(device) {
  const alias = await showAppConfirm({
    title: `设置 ${autoDeviceName(device)} 的别名`,
    message: '别名只用于在列表与日志里区分设备，留空则回落到卷标。',
    input: {
      label: '别名（留空 = 用卷标）',
      value: device.alias || '',
      placeholder: '例如：相机SD卡',
      maxLength: 32,
    },
    confirmText: '保存',
  });
  if (alias === null || alias === undefined) return;
  await autoBackupMarkerAction('rename', { path: device.mount_point, alias });
}

// --- 规则编辑器 -------------------------------------------------------------

function autoDirectionFromEditor() {
  const checked = document.querySelector('input[name="auto-backup-direction"]:checked');
  return checked ? checked.value : 'from_volume';
}

function autoModeFromEditor() {
  const checked = document.querySelector('input[name="auto-backup-mode"]:checked');
  return checked ? checked.value : 'archive';
}

// setAutoMoveConfirmAlert 打开/收起确认项的告警高亮（配合 styles.css 里的 .needs-confirm）。
function setAutoMoveConfirmAlert(on) {
  const next = on === true;
  if (autoMoveConfirmAlert === next) return;
  autoMoveConfirmAlert = next;
  $('auto-backup-allow-move-field').classList.toggle('needs-confirm', next);
}

// pulseAutoBackupConfirm 用户没勾确认就去保存/试跑时抖一下那一行，并把焦点送过去。
function pulseAutoBackupConfirm() {
  const field = $('auto-backup-allow-move-field');
  setAutoMoveConfirmAlert(true);
  // 连续点两次也要重新播放动画：先去掉类、读一次布局强制回流，再加回去。
  field.classList.remove('shake');
  void field.offsetWidth;
  field.classList.add('shake');
  setTimeout(() => field.classList.remove('shake'), 700);
  if (typeof field.scrollIntoView === 'function') field.scrollIntoView({ block: 'nearest' });
  const input = $('auto-backup-allow-move');
  if (typeof input.focus === 'function') input.focus({ preventScroll: true });
}

function updateAutoBackupModeControls() {
  const direction = autoDirectionFromEditor();
  const toVolume = direction === 'to_volume';
  const effectiveMode = autoModeFromEditor();
  const effectiveMove = effectiveMode === 'move_overwrite' || effectiveMode === 'move_archive';
  // 移动类模式两个方向都能用，但「NAS → 打标卷」的源是 NAS 上的原始资料，删源不可逆，
  // 而且按键触发时没有界面能弹窗——所以这条规则必须逐条勾选确认（后端同样校验），
  // 页面上手动运行时还会再弹一次确认框。
  const riskyMove = effectiveMove && toVolume;
  $('auto-backup-allow-move-field').hidden = !riskyMove;
  if (!riskyMove) {
    $('auto-backup-allow-move').checked = false;
    setAutoMoveConfirmAlert(false);
  }
  $('auto-backup-overwrite').disabled = effectiveMode !== 'copy';
  $('auto-backup-keep').disabled = effectiveMode !== 'archive' && effectiveMode !== 'move_archive';
  syncBackupKeepRow('auto-backup-keep-row', backupKeepAnchorID(effectiveMode, 'auto-backup'));
  $('auto-backup-fixed-label').textContent = toVolume
    ? 'NAS 侧源（要备份的目录）'
    : 'NAS 侧目标（导入到哪里）';
  // 「打标卷 → NAS」方向：卷内范围由打标时标记的子目录决定，规则的子目录输入框
  // 隐藏（老配置里填过的值后端仍然尊重，见 autoBackupJobForVolume）。
  $('auto-backup-sub-field').hidden = !toVolume;
  $('auto-backup-sub-hint').hidden = toVolume;
  const hint = $('auto-backup-mode-hint');
  hint.className = riskyMove ? 'backup-hint danger' : 'backup-hint';
  if (riskyMove) {
    hint.textContent = '注意：移动模式会删掉源里的文件，这条规则的源在 NAS 上——复制成功一个删一个，跑完 NAS 上就只剩没传成功的文件。同一时间只能插一块打标盘，多块盘同时插着时这条规则会被拒绝运行。';
  } else if (effectiveMove) {
    hint.textContent = '移动模式复制成功一个删一个，只删已成功复制的文件；打标子目录模式下也只删这些子目录里的文件。';
  } else if (effectiveMode === 'sync' && toVolume && !$('auto-backup-sub').value.trim()) {
    hint.textContent = '注意：同步模式 + 留空子目录 = 清空整块打标卷，保存时会被拒绝；请填写一个子目录。';
  } else {
    hint.textContent = '';
  }
}

function refreshAutoBackupFixedHints() {
  const hint = $('auto-backup-fixed-hint');
  const location = autoEditorLocations.fixed;
  if (!location) {
    hint.textContent = '';
    return;
  }
  if (location.kind === 'volume') {
    hint.textContent = '按卷标/UUID 记住介质本身，换接口或换挂载路径后仍能自动找到。';
    return;
  }
  hint.textContent = '';
}

function setAutoBackupFixedLocation(location) {
  autoEditorLocations.fixed = location;
  $('auto-backup-fixed').value = backupLocationSummary(location) || '';
  refreshAutoBackupFixedHints();
}

function openAutoBackupEditor(ruleID = '') {
  const rule = gpioAutos.find((item) => item.id === ruleID);
  autoEditingRuleID = rule ? rule.id : generateAutoBackupRuleID();
  $('auto-backup-id').value = autoEditingRuleID;
  $('auto-backup-name').value = rule ? rule.name : '';
  $('auto-backup-sub').value = rule ? (rule.volume_sub || '') : '';
  $('auto-backup-keep').value = rule ? String(rule.keep || 0) : '0';
  $('auto-backup-overwrite').checked = rule ? rule.overwrite !== false : true;
  $('auto-backup-allow-move').checked = rule ? rule.allow_move_to_volume === true : false;
  document.querySelectorAll('input[name="auto-backup-direction"]').forEach((radio) => {
    radio.checked = radio.value === (rule ? rule.direction : 'from_volume');
  });
  document.querySelectorAll('input[name="auto-backup-mode"]').forEach((radio) => {
    radio.checked = radio.value === (rule ? rule.mode : 'archive');
  });
  autoEditorLocations.fixed = rule ? rule.fixed : null;
  $('auto-backup-fixed').value = backupLocationSummary(autoEditorLocations.fixed);
  $('auto-backup-editor').hidden = false;
  updateAutoBackupModeControls();
  setAutoMoveConfirmAlert(false);
  refreshAutoBackupFixedHints();
  showAutoBackupEditorMessage(rule ? `正在编辑「${rule.name}」` : '正在创建新的自动备份规则');
  $('auto-backup-name').focus();
}

function closeAutoBackupEditor() {
  autoEditingRuleID = '';
  autoEditorLocations.fixed = null;
  $('auto-backup-editor').hidden = true;
  $('auto-backup-id').value = '';
  $('auto-backup-name').value = '';
  $('auto-backup-fixed').value = '';
  $('auto-backup-sub').value = '';
  $('auto-backup-keep').value = '0';
  $('auto-backup-overwrite').checked = true;
  $('auto-backup-allow-move').checked = false;
  setAutoMoveConfirmAlert(false);
  showAutoBackupEditorMessage('');
}

function autoRuleFromEditor() {
  const mode = autoModeFromEditor();
  const direction = autoDirectionFromEditor();
  const keepEnabled = mode === 'archive' || mode === 'move_archive';
  const moveMode = mode === 'move_overwrite' || mode === 'move_archive';
  return {
    id: autoEditingRuleID || generateAutoBackupRuleID(),
    name: $('auto-backup-name').value.trim(),
    direction,
    fixed: autoEditorLocations.fixed,
    // 卷内子目录只在「NAS → 打标卷」方向有意义；「打标卷 → NAS」的范围由打标时
    // 标记的子目录决定。这里必须清空，否则残留值会在后端压过打标子目录。
    volume_sub: direction === 'to_volume' ? $('auto-backup-sub').value.trim() : '',
    mode,
    overwrite: mode === 'copy' ? $('auto-backup-overwrite').checked : true,
    keep: keepEnabled ? Math.max(0, Math.round(Number($('auto-backup-keep').value) || 0)) : 0,
    // 只有「NAS → 打标卷 + 移动类」这个组合需要显式确认，其它情况一律不带这个标记。
    allow_move_to_volume: direction === 'to_volume' && moveMode ? $('auto-backup-allow-move').checked : false,
  };
}

// autoRuleDeletesNasSource 规则会不会删掉 NAS 上的文件（也就是需要额外确认的那一类）。
function autoRuleDeletesNasSource(rule) {
  if (!rule || rule.direction !== 'to_volume') return false;
  return rule.mode === 'move_overwrite' || rule.mode === 'move_archive';
}

// 卷内子目录清理后是否等于卷根本身：空、"."、"./"、"././" 都算。这类输入等于
// 「没选子目录」，同步模式会带 --delete 清空整块盘，后端也会拒绝。
function autoVolumeSubIsVolumeRoot(sub) {
  return String(sub || '').trim().split('/').every((part) => part === '' || part === '.');
}

function validateAutoRuleDraft(rule) {
  if (!rule.name) {
    showAutoBackupEditorMessage('规则名称不能为空。', true);
    $('auto-backup-name').focus();
    return false;
  }
  if (gpioAutos.some((item) => item.id !== rule.id && item.name.trim().toLowerCase() === rule.name.toLowerCase())) {
    showAutoBackupEditorMessage(`规则名称“${rule.name}”已存在。`, true);
    $('auto-backup-name').focus();
    return false;
  }
  if (!rule.fixed) {
    showAutoBackupEditorMessage('请先选择 NAS 侧位置。', true);
    return false;
  }
  // from_volume（打标卷 → NAS）时 rule.fixed 才是 NAS 侧的目标；to_volume 时它是
  // NAS 侧的源（见 backup_auto.go 里 Fixed 字段的说明）。目标直接落在整个「我的
  // 文件」根目录上，同步模式会把个人空间清空，必须在保存前拦住。
  const target = rule.direction === 'from_volume' ? rule.fixed : null;
  if (target && target.kind === 'path' && /^\/vol[0-9]+\/[0-9]+\/?$/.test(target.path)) {
    showAutoBackupEditorMessage('NAS 侧目标不能直接是整个「我的文件」根目录，请在它下面新建一个备份目录。', true);
    return false;
  }
  // 卷内子目录按「清理后的路径」判断：空、"."、"./"、"././" 都落在卷根。
  // 同步模式会 --delete 清空整块盘；其它模式下点目录也等于把规则作用到整卷，
  // 与用户填了子目录的预期不符，一起拦住（后端规则校验与执行入口有同样的闸）。
  if (rule.direction === 'to_volume' && autoVolumeSubIsVolumeRoot(rule.volume_sub)) {
    if (rule.mode === 'sync') {
      showAutoBackupEditorMessage('同步模式请填写打标卷内的目标子目录（留空、. 或 ./ 都会清空整块盘）。', true);
      return false;
    }
    if (rule.volume_sub) {
      showAutoBackupEditorMessage('卷内子目录不能是 . 或 ./（等同于整块盘的根目录）。', true);
      return false;
    }
  }
  if (autoRuleDeletesNasSource(rule) && !rule.allow_move_to_volume) {
    // 只说不做没用：抖一下那一行、把焦点送过去，让用户知道要勾的是哪里。
    pulseAutoBackupConfirm();
    showAutoBackupEditorMessage('还没有确认风险：这条规则会删除 NAS 上的源文件，请勾选下方的「我确认：这条规则可以删除 NAS 上的源文件」再保存。', true);
    return false;
  }
  return true;
}

function commitAutoBackupEditor() {
  if ($('auto-backup-editor').hidden) return true;
  const rule = autoRuleFromEditor();
  if (!validateAutoRuleDraft(rule)) return false;
  const index = gpioAutos.findIndex((item) => item.id === rule.id);
  if (index >= 0) gpioAutos[index] = rule;
  else gpioAutos.push(rule);
  closeAutoBackupEditor();
  setupGPIOActions(gpioScripts, true, gpioBackups, gpioAutos);
  renderAutoBackupList();
  showAutoBackupStatus(`规则“${rule.name}”已加入草稿。别忘了点击下方的「保存按钮控制」持久化。`);
  return true;
}

async function deleteAutoBackupRule(ruleID) {
  const rule = gpioAutos.find((item) => item.id === ruleID);
  if (!rule) return;
  const bindings = autoBackupBindings(ruleID);
  if (bindings.length) {
    showAutoBackupStatus(`规则“${rule.name}”仍被按键（${bindings.join('、')}）引用，请先在上方下拉框改成其他动作。`, true);
    return;
  }
  const ok = await showAppConfirm({
    title: '删除自动备份规则',
    message: `确定删除自动备份规则“${rule.name}”吗？删除后仍需点击“保存按钮控制”才会持久化。`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok) return;
  gpioAutos = gpioAutos.filter((item) => item.id !== ruleID);
  if (autoEditingRuleID === ruleID) closeAutoBackupEditor();
  setupGPIOActions(gpioScripts, true, gpioBackups, gpioAutos);
  renderAutoBackupList();
  showAutoBackupStatus(`规则“${rule.name}”已从本页草稿删除。`);
}

async function runAutoBackup(ruleID, dryRun = false, definition = null) {
  const rule = definition || gpioAutos.find((item) => item.id === ruleID);
  const name = rule ? rule.name : ruleID;
  // 「NAS → 打标卷 + 移动类」会删掉 NAS 上的源文件：正式运行前再确认一次。
  // 按键触发那一路没有界面（保存规则时的勾选已经确认过一次），这里只覆盖页面上的运行按钮。
  // 试跑不写不删任何文件，不需要确认。
  if (!dryRun && autoRuleDeletesNasSource(rule)) {
    const confirmed = await showAppConfirm({
      title: '确认删除 NAS 上的源文件',
      message: `规则「${name}」用的是移动类模式：运行后会把 ${backupLocationSummary(rule.fixed)} 里已成功复制的文件从 NAS 上删除（复制成功一个删一个，目录结构保留，没传完的文件不删）。同时插着多块打标卷时这条规则会被拒绝运行。`,
      confirmText: '确认运行',
      danger: true,
    });
    if (!confirmed) {
      showAutoBackupStatus('已取消运行：移动模式会删除 NAS 上的源文件。');
      return;
    }
  }
  showAutoBackupStatus(dryRun ? `正在试跑「${name}」…` : `正在启动「${name}」…`);
  try {
    const payload = definition
      ? { rule: definition, dry_run: dryRun }
      : { rule_id: ruleID, dry_run: dryRun };
    const result = await request('api/backup/auto/run', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    if (dryRun) {
      showAutoBackupStatus(`试跑完成：找到 ${result.volumes} 块打标卷，将处理 ${result.files} 个文件（${formatByteSize(result.bytes)}）。均为演练，未写入任何文件。`);
    } else {
      showAutoBackupStatus(`「${name}」已开始自动备份，结束后会闪灯并滴滴滴提示。`);
      startBackupPolling();
    }
  } catch (error) {
    showAutoBackupStatus(`执行失败：${error.message}`, true);
  }
}

// --- 位置 / 目录选择对话框 ---------------------------------------------------
//
// 默认只让用户在飞牛的四类位置里选（我的文件 / 团队文件 / 外接存储 / 远程挂载），
// 不暴露系统目录，避免小白把备份目标指到乱七八糟的地方；确有需要时可用
// 「高级」模式浏览整个文件系统（有明确的风险提示与二次确认）。

async function openPathPicker(role, startLocation = null) {
  pathPicker.role = role;
  pathPicker.allowFile = role === 'source';
  pathPicker.selectedFile = '';
  pathPicker.scope = 'places';
  $('path-picker-title').textContent = role === 'source'
    ? '选择源目录或文件'
    : role === 'marker' ? '选择要打标的可移动卷（选卷内任意目录即可）'
      : role === 'submark' ? '选择要打标的卷内子目录（只操作它的内容）'
        : '选择目标目录';
  $('path-picker').hidden = false;
  const start = (role === 'marker' || role === 'submark') ? startLocation : backupEditorLocations[role];
  await browsePathTo(start && start.kind === 'path' ? start.path : '', 'places');
}

function closePathPicker() {
  $('path-picker').hidden = true;
  pathPicker.selectedFile = '';
}

async function browsePathTo(path, scope) {
  if (scope) pathPicker.scope = scope;
  const message = $('path-picker-message');
  message.textContent = '正在读取…';
  message.className = 'inline-status';
  try {
    const query = `path=${encodeURIComponent(path || '')}&scope=${encodeURIComponent(pathPicker.scope)}`;
    const data = await request(`api/backup/browse?${query}`);
    const places = Array.isArray(data.places) ? data.places : [];
    pathPicker.path = data.path || '';
    pathPicker.places = places;
    pathPicker.currentPlace = places.find((item) => item.id === data.inside) || null;
    pathPicker.selectedFile = '';
    renderPathPicker(data);
    if (data.mode !== 'browse') {
      message.textContent = places.length ? `共 ${places.length} 个可用位置` : '没有检测到可用位置，可用「高级」手动指定路径';
    } else {
      message.textContent = data.entries && data.entries.length ? `该目录下有 ${data.entries.length} 项` : '该目录为空';
    }
  } catch (error) {
    message.textContent = error.message;
    message.className = 'inline-status error';
  }
}

function renderPathPicker(data) {
  const placesMode = data.mode !== 'browse';
  renderPathPlaces(data.places || [], data.inside || '');
  renderPathCrumbs(data);
  renderPathEntries(data, placesMode);
  $('path-picker-up').disabled = !data.parent;
  $('path-picker-up').dataset.parent = data.parent || '';
  // 位置总览还没有「当前目录」，新建文件夹无从落脚。
  $('path-picker-mkdir').disabled = placesMode;
  const advanced = data.scope === 'all';
  document.querySelector('.path-picker-body').classList.toggle('advanced', advanced);
  const advancedButton = $('path-picker-advanced');
  advancedButton.textContent = advanced ? '退出原生目录模式' : '高级：原生目录模式';
  advancedButton.classList.toggle('active', advanced);
  const warning = $('path-picker-warning');
  warning.hidden = !advanced;
  if (advanced) {
    warning.textContent = '原生目录模式：正在浏览整个文件系统。系统目录不是数据盘，把备份目标放在这里可能损坏系统，请确认你清楚自己在做什么。';
  }
  updatePathPickerSelectLabel();
}

// 容量文字与硬盘测试工具完全一套：「63.1 GB 可用 36%」；网盘虚标容量
// （rclone 一律报 ~1 PiB）按「容量未知」处理。
const TIB = 1024 * 1024 * 1024 * 1024;
const FAKE_CAPACITY_MIN = 900 * TIB;
const CAPACITY_WHY = {
  cloud: '网盘挂载不上报可用空间（rclone / WebDAV 对网盘一律返回空容量），所以只能显示「容量未知」。',
  cache: '这个远程挂载走本地缓存，不上报可用空间，所以只能显示「容量未知」。',
  direct: '挂载端没有上报可用空间，所以显示「容量未知」。',
};

function isFakeCapacity(bytes) {
  return bytes >= FAKE_CAPACITY_MIN;
}

function placeCapacityText(place) {
  const free = place.free_bytes || 0;
  const total = place.total_bytes || 0;
  if (free > 0 && !isFakeCapacity(free)) {
    const pct = total > 0 ? Math.round(free / total * 100) : 0;
    return {
      text: `${formatByteSize(free)} 可用 ${pct}%`,
      title: `可用 ${formatByteSize(free)} / 共 ${formatByteSize(total)}`,
      unknown: false,
    };
  }
  if (place.kind === 'remote') {
    return { text: '容量未知', title: CAPACITY_WHY[place.mode] || CAPACITY_WHY.direct, unknown: true };
  }
  return { text: '—', title: '没能读到这个位置的容量信息。', unknown: true };
}

function placeMetaText(place) {
  const parts = [];
  if (place.subtitle) parts.push(place.subtitle);
  if (place.read_only) parts.push('只读');
  return parts.join(' · ');
}

function renderPathPlaces(places, insideID) {
  const list = $('path-picker-places');
  list.replaceChildren();
  if (!places.length) {
    const empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = '没有找到可用的存储卷';
    list.append(empty);
    return;
  }
  places.forEach((place) => {
    const li = document.createElement('li');
    li.className = `place-item${insideID && place.id === insideID ? ' active' : ''}${place.kind === 'external' ? ' removable' : ''}`;
    li.title = `${place.path}${place.read_only ? '（只读）' : ''}`;
    // 第一行：名称 + 剩余容量（右对齐），与硬盘测试工具的选目录列表同一套排法
    const row = document.createElement('div');
    row.className = 'pl-row';
    const name = document.createElement('span');
    name.className = 'pl-name';
    name.textContent = place.name;
    row.append(name);
    const cap = placeCapacityText(place);
    if (cap.text) {
      const free = document.createElement('span');
      free.className = `pl-free${cap.unknown ? ' unknown' : ''}`;
      free.textContent = cap.text;
      free.title = cap.title;
      row.append(free);
    }
    li.append(row);
    // 第二行：身份信息（用户名 / 可移动介质 / 远程挂载 · 只读），远程挂载右侧带改名
    const meta = placeMetaText(place);
    if (meta || place.kind === 'remote') {
      const subRow = document.createElement('div');
      subRow.className = 'pl-row sub';
      const sub = document.createElement('span');
      sub.className = 'pl-sub';
      sub.textContent = meta;
      sub.hidden = !meta;
      subRow.append(sub);
      if (place.kind === 'remote') {
        const rename = document.createElement('button');
        rename.type = 'button';
        rename.className = 'rename';
        rename.textContent = '改名';
        rename.title = '给这个远程挂载设置一个看得懂的名字（例如：夸克网盘）';
        rename.addEventListener('click', (event) => {
          event.stopPropagation();
          promptRemoteAlias(place);
        });
        subRow.append(rename);
      }
      li.append(subRow);
    }
    // 与硬盘测试工具一致：点存储空间直接进入卷内用户目录（home），不停在卷根
    li.addEventListener('click', () => browsePathTo(place.home || place.path, 'places'));
    // 键盘可达：Enter / 空格 触发与点击相同的行为（target 限定 li 本身，避免劫持内部改名按钮）
    li.tabIndex = 0;
    li.addEventListener('keydown', (event) => {
      if (event.target !== li || (event.key !== 'Enter' && event.key !== ' ')) return;
      event.preventDefault();
      li.click();
    });
    list.append(li);
  });
}

// 路径栏照搬硬盘测试工具：一条灰底横条里放当前完整路径（单行省略）。
function renderPathCrumbs(data) {
  const code = $('path-picker-path');
  const hint = $('path-picker-hint');
  const placesMode = data.mode !== 'browse';
  code.textContent = placesMode ? '从左侧选择存储位置' : (data.path || '/');
  code.title = placesMode ? '从左侧选择存储位置' : (data.path || '/');
  if (hint) {
    const place = (data.places || []).find((item) => item.id === data.inside);
    const readOnly = !placesMode && place && place.read_only;
    hint.textContent = readOnly ? '该位置只读' : '';
    hint.hidden = !readOnly;
  }
}

function renderPathEntries(data, placesMode) {
  const list = $('path-picker-entries');
  list.replaceChildren();
  if (placesMode) {
    const guide = document.createElement('li');
    guide.className = 'empty';
    guide.textContent = '请从左侧选择一个存储位置';
    list.append(guide);
    return;
  }
  const entries = data.entries || [];
  if (!entries.length) {
    const empty = document.createElement('li');
    empty.className = 'empty';
    empty.textContent = '（没有子目录）';
    list.append(empty);
    return;
  }
  entries.forEach((entry) => {
    const row = document.createElement('li');
    row.className = `path-entry${entry.is_dir ? ' is-dir' : ' is-file'}${entry.read_only ? ' locked' : ''}`;
    const name = document.createElement('span');
    name.className = 'path-entry-name';
    name.textContent = entry.name;
    row.append(name);
    if (!entry.is_dir) {
      const size = document.createElement('span');
      size.className = 'path-entry-meta';
      size.textContent = formatByteSize(entry.size);
      row.append(size);
    }
    if (entry.is_place) {
      const badge = document.createElement('span');
      badge.className = 'path-entry-badge';
      badge.textContent = '位置';
      row.append(badge);
    } else if (entry.is_volume) {
      const badge = document.createElement('span');
      badge.className = 'path-entry-badge';
      badge.textContent = entry.read_only ? '只读卷' : '卷';
      row.append(badge);
    }
    if (entry.read_only && entry.is_dir) row.title = '无写权限';
    row.addEventListener('click', () => {
      if (entry.is_dir) browsePathTo(entry.path, pathPicker.scope);
      else if (pathPicker.allowFile) selectPathFile(entry.path, row);
    });
    // 键盘可达：列表是 role="listbox"，每行作为 option 可聚焦，Enter / 空格 同点击
    row.tabIndex = 0;
    row.setAttribute('role', 'option');
    row.setAttribute('aria-selected', 'false');
    row.addEventListener('keydown', (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      row.click();
    });
    list.append(row);
  });
}

function selectPathFile(path, row) {
  pathPicker.selectedFile = path;
  document.querySelectorAll('.path-entry.selected').forEach((item) => {
    item.classList.remove('selected');
    item.setAttribute('aria-selected', 'false');
  });
  row.classList.add('selected');
  row.setAttribute('aria-selected', 'true');
  updatePathPickerSelectLabel();
}

function updatePathPickerSelectLabel() {
  const button = $('path-picker-select');
  if (!pathPicker.path) {
    button.disabled = true;
    button.textContent = pathPicker.role === 'source' ? '使用当前目录作为源' : '使用当前目录作为目标';
    return;
  }
  button.disabled = false;
  if (!pathPicker.allowFile || !pathPicker.selectedFile) {
    button.textContent = pathPicker.role === 'source' ? '使用当前目录作为源' : '使用当前目录作为目标';
    return;
  }
  button.textContent = `选择文件：${pathPicker.selectedFile.split('/').pop()}`;
}

// promptCreateDirectory 在当前浏览目录下新建一个子文件夹，建完直接进去。
// 只建一层且由后端校验（位置范围 / 只读卷 / 重名 / 非法名字），前端只做提示。
async function promptCreateDirectory() {
  const message = $('path-picker-message');
  if (!pathPicker.path) {
    message.textContent = '请先进入一个目录，再新建文件夹。';
    message.className = 'inline-status error';
    return;
  }
  const name = await showAppConfirm({
    title: '新建文件夹',
    message: `将在 ${pathPicker.path} 下新建一个文件夹。`,
    input: {
      label: '文件夹名称',
      value: '',
      placeholder: '例如：备份',
      maxLength: 40,
    },
    confirmText: '创建',
  });
  const trimmed = String(name === null || name === undefined ? '' : name).trim();
  if (!trimmed) return;
  message.textContent = '正在创建…';
  message.className = 'inline-status';
  try {
    const result = await request('api/backup/mkdir', {
      method: 'POST',
      body: JSON.stringify({
        path: pathPicker.path,
        name: trimmed,
        scope: pathPicker.scope || 'places',
      }),
    });
    const created = (result && result.path) || '';
    // 建完直接进去：用户点「选择当前目录」就是它，少一次点击。
    await browsePathTo(created, pathPicker.scope || 'places');
    message.textContent = `已新建文件夹「${(result && result.name) || trimmed}」`;
    message.className = 'inline-status';
  } catch (error) {
    message.textContent = error.message;
    message.className = 'inline-status error';
  }
}

// --- 内置确认 / 输入对话框 --------------------------------------------------
// 替代 window.confirm：浏览器原生弹窗与应用风格不搭，也没法做警示美化。
// 用法：const ok = await showAppConfirm({ title, message, confirmText, danger });
// 输入模式再传 input: { label, value, placeholder, maxLength }，确定时回传输入值。
// 需要第三个动作时传 secondaryText，点它返回字符串 'secondary'。
// 风扇曲线的模式弹窗靠这个多出「应用并保存风扇控制」。
// hideConfirm 把主按钮藏起来（只留副按钮当唯一动作），secondaryDanger 让副按钮走红底。

const appConfirm = { onOk: null, resolve: null };

function showAppConfirm(options = {}) {
  return new Promise((resolve) => {
    appConfirm.onOk = typeof options.onOk === 'function' ? options.onOk : null;
    appConfirm.resolve = resolve;
    $('confirm-title').textContent = options.title || '确认操作';
    const message = $('confirm-message');
    message.textContent = options.message || '';
    message.hidden = !options.message;
    const ok = $('confirm-ok');
    ok.textContent = options.confirmText || '确定';
    ok.className = options.danger ? 'danger' : 'primary';
    ok.hidden = Boolean(options.hideConfirm);
    $('confirm-icon').hidden = !options.danger;
    const secondary = $('confirm-secondary');
    secondary.hidden = !options.secondaryText;
    secondary.textContent = options.secondaryText || '';
    secondary.className = `confirm-secondary${options.secondaryDanger ? ' danger' : ''}`;
    const inputRow = $('confirm-input-row');
    inputRow.hidden = !options.input;
    if (options.input) {
      $('confirm-input-label').textContent = options.input.label || '';
      const input = $('confirm-input');
      input.value = options.input.value || '';
      input.placeholder = options.input.placeholder || '';
      input.maxLength = options.input.maxLength || 64;
    }
    $('app-confirm').hidden = false;
    if (options.input) {
      const input = $('confirm-input');
      input.focus();
      input.select();
    }
  });
}

function closeAppConfirm(result) {
  if ($('app-confirm').hidden) return;
  $('app-confirm').hidden = true;
  if (appConfirm.resolve) {
    appConfirm.resolve(result);
    appConfirm.resolve = null;
  }
}

$('confirm-cancel').addEventListener('click', () => closeAppConfirm(null));
document.querySelectorAll('[data-confirm-close]').forEach((el) => {
  el.addEventListener('click', () => closeAppConfirm(null));
});
// 副按钮（「应用并保存风扇控制」）回传 { secondary: true, value }，
// value 带上输入框内容，手动填百分比时点它也能用用户填的数
$('confirm-secondary').addEventListener('click', () => {
  const inputMode = !$('confirm-input-row').hidden;
  closeAppConfirm({ secondary: true, value: inputMode ? $('confirm-input').value.trim() : undefined });
});
$('confirm-ok').addEventListener('click', () => {
  const onOk = appConfirm.onOk;
  const inputMode = !$('confirm-input-row').hidden;
  const value = inputMode ? $('confirm-input').value.trim() : undefined;
  closeAppConfirm(inputMode ? value : true);
  if (onOk) onOk(value);
});
$('confirm-input').addEventListener('keydown', (event) => {
  if (event.key === 'Enter') {
    event.preventDefault();
    // 主按钮被藏起来的弹窗（关停只有一个红色动作）里，回车走副按钮
    if ($('confirm-ok').hidden && !$('confirm-secondary').hidden) $('confirm-secondary').click();
    else $('confirm-ok').click();
  }
});

async function togglePathPickerAdvanced() {
  if (pathPicker.scope === 'all') {
    await browsePathTo('', 'places');
    return;
  }
  const ok = await showAppConfirm({
    title: '进入原生目录模式',
    message: '该模式可以浏览并选择整个文件系统（包含系统目录）。系统目录不是数据盘，把备份目标放在这里可能损坏系统，请确认你清楚自己在做什么。',
    confirmText: '进入原生目录模式',
    danger: true,
  });
  if (!ok) return;
  await browsePathTo('/', 'all');
}

// --- 远程挂载显示名 ---------------------------------------------------------

async function promptRemoteAlias(place) {
  const defaultName = place.path.split('/').pop();
  const isDefault = place.name === defaultName;
  const value = await showAppConfirm({
    title: '设置远程挂载显示名',
    message: `${place.path}\n当前显示：${isDefault ? `${defaultName}（系统自动命名）` : place.name}`,
    input: {
      label: '显示名（留空恢复系统命名）',
      value: isDefault ? '' : place.name,
      placeholder: '例如：夸克网盘',
      maxLength: 64,
    },
    confirmText: '保存',
  });
  if (value === null || value === undefined) return;
  const message = $('path-picker-message');
  try {
    await request('api/backup/alias', {
      method: 'POST',
      body: JSON.stringify({ mount_point: place.path, name: value }),
    });
    message.textContent = value ? `已改名为「${value}」` : '已恢复系统命名';
    message.className = 'inline-status';
    await browsePathTo(pathPicker.path || '', pathPicker.scope || 'places');
  } catch (error) {
    message.textContent = error.message;
    message.className = 'inline-status error';
  }
}

// backupLocationForChosenPath 选到外接存储里的路径时，按卷标/UUID 记录介质本身，
// 再加上卷内子目录；这样 U 盘换接口、换挂载路径后任务依然能自动找到。
function backupLocationForChosenPath(path) {
  const place = pathPicker.currentPlace;
  if (place && place.kind === 'external' && place.uuid) {
    const root = place.path.replace(/\/+$/, '');
    let sub = '';
    if (path !== root && path.startsWith(root + '/')) sub = path.slice(root.length + 1);
    return { kind: 'volume', uuid: place.uuid, label: place.label || '', sub };
  }
  return { kind: 'path', path };
}

function confirmPathPicker() {
  const chosen = pathPicker.selectedFile || pathPicker.path;
  if (!chosen) return;
  if ((pathPicker.role === 'target' || pathPicker.role === 'fixed') && pathPicker.selectedFile) return;
  if (pathPicker.role === 'marker') {
    // 打标不经过草稿：选完先问别名，再把打标信息写进该卷的打标目录。
    closePathPicker();
    promptAutoBackupMark(chosen);
    return;
  }
  if (pathPicker.role === 'submark') {
    // 子目录打标：选中的目录成为这块盘的「操作范围」之一。
    closePathPicker();
    autoBackupMarkerAction('sub_add', { path: chosen });
    return;
  }
  if (pathPicker.role === 'fixed') {
    setAutoBackupFixedLocation(backupLocationForChosenPath(chosen));
    closePathPicker();
    return;
  }
  setBackupEditorLocation(pathPicker.role, backupLocationForChosenPath(chosen));
  closePathPicker();
}

// --- 可移动卷选择 -----------------------------------------------------------

async function openVolumePicker(role) {
  volumePicker.role = role;
  volumePicker.selected = null;
  $('volume-picker-sub').value = '';
  $('volume-picker').hidden = false;
  await loadVolumeList();
}

function closeVolumePicker() {
  $('volume-picker').hidden = true;
  volumePicker.selected = null;
}

async function loadVolumeList() {
  const list = $('volume-picker-list');
  const message = $('volume-picker-message');
  list.replaceChildren();
  message.textContent = '正在读取…';
  message.className = 'inline-status';
  try {
    const data = await request('api/backup/volumes');
    volumePicker.volumes = Array.isArray(data.volumes) ? data.volumes : [];
    renderVolumeList();
    message.textContent = volumePicker.volumes.length
      ? `共 ${volumePicker.volumes.length} 个卷，点击选择`
      : '没有检测到可用卷：请先插好设备并等它挂载完成';
  } catch (error) {
    message.textContent = error.message;
    message.className = 'inline-status error';
  }
}

function renderVolumeList() {
  const list = $('volume-picker-list');
  list.replaceChildren();
  if (!volumePicker.volumes.length) {
    const empty = document.createElement('p');
    empty.className = 'gpio-script-empty';
    empty.textContent = '（没有可用卷）';
    list.append(empty);
    return;
  }
  volumePicker.volumes.forEach((volume) => {
    const row = document.createElement('button');
    row.type = 'button';
    row.className = `path-entry is-dir${volume.removable ? ' is-volume' : ''}`;
    const icon = document.createElement('span');
    icon.className = 'path-entry-icon';
    const name = document.createElement('span');
    name.className = 'path-entry-name';
    name.textContent = volume.label || volume.mount_point;
    const meta = document.createElement('span');
    meta.className = 'path-entry-meta';
    meta.textContent = [
      volume.mount_point,
      volume.uuid ? `UUID ${volume.uuid}` : '',
      volume.total_bytes ? `可用 ${formatByteSize(volume.free_bytes)} / ${formatByteSize(volume.total_bytes)}` : '',
    ].filter(Boolean).join(' · ');
    row.append(icon, name, meta);
    if (volume.removable) {
      const badge = document.createElement('span');
      badge.className = 'path-entry-badge';
      badge.textContent = '可移动';
      row.append(badge);
    }
    row.addEventListener('click', () => {
      volumePicker.selected = volume;
      document.querySelectorAll('#volume-picker-list .path-entry.selected').forEach((item) => item.classList.remove('selected'));
      row.classList.add('selected');
      $('volume-picker-message').textContent = `已选择：${volume.label || volume.mount_point}`;
    });
    list.append(row);
  });
}

function confirmVolumePicker() {
  if (!volumePicker.selected) {
    $('volume-picker-message').textContent = '请先点击上方列表选择一个卷';
    $('volume-picker-message').className = 'inline-status error';
    return;
  }
  const volume = volumePicker.selected;
  setBackupEditorLocation(volumePicker.role, {
    kind: 'volume',
    uuid: volume.uuid || '',
    label: volume.label || '',
    sub: $('volume-picker-sub').value.trim(),
  });
  closeVolumePicker();
}

// --- 运行与状态 -------------------------------------------------------------

async function runBackupJob(jobID, dryRun = false, definition = null) {
  const job = definition || gpioBackups.find((item) => item.id === jobID);
  const name = job ? job.name : jobID;
  showBackupStatus(dryRun ? `正在试跑「${name}」…` : `正在启动「${name}」…`);
  try {
    const payload = definition
      ? { job: definition, dry_run: dryRun }
      : { job_id: jobID, dry_run: dryRun };
    const result = await request('api/backup/run', {
      method: 'POST',
      body: JSON.stringify(payload),
    });
    if (dryRun) {
      showBackupStatus(`试跑完成：将复制 ${result.files} 个文件（${formatByteSize(result.bytes)}）`
        + `${result.deleted ? `，将删除 ${result.deleted} 项` : ''}。均为演练，未写入任何文件。`);
    } else {
      showBackupStatus(`「${name}」已开始备份，结束后会闪灯并滴滴滴提示。`);
      startBackupPolling();
    }
  } catch (error) {
    showBackupStatus(`执行失败：${error.message}`, true);
  }
}

function startBackupPolling() {
  clearTimeout(backupPollTimer);
  const tick = async () => {
    try {
      const status = await request('api/backup/status');
      renderBackupStatus(status);
      if (status.running) backupPollTimer = setTimeout(tick, 2000);
    } catch (error) {
      // 轮询失败不打扰用户：下一轮继续
    }
  };
  backupPollTimer = setTimeout(tick, 1500);
}

function renderBackupStatus(status) {
  if (!status) return;
  renderAutoBackupStatusSummary(status);
  if (status.running) {
    showBackupStatus(`正在备份「${status.job_name}」…（${formatClock(status.started_at)} 开始，完成后会闪灯 + 滴滴滴）`);
    return;
  }
  if (status.last_error) {
    showBackupStatus(`上次备份失败：${status.last_error}`, true);
    return;
  }
  const last = status.last;
  if (!last) return;
  showBackupStatus([
    `上次备份完成：${last.job_name}`,
    BACKUP_MODE_LABELS[last.mode] || last.mode,
    `复制 ${last.files} 个文件 / ${formatByteSize(last.bytes)}`,
    last.deleted ? `删除 ${last.deleted} 项` : '',
    last.pruned ? `清理 ${last.pruned} 份旧快照` : '',
    last.snapshot ? `快照 ${String(last.snapshot).split('/').pop()}` : '',
    `用时 ${formatDuration(last.duration_ms)}`,
    last.warning ? `注意：${last.warning}` : '',
  ].filter(Boolean).join(' · '), Boolean(last.warning));
}

// renderAutoBackupStatusSummary 把自动备份的汇总（含失败时逐卷结果）渲染到
// 自动备份卡片的状态行；普通任务的结果不会出现在这里（互斥存储）。
function renderAutoBackupStatusSummary(status) {
  const target = $('auto-backup-status');
  if (!target) return;
  const auto = status.auto;
  if (!auto) {
    target.textContent = '';
    target.className = 'inline-status';
    return;
  }
  const failedVolumes = (Array.isArray(auto.results) ? auto.results : [])
    .filter((item) => item.warning).length;
  const summary = [
    `上次自动备份「${auto.rule_name}」${auto.dry_run ? '（试跑）' : ''}：`,
    `${auto.volumes} 块打标卷`,
    `处理 ${auto.files} 个文件（${formatByteSize(auto.bytes)}）`,
    failedVolumes ? `${failedVolumes} 块卷有告警` : '',
  ].filter(Boolean).join(' · ');
  const error = Boolean(status.last_error);
  target.textContent = error ? `${summary}；失败原因：${status.last_error}` : summary;
  target.className = `inline-status${error ? ' error' : ''}`;
}

async function refreshBackupStatus() {
  try {
    renderBackupStatus(await request('api/backup/status'));
  } catch (error) {
    // 状态接口不可用时保持静默
  }
}

// ---- 固件升级：BIOS（Intel FPT）与网卡（Mellanox MFT） ----
// 刷写是后台任务，页面只轮询 api/firmware 拿任务步骤与日志；GET 返回缓存，
// 不会反复触发 lspci/flint 等外部命令。
const FIRMWARE_POLL_MS = 2000;
const STRESS_POLL_MS = 1000;
const NIC_TARGET_FW = '14.32.1900';
let firmwarePollTimer = null;
let stressPollTimer = null;

async function postForm(path, formData) {
  // multipart 不能手动设 Content-Type：浏览器需要自己带 boundary
  const response = await fetch(baseUrl(path), { method: 'POST', cache: 'no-store', body: formData });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error((body && body.error) || `HTTP ${response.status}`);
  // 2xx 但响应体损坏时结果不确定（刷写类操作尤其不能当成成功）
  if (body === null) throw new Error(`响应体解析失败（HTTP ${response.status}）`);
  return body;
}

function firmwareFile(inputID) {
  const input = $(inputID);
  const files = input && input.files;
  return files && files.length ? files[0] : null;
}

// 手动升级 BIOS 默认折叠：只有显式打开开关才显示本地刷写入口。
// 在线刷写进行中会自动展开，否则进度日志被藏在折叠区里看不见。
function setManualBIOSVisible(visible) {
  const card = document.querySelector('.firmware-bios-manual-card');
  const body = $('firmware-bios-manual-body');
  const toggle = $('firmware-bios-manual-toggle');
  if (toggle) toggle.checked = Boolean(visible);
  if (card) card.hidden = !visible;
  if (body) body.hidden = !visible;
}

function firmwareLogText(job) {
  const logs = job && Array.isArray(job.logs) ? job.logs : [];
  return logs.length ? logs.join('\n') : '等待操作…';
}

function showFirmwareLog(kind, message, error = false) {
  const target = $(kind === 'nic' ? 'firmware-nic-log' : 'firmware-bios-log');
  if (!target) return;
  target.textContent = message;
  target.className = `firmware-log${error ? ' error' : ''}`;
}

function showFirmwareHint(message, error = false) {
  const hint = $('firmware-nic-hint');
  if (!hint) return;
  hint.textContent = message;
  hint.className = `inline-status${error ? ' error' : ''}`;
}

function renderFirmwareJob(job) {
  if (!job) return;
  const target = $(job.kind === 'nic' ? 'firmware-nic-log' : 'firmware-bios-log');
  if (!target) return;
  const state = job.state || 'idle';
  // 在线升级的日志也在手动升级卡片里：刷写期间自动展开，避免进度被折叠隐藏
  if (job.kind !== 'nic' && state === 'running') setManualBIOSVisible(true);
  target.textContent = firmwareLogText(job);
  target.className = `firmware-log${state === 'success' ? ' success' : state === 'failed' ? ' error' : ''}`;
  const running = state === 'running';
  ['firmware-fpt-upload', 'firmware-bios-flash', 'firmware-nic-check', 'firmware-nic-flash', 'firmware-nic-flash-force', 'firmware-nic-mft-install', 'bios-online-check'].forEach((id) => {
    const button = $(id);
    if (!button) return;
    // 网卡升级键的 disabled 归 renderFirmwareStatus 的同版本逻辑管，空闲时不得强制放开
    if (!running && id === 'firmware-nic-flash') return;
    button.disabled = running || (id === 'bios-online-check' && biosOnlinePollTimer !== null);
  });
  if (running) {
    const onlineFlash = $('bios-online-flash');
    if (onlineFlash) onlineFlash.disabled = true;
  } else {
    updateBIOSOnlineFlashButton();
  }
}

function renderFirmwareStatus(status) {
  if (!status) return;
  const fpt = status.fpt || {};
  const flashrom = status.flashrom || {};
  const badge = $('firmware-tool-badge');
  if (badge) {
    if (flashrom.present) {
      badge.textContent = 'flashrom 可用';
      badge.className = 'inline-status';
      badge.title = flashrom.path || 'flashrom';
    } else if (fpt.present && fpt.builtin) {
      badge.textContent = 'FPT 内置';
      badge.className = 'inline-status';
      badge.title = [fpt.path, fpt.size ? formatByteSize(fpt.size) : ''].filter(Boolean).join(' · ');
    } else if (fpt.present) {
      badge.textContent = '自定义 FPT';
      badge.className = 'inline-status';
      badge.title = [fpt.path, fpt.size ? formatByteSize(fpt.size) : '', fpt.uploaded_at ? formatClock(fpt.uploaded_at) : '']
        .filter(Boolean).join(' · ');
    } else {
      badge.textContent = '刷写工具缺失';
      badge.className = 'inline-status error';
      badge.title = '未找到 flashrom，也未上传 Linux 版 FPT';
    }
  }
  const nic = status.nic || {};
  const setText = (id, value) => {
    const node = $(id);
    if (node) node.textContent = value || '—';
  };
  setText('firmware-nic-model', nic.model);
  setText('firmware-nic-pci', nic.pci_address);
  setText('firmware-nic-fw', nic.fw_version);
  setText('firmware-nic-psid', nic.psid);
  setText('firmware-nic-mft', nic.mft_installed ? '已安装' : '未安装（可手动安装，升级时也会自动安装）');
  setText('firmware-nic-target', nic.target_fw || NIC_TARGET_FW);
  const flash = $('firmware-nic-flash');
  const force = $('firmware-nic-flash-force');
  const mftBtn = $('firmware-nic-mft-install');
  // 三个操作按钮都只在检测到网卡之后才出现（2026-10-23 用户要求）
  if (!nic.present) {
    if (nic.error) {
      showFirmwareHint(`检测失败：${nic.error}`, true);
    } else {
      showFirmwareHint('尚未检测到 Mellanox 网卡，请点击「检测网卡」。');
    }
    if (flash) flash.hidden = true;
    if (force) force.hidden = true;
    if (mftBtn) mftBtn.hidden = true;
  } else {
    const hint = $('firmware-nic-hint');
    const currentFw = (nic.fw_version || '').trim();
    const targetFw = (nic.target_fw || NIC_TARGET_FW || '').trim();
    const sameVersion = !!(currentFw && targetFw && currentFw === targetFw);
    const mftMissing = !nic.mft_installed;
    if (hint) {
      if (mftMissing) {
        hint.textContent = `已检测到 ${nic.model || 'Mellanox 网卡'}，但未安装 MFT 工具：可先点「安装 MFT 工具」（自动从服务器下载安装），或直接开始升级。`;
        hint.className = 'inline-status';
      } else if (sameVersion) {
        hint.textContent = `已检测到 ${nic.model || 'Mellanox 网卡'}，当前固件 ${currentFw} 与在线版本一致不用升级。`;
        hint.className = 'inline-status';
      } else {
        hint.textContent = `已检测到 ${nic.model || 'Mellanox 网卡'}，当前固件 ${currentFw || '未知'}，可开始升级。`;
        hint.className = 'inline-status';
      }
    }
    if (mftBtn) mftBtn.hidden = !mftMissing;
    if (flash) { flash.hidden = false; flash.disabled = sameVersion; }
    if (force) force.hidden = !sameVersion;
  }
  renderFirmwareJob(status.job);
}

async function refreshFirmwareStatus(showError = false) {
  try {
    renderFirmwareStatus(await request('api/firmware'));
  } catch (error) {
    if (showError) showFirmwareHint(`读取固件状态失败：${error.message}`, true);
  }
}

function startFirmwarePoll() {
  if (firmwarePollTimer) return;
  firmwarePollTimer = setInterval(() => refreshFirmwareStatus(), FIRMWARE_POLL_MS);
}

function stopFirmwarePoll() {
  if (!firmwarePollTimer) return;
  clearInterval(firmwarePollTimer);
  firmwarePollTimer = null;
}

async function uploadFPT() {
  const file = firmwareFile('firmware-fpt-file');
  if (!file) {
    showFirmwareLog('bios', '请先选择 FPT 文件（Linux 版 fpt 可执行文件）。', true);
    return;
  }
  const button = $('firmware-fpt-upload');
  if (button) {
    button.disabled = true;
    button.textContent = '上传中…';
  }
  try {
    const form = new FormData();
    form.append('fpt', file);
    renderFirmwareStatus(await postForm('api/firmware/fpt', form));
    showFirmwareLog('bios', 'FPT 已上传，可以开始刷写 BIOS。');
  } catch (error) {
    showFirmwareLog('bios', `FPT 上传失败：${error.message}`, true);
  } finally {
    if (button) {
      button.disabled = false;
      button.textContent = '上传 FPT';
    }
  }
}

async function flashBIOS() {
  const file = firmwareFile('firmware-bios-file');
  if (!file) {
    showFirmwareLog('bios', '请先选择 BIOS 镜像文件。', true);
    return;
  }
  // 第一对话框：备份确认（替代原 checkbox）
  const backed = await showAppConfirm({
    title: '确认备份 BIOS',
    message: `即将用「${file.name}」刷写 BIOS。请确认已备份当前 BIOS，并接受刷写失败的救砖风险。`,
    confirmText: '已备份并接受风险',
    danger: true,
  });
  if (!backed) { showFirmwareLog('bios', '已取消刷写。'); return; }
  // 第二对话框：刷写最终确认
  const confirmed = await showAppConfirm({
    title: '再确认：刷写 BIOS',
    message: `刷写期间不能断电，计划关机与系统关机会被拦截；刷写失败可能导致主板无法启动，请确认型号匹配且有救砖方案。`,
    confirmText: '确认刷写',
    danger: true,
  });
  if (!confirmed) { showFirmwareLog('bios', '已取消刷写。'); return; }
  const button = $('firmware-bios-flash');
  if (button) button.disabled = true;
  try {
    const form = new FormData();
    form.append('confirm', 'FLASH');
    form.append('image', file);
    renderFirmwareStatus(await postForm('api/firmware/bios/flash', form));
    startFirmwarePoll();
  } catch (error) {
    showFirmwareLog('bios', `刷写启动失败：${error.message}`, true);
    if (button) button.disabled = false;
  }
}

async function checkNIC() {
  const button = $('firmware-nic-check');
  if (button) {
    button.disabled = true;
    button.textContent = '检测中…';
  }
  showFirmwareHint('正在检测网卡与 MFT 工具…');
  try {
    renderFirmwareStatus(await request('api/firmware/nic/check', { method: 'POST' }));
  } catch (error) {
    showFirmwareHint(`检测失败：${error.message}`, true);
  } finally {
    if (button) {
      button.disabled = false;
      button.textContent = '检测网卡';
    }
  }
}

async function flashNIC(force = false) {
  // 第一对话框：网卡型号与断电确认（替代原 checkbox，不再额外加 secondaryText 避免双取消）
  const modelOk = await showAppConfirm({
    title: force ? '强制升级网卡固件' : '确认网卡型号',
    message: force
      ? `当前固件已与线上版本 ${NIC_TARGET_FW} 一致。强制刷写通常用于固件损坏或分区修复。刷写后必须完全断电再上电。`
      : `即将升级网卡固件到 ${NIC_TARGET_FW}。请确认网卡型号为 MCX4421A-ACQ，并接受刷写后必须完全断电再上电。`,
    confirmText: force ? '强制刷写' : '已确认型号并接受断电要求',
    danger: true,
  });
  if (!modelOk) { showFirmwareLog('nic', '已取消升级。'); return; }
  // 第二对话框：升级最终确认
  const confirmed = await showAppConfirm({
    title: '再确认：升级网卡固件',
    message: `升级期间不能断电，计划关机与系统关机会被拦截；刷写完成后必须完全断电再上电。`,
    confirmText: '确认升级',
    danger: true,
  });
  if (!confirmed) { showFirmwareLog('nic', '已取消升级。'); return; }
  const button = force ? $('firmware-nic-flash-force') : $('firmware-nic-flash');
  if (button) button.disabled = true;
  try {
    const form = new FormData();
    form.append('confirm', 'UPGRADE');
    if (force) form.append('force', '1');
    renderFirmwareStatus(await postForm('api/firmware/nic/flash', form));
    startFirmwarePoll();
  } catch (error) {
    showFirmwareLog('nic', `升级启动失败：${error.message}`, true);
    if (button) button.disabled = false;
  }
}

// 手动安装 MFT：检测到网卡但没装 MFT 时显示，自动从服务器下载并安装。
async function installMFTTool() {
  const ok = await showAppConfirm({
    title: '安装 MFT 工具',
    message: '将自动从服务器下载 MFT 4.37.0 并安装到系统，过程约 1–2 分钟，期间请勿关闭页面。',
    confirmText: '开始安装',
  });
  if (!ok) { showFirmwareLog('nic', '已取消安装。'); return; }
  const button = $('firmware-nic-mft-install');
  if (button) button.disabled = true;
  try {
    renderFirmwareStatus(await postForm('api/firmware/nic/install-mft', new FormData()));
    startFirmwarePoll();
  } catch (error) {
    showFirmwareLog('nic', `安装启动失败：${error.message}`, true);
    if (button) button.disabled = false;
  }
}

// ---- BIOS 在线升级：拉取云端 zip.txt 索引，严格同机型判定 ----
let biosOnlineUpdateAvailable = false;
// 本次检查到的候选固件文案：二次确认弹窗里要写清楚刷的是哪个版本
let biosOnlineCandidateLabel = '';

function showBIOSOnlineHint(message, error = false) {
  const hint = $('bios-online-hint');
  if (!hint) return;
  hint.textContent = message;
  hint.className = `inline-status${error ? ' error' : ''}`;
}

function renderBIOSOnline(status) {
  if (!status) return;
  const setText = (id, value) => {
    const node = $(id);
    if (node) node.textContent = value || '—';
  };
  setText('bios-online-model', status.model);
  const current = status.current_version
    ? `${status.current_version}（${status.current_build_date || '日期未知'}）`
    : status.current_error || (status.checking ? '读取中…' : '');
  setText('bios-online-current', status.current_version_note ? `${current} · ${status.current_version_note}` : current);
  const latest = status.candidate_version
    ? `${status.candidate_version}（${status.candidate_build_date || '日期未知'}）`
    : (status.checking ? '检查中…' : '');
  setText('bios-online-latest', latest);
  setText('bios-online-zip', status.candidate_zip);
  biosOnlineCandidateLabel = latest || status.candidate_zip || '';
  const changelog = $('bios-online-changelog');
  if (changelog) {
    if (status.changelog) {
      changelog.textContent = `更新日志（${status.candidate_zip || ''}）：\n${status.changelog}`;
      changelog.hidden = false;
    } else {
      changelog.textContent = '';
      changelog.hidden = true;
    }
  }
  const badge = $('bios-online-badge');
  const flashButton = $('bios-online-flash');
  biosOnlineUpdateAvailable = Boolean(status.update_available);
  if (status.error) {
    showBIOSOnlineHint(status.error, true);
  } else if (status.update_available) {
    showBIOSOnlineHint(`发现新版本 ${status.candidate_version}（${status.candidate_build_date}），可下载更新。`);
  } else if (status.checking) {
    showBIOSOnlineHint(status.candidate_version
      ? '已获取最新版本信息,正在读取本机当前 BIOS 版本…'
      : '正在连接厂商服务器读取索引…');
  } else if (status.candidate_zip) {
    // 已是最新：不再显示 badge 状态，hint 保持空白让面板更干净
    showBIOSOnlineHint('');
  }
  if (badge) {
    if (status.update_available) { badge.textContent = '有新版本'; badge.className = 'inline-status success'; }
    else if (status.error) { badge.textContent = '检查失败'; badge.className = 'inline-status error'; }
    else if (status.candidate_zip) { badge.textContent = ''; badge.className = 'inline-status'; }
    else { badge.textContent = '未检查'; badge.className = 'inline-status'; }
  }
  updateBIOSOnlineFlashButton();
}

function updateBIOSOnlineFlashButton() {
  const flashButton = $('bios-online-flash');
  if (!flashButton) return;
  flashButton.disabled = !biosOnlineUpdateAvailable;
}

let biosOnlinePollTimer = null;

function stopBIOSOnlinePoll() {
  if (biosOnlinePollTimer !== null) {
    clearInterval(biosOnlinePollTimer);
    biosOnlinePollTimer = null;
  }
  const button = $('bios-online-check');
  if (button) {
    button.disabled = false;
    button.textContent = '检查更新';
  }
}

async function checkBIOSOnline() {
  const button = $('bios-online-check');
  if (button) {
    button.disabled = true;
    button.textContent = '检查中…';
  }
  showBIOSOnlineHint('正在连接厂商服务器,页面会随检查进度逐步刷新…');
  // 渐进渲染:后端各阶段完成即发布缓存快照,轮询 GET 即可逐步拿到
  // 候选版本 → 当前版本 → 是否可更新,无需等整个检查跑完
  const pollOnce = async () => {
    try {
      renderBIOSOnline(await request('api/firmware/bios-online'));
    } catch (error) { /* 单次轮询失败忽略,下一轮再取 */ }
  };
  pollOnce();
  if (biosOnlinePollTimer !== null) clearInterval(biosOnlinePollTimer);
  biosOnlinePollTimer = setInterval(async () => {
    try {
      const status = await request('api/firmware/bios-online');
      renderBIOSOnline(status);
      if (!status.checking) stopBIOSOnlinePoll();
    } catch (error) { /* 忽略,下一轮再取 */ }
  }, 2000);
  // 触发检查:不 await(耗时阶段在服务端跑),终态由轮询渲染;这里只兜底收尾
  request('api/firmware/bios-online/check', { method: 'POST' })
    .then((status) => {
      renderBIOSOnline(status);
      // 后台检查:响应是"检查中"快照时保持轮询,终态才停
      if (!status.checking) stopBIOSOnlinePoll();
    })
    .catch((error) => {
      stopBIOSOnlinePoll();
      showBIOSOnlineHint(`检查失败：${error.message}`, true);
    });
}

async function flashBIOSOnline() {
  const target = biosOnlineCandidateLabel || '云端候选固件';
  // 第一对话框：备份确认（替代原 checkbox）
  const backed = await showAppConfirm({
    title: '确认备份 BIOS',
    message: `即将下载并刷写 ${target}。请确认已备份当前 BIOS，并接受刷写失败的救砖风险。`,
    confirmText: '已备份并接受风险',
    danger: true,
  });
  if (!backed) { showBIOSOnlineHint('已取消刷写。'); return; }
  // 第二对话框：刷写最终确认
  const confirmed = await showAppConfirm({
    title: '再确认：下载并刷写 BIOS',
    message: `刷写期间不能断电，计划关机与系统关机会被拦截；刷写失败可能导致主板无法启动，请确认已有救砖方案。`,
    confirmText: '确认刷写',
    danger: true,
  });
  if (!confirmed) { showBIOSOnlineHint('已取消刷写。'); return; }
  const button = $('bios-online-flash');
  if (button) button.disabled = true;
  try {
    const form = new FormData();
    form.append('confirm', 'FLASH');
    renderFirmwareStatus(await postForm('api/firmware/bios-online/flash', form));
    startFirmwarePoll();
    showBIOSOnlineHint('已开始下载并刷写，进度见下方「手动升级 BIOS」卡片里的日志（刷写期间会自动展开）。');
  } catch (error) {
    showBIOSOnlineHint(`刷写启动失败：${error.message}`, true);
    updateBIOSOnlineFlashButton();
  }
}

// ---- 开关机计划：定时关机由后端触发，定时开机写主板 RTC wakealarm ----
let powerScheduleEntries = [];
let powerScheduleSelected = -1; // 表格选中行；-1 = 无选中
let powerScheduleEditing = -1;  // 编辑器正在编辑的行；-1 = 新增

const SCHEDULE_DAYS_LABEL = { weekday: '平日', weekend: '假日', daily: '每天' };

function showScheduleHint(message, error = false) {
  const hint = $('schedule-hint');
  if (!hint) return;
  hint.textContent = message;
  hint.className = `inline-status${error ? ' error' : ''}`;
}

function updateScheduleToolbar() {
  const has = powerScheduleSelected >= 0 && powerScheduleSelected < powerScheduleEntries.length;
  const entry = has ? powerScheduleEntries[powerScheduleSelected] : null;
  $('schedule-edit').disabled = !has;
  $('schedule-delete').disabled = !has;
  $('schedule-enable').disabled = !has || entry.enabled;
  $('schedule-disable').disabled = !has || !entry.enabled;
}

function renderPowerSchedule(status) {
  if (!status) return;
  powerScheduleEntries = status.entries || [];
  if (powerScheduleSelected >= powerScheduleEntries.length) powerScheduleSelected = -1;
  $('schedule-total').textContent = String(status.total ?? powerScheduleEntries.length);
  $('schedule-enabled').textContent = String(status.enabled_count ?? 0);
  $('schedule-next').textContent = status.next_trigger || '—';
  const trs = powerScheduleEntries.map((entry, index) => {
    const tr = document.createElement('tr');
    if (index === powerScheduleSelected) tr.classList.add('selected');
    const enableTd = document.createElement('td');
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.checked = entry.enabled;
    checkbox.setAttribute('aria-label', `${SCHEDULE_DAYS_LABEL[entry.days] || entry.days} ${entry.time} 启用开关`);
    checkbox.addEventListener('click', (event) => event.stopPropagation());
    checkbox.addEventListener('change', () => {
      powerScheduleEntries[index].enabled = checkbox.checked;
      savePowerSchedule();
    });
    enableTd.appendChild(checkbox);
    tr.appendChild(enableTd);
    const timeTd = document.createElement('td');
    timeTd.textContent = `${SCHEDULE_DAYS_LABEL[entry.days] || entry.days} ${entry.time}`;
    tr.appendChild(timeTd);
    const actionTd = document.createElement('td');
    actionTd.textContent = entry.action === 'poweron' ? '开机' : '关机';
    tr.appendChild(actionTd);
    const statusTd = document.createElement('td');
    const badge = document.createElement('span');
    badge.className = `schedule-status ${entry.enabled ? 'on' : 'off'}`;
    badge.textContent = entry.enabled ? '已启用' : '已停用';
    statusTd.appendChild(badge);
    tr.appendChild(statusTd);
    tr.addEventListener('click', () => {
      powerScheduleSelected = index;
      const rows = $('schedule-rows');
      rows.querySelectorAll('tr').forEach((row, rowIndex) => row.classList.toggle('selected', rowIndex === index));
      updateScheduleToolbar();
    });
    return tr;
  });
  $('schedule-rows').replaceChildren(...trs);
  $('schedule-empty').hidden = powerScheduleEntries.length > 0;
  // 页脚对齐参考界面：RTC · alarm_IRQ · 服务 · 计划写入
  $('schedule-footer').textContent = [
    `RTC: ${status.rtc_time || '读取失败'}`,
    `alarm_IRQ: ${status.rtc_alarm_irq || '未知'}`,
    '服务: enabled',
    `计划写入: ${status.applied ? '是' : '否'}`,
  ].join(' · ');
  updateScheduleToolbar();
  if (status.error) showScheduleHint(status.error, true);
}

async function loadPowerSchedule() {
  try {
    renderPowerSchedule(await request('api/power-schedule'));
  } catch (error) {
    showScheduleHint(`读取计划失败：${error.message}`, true);
  }
}

async function savePowerSchedule() {
  try {
    const status = await request('api/power-schedule/config', {
      method: 'POST',
      body: JSON.stringify({ entries: powerScheduleEntries }),
    });
    renderPowerSchedule(status);
    // RTC 缺失时保存仍成功（仅不能写唤醒）：不能把错误提示覆盖成「已同步到 RTC」
    if (status.error) showScheduleHint(`计划已保存，但${status.error}`, true);
    else showScheduleHint('计划已保存，开机时刻已同步到 RTC。');
  } catch (error) {
    showScheduleHint(`保存失败：${error.message}`, true);
    loadPowerSchedule(); // 回滚到服务端状态，避免界面与服务端不一致
  }
}

function openScheduleEditor(index) {
  powerScheduleEditing = index;
  const entry = index >= 0 ? powerScheduleEntries[index] : { days: 'weekday', time: '10:00', action: 'poweron' };
  $('schedule-editor-title').textContent = index >= 0 ? '编辑计划' : '新增计划';
  $('schedule-editor-days').value = entry.days;
  $('schedule-editor-time').value = entry.time;
  $('schedule-editor-action').value = entry.action;
  $('schedule-editor').hidden = false;
}

function closeScheduleEditor() {
  $('schedule-editor').hidden = true;
}

function submitScheduleEditor() {
  const time = $('schedule-editor-time').value;
  if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(time)) {
    showScheduleHint('时间格式无效，请选择正确的触发时间。', true);
    return;
  }
  const entry = {
    enabled: powerScheduleEditing >= 0 ? powerScheduleEntries[powerScheduleEditing].enabled : true,
    days: $('schedule-editor-days').value,
    time,
    action: $('schedule-editor-action').value,
  };
  if (powerScheduleEditing >= 0) powerScheduleEntries[powerScheduleEditing] = entry;
  else powerScheduleEntries.push(entry);
  closeScheduleEditor();
  savePowerSchedule();
}

// 工具栏「启用/停用」：切换选中行的 enabled 并保存
async function setPowerScheduleEnabled(enabled) {
  if (powerScheduleSelected < 0 || powerScheduleSelected >= powerScheduleEntries.length) return;
  powerScheduleEntries[powerScheduleSelected].enabled = enabled;
  savePowerSchedule();
}

async function deletePowerSchedule() {
  if (powerScheduleSelected < 0 || powerScheduleSelected >= powerScheduleEntries.length) return;
  const entry = powerScheduleEntries[powerScheduleSelected];
  const ok = await showAppConfirm({
    title: '删除计划',
    message: `确定删除「${SCHEDULE_DAYS_LABEL[entry.days] || entry.days} ${entry.time} ${entry.action === 'poweron' ? '开机' : '关机'}」这条计划吗？`,
    confirmText: '删除',
    danger: true,
  });
  if (ok !== true) return;
  powerScheduleEntries.splice(powerScheduleSelected, 1);
  powerScheduleSelected = -1;
  savePowerSchedule();
}

async function applyPowerSchedule() {
  try {
    renderPowerSchedule(await request('api/power-schedule/apply', { method: 'POST' }));
    showScheduleHint('已把下次开机时刻写入 RTC。');
  } catch (error) {
    showScheduleHint(`应用失败：${error.message}`, true);
  }
}

async function testPowerSchedule() {
  const ok = await showAppConfirm({
    title: '测试开机计划',
    message: '将写入 3 分钟后的 RTC 唤醒闹钟。请随后手动关机，观察机器是否自动上电。测试闹钟在保护窗口内不会被计划同步覆盖，过期后自动恢复。',
    confirmText: '写入测试闹钟',
  });
  if (ok !== true) return;
  try {
    const result = await request('api/power-schedule/test', { method: 'POST', body: JSON.stringify({ minutes: 3 }) });
    showScheduleHint(`测试闹钟已写入：${result.wake_at}（RTC UTC）。请手动关机验证自动开机。`);
  } catch (error) {
    showScheduleHint(`测试闹钟写入失败：${error.message}`, true);
  }
}

// ---- 压力测试：CPU / 内存 / GPU 全部由后端施压 ----
function stressOptionsFromForm() {
  const cpu = Boolean($('stress-cpu').checked);
  const gpu = Boolean($('stress-gpu').checked);
  const memory = Boolean($('stress-memory').checked);
  if (!cpu && !gpu && !memory) return null;
  const permanentNode = $('stress-permanent');
  const permanent = Boolean(permanentNode && permanentNode.checked);
  const durationValue = Number($('stress-duration').value);
  const memoryValue = Number($('stress-memory-mb').value);
  // 0 表示永久测试，由后端一直运行到用户点击停止。
  const duration = permanent
    ? 0
    : Math.min(3600, Math.max(10, Number.isFinite(durationValue) && durationValue > 0 ? Math.round(durationValue) : 300));
  const memoryMB = Math.max(64, Number.isFinite(memoryValue) && memoryValue > 0 ? Math.round(memoryValue) : 512);
  const memoryAutoNode = $('stress-memory-auto');
  const memoryAuto = Boolean(memory && memoryAutoNode && memoryAutoNode.checked);
  // 自动预留时 MB 由后端按「可用内存 − 256 MB」计算，这里传 0 表示不用手填值
  return { cpu, gpu, memory, duration_seconds: duration, memory_mb: memoryAuto ? 0 : memoryMB, memory_auto: memoryAuto };
}

// 勾选「自动预留」后禁用 MB 输入框，值不再参与计算
function updateStressMemoryAuto() {
  const auto = $('stress-memory-auto');
  const input = $('stress-memory-mb');
  if (!auto || !input) return;
  input.disabled = Boolean(auto.checked);
  input.placeholder = auto.checked ? '自动' : '';
}

// 只在压力测试运行期间检测风扇关停档：避免用户在不知情时一边压测一边停转导致超温
function updateStressOffWarning(running = false) {
  const warning = $('stress-off-warning');
  if (!warning) return;
  warning.hidden = !(running && fanOffModeActive());
}

function stressParts(options) {
  const parts = [];
  if (!options) return parts;
  if (options.cpu) parts.push('CPU');
  if (options.memory) parts.push('内存');
  if (options.gpu) parts.push('GPU');
  return parts;
}

function renderStressStatus(status) {
  if (!status) return;
  const options = status.options || {};
  const running = Boolean(status.running);
  updateStressOffWarning(running);
  const parts = stressParts(options);
  const permanent = (options.duration_seconds || 0) === 0;
  const text = $('stress-status-text');
  if (text) {
    if (status.error) {
      text.textContent = `压测异常：${status.error}`;
      text.className = 'message error';
    } else if (running) {
      if (permanent) {
        text.textContent = `压测进行中（${parts.join(' + ')}），永久运行，点击「停止」结束`;
      } else {
        const remaining = Math.max(0, (options.duration_seconds || 0) - (status.elapsed_seconds || 0));
        text.textContent = `压测进行中（${parts.join(' + ')}），剩余 ${remaining} 秒`;
      }
      text.className = 'message';
    } else if (parts.length) {
      text.textContent = `已结束（用时 ${status.elapsed_seconds || 0} 秒）`;
      text.className = 'message';
    } else {
      text.textContent = '未开始';
      text.className = 'message';
    }
  }
  const setText = (id, value) => {
    const node = $(id);
    if (node) node.textContent = value;
  };
  setText('stress-elapsed', status.elapsed_seconds ? formatDuration(status.elapsed_seconds * 1000) : '—');
  setText('stress-cpu-percent', running ? `${Number(status.cpu_percent || 0).toFixed(1)}%` : '—');
  setText('stress-memory-used', status.memory_total_bytes
    ? `${formatByteSize(status.memory_used_bytes)} / ${formatByteSize(status.memory_total_bytes)}`
    : '—');
  const memoryNode = $('stress-memory-used');
  if (memoryNode) memoryNode.title = status.memory_allocated_bytes ? `压测分配 ${formatByteSize(status.memory_allocated_bytes)}` : '';
  setText('stress-load', running ? Number(status.load1 || 0).toFixed(2) : '—');
  setText('stress-cpu-temp', running && status.cpu_temp_c ? `${Number(status.cpu_temp_c).toFixed(1)} °C` : '—');
  setText('stress-gpu-freq', status.gpu_freq_mhz ? `${status.gpu_freq_mhz} MHz` : '—');
  setText('stress-gpu-backend', status.gpu_backend === 'vaapi' ? 'VAAPI / ffmpeg' : '—');
  const gpuHint = $('stress-gpu-hint');
  if (gpuHint) {
    if (status.gpu_error) {
      gpuHint.textContent = `GPU 压测异常：${status.gpu_error}`;
      gpuHint.className = 'inline-status error';
    } else if (running && options.gpu) {
      gpuHint.textContent = 'GPU 压测运行中：后端通过 VAAPI 占用核显。';
      gpuHint.className = 'inline-status';
    } else {
      gpuHint.textContent = 'GPU 压测由后端通过 VAAPI 调用核显（需 root 权限）。';
      gpuHint.className = 'inline-status';
    }
  }
  const start = $('stress-start');
  const stop = $('stress-stop');
  if (start) start.disabled = running;
  if (stop) stop.disabled = !running;
  ['stress-cpu', 'stress-gpu', 'stress-memory', 'stress-permanent', 'stress-memory-mb'].forEach((id) => {
    const node = $(id);
    if (node) node.disabled = running;
  });
  const permanentNode = $('stress-permanent');
  const durationNode = $('stress-duration');
  if (durationNode) durationNode.disabled = running || Boolean(permanentNode && permanentNode.checked);
}

async function refreshStressStatus(showError = false) {
  try {
    renderStressStatus(await request('api/stress'));
  } catch (error) {
    if (showError) {
      const text = $('stress-status-text');
      if (text) {
        text.textContent = `读取压测状态失败：${error.message}`;
        text.className = 'message error';
      }
    }
  }
}

function startStressPoll() {
  if (stressPollTimer) return;
  stressPollTimer = setInterval(() => refreshStressStatus(), STRESS_POLL_MS);
}

function stopStressPoll() {
  if (!stressPollTimer) return;
  clearInterval(stressPollTimer);
  stressPollTimer = null;
}

async function startStressRun() {
  const options = stressOptionsFromForm();
  const text = $('stress-status-text');
  if (!options) {
    if (text) {
      text.textContent = '请至少勾选 CPU、GPU、内存中的一项。';
      text.className = 'message error';
    }
    return;
  }
  // 只在压力测试启用时检测关停档：压测持续升温而风扇不会起转，容易在不知情下超温
  if (fanOffModeActive()) {
    const result = await showAppConfirm({
      title: '风扇处于「关停」档',
      message: '压力测试会让整机持续升温，而关停档下风扇不会自动起转、超温只蜂鸣报警，可能造成硬件损坏。建议先恢复关停前的风扇曲线再开始压测。',
      confirmText: '仍然开始压测',
      danger: true,
      secondaryText: '恢复关停前状态并开始',
    });
    if (!result) return;
    if (result.secondary) {
      // 「恢复并开始」：恢复后必须回读权威服务端状态，确认确实退出关停档才继续；
      // 回读失败 / 响应损坏 / 仍是关停档都按不确定处理——保留提示并停止启动。
      if (!(await restoreFanOffState({ skipConfirm: true }))) return;
      if (!(await confirmFanOffModeReleased())) return;
    } else {
      // 用户明确承担风险直接压测：告知服务端跳过关停档门槛
      options.ack_off_mode = true;
    }
  }
  const button = $('stress-start');
  if (button) button.disabled = true;
  try {
    renderStressStatus(await request('api/stress/start', { method: 'POST', body: JSON.stringify(options) }));
    startStressPoll();
  } catch (error) {
    if (text) {
      text.textContent = `启动失败：${error.message}`;
      text.className = 'message error';
    }
    if (button) button.disabled = false;
  }
}

async function stopStressRun() {
  const button = $('stress-stop');
  if (button) button.disabled = true;
  try {
    renderStressStatus(await request('api/stress/stop', { method: 'POST' }));
  } catch (error) {
    if (button) button.disabled = false;
    const text = $('stress-status-text');
    if (text) {
      text.textContent = `停止失败：${error.message}`;
      text.className = 'message error';
    }
  }
}


function activateTab(tabID, focus = false) {
  // CRPS 页 iframe 懒加载：首次切到该页才加载，避免常驻轮询耗电 API。
  // 加载后按内容高度自适应 iframe，页面内部不滚动，滚动交给主应用整页（滚动条直达底部）。
  if (tabID === 'tab-crps') {
    const frame = $('crps-frame');
    if (frame && !frame.src && frame.dataset.src) {
      frame.src = frame.dataset.src;
      frame.addEventListener('load', () => {
        const mobileMQ = window.matchMedia('(max-width: 640px)');
        const fit = () => {
          try {
            if (!frame.offsetParent) return; // 面板隐藏: 不按 0 高度改写
            // 手机端: iframe 固定为可视高度, 内部自滚动。
            if (mobileMQ.matches) {
              const top = Math.max(0, frame.getBoundingClientRect().top);
              frame.style.height = Math.max(320, Math.round(window.innerHeight - top - 4)) + 'px';
              return;
            }
            const doc = frame.contentDocument;
            if (doc && doc.body) {
              // body 实际布局高度 (不受 iframe 初始视口下限影响), 内容收缩时可回缩
              const h = Math.ceil(doc.body.getBoundingClientRect().height);
              if (h > 0) frame.style.height = Math.max(320, h) + 'px';
            }
          } catch (e) { /* 同源保护, 不可读时保持 CSS 高度 */ }
        };
        frame._crpsFit = fit;
        fit();
        if (frame._crpsFitTimer) clearInterval(frame._crpsFitTimer);
        frame._crpsFitTimer = setInterval(fit, 3000); // 内容随数据刷新高度会变, 周期校准
      });
    } else if (frame && frame._crpsFit) {
      // 已加载过: 切回时立即校准一次 (隐藏期间定时器一直在空转)
      requestAnimationFrame(frame._crpsFit);
    }
  }
  // 只遍历主导航标签：曲线编辑器等页内 [role=tab] 自成 tablist，
  // 不归主页面切换管——全局遍历会把它们的 aria-controls 面板误隐藏
  const tabs = [...document.querySelectorAll('.app-tabs [role="tab"]')];
  tabs.forEach((tab) => {
    const active = tab.id === tabID;
    tab.setAttribute('aria-selected', String(active));
    tab.tabIndex = active ? 0 : -1;
    const panel = $(tab.getAttribute('aria-controls'));
    if (panel) panel.hidden = !active;
    if (active && focus) tab.focus();
  });
  if (tabID === 'tab-fan') requestAnimationFrame(() => { CURVE_KINDS.forEach(renderFanChart); fetchHistory(); renderHistoryChart(); if (fanDebugVisiblePref()) startFanDebugPoll(); });
  else stopFanDebugPoll(); // 离开风扇控制页即停轮询(调试卡片不在当前页)
  // 固件/压测状态只在对应页轮询；压测任务本身在后端继续跑，GPU 着色器按剩余时长自动停止
  if (tabID === 'tab-firmware') requestAnimationFrame(() => { refreshFirmwareStatus(); startFirmwarePoll(); });
  else stopFirmwarePoll();
  if (tabID === 'tab-stress') requestAnimationFrame(() => { refreshStressStatus(); startStressPoll(); });
  else stopStressPoll();
  // 进按键映射页时刷新一次备份运行状态（备份是异步的，回来能看到结果）
  if (tabID === 'tab-gpio') requestAnimationFrame(refreshBackupStatus);
  // 调试页的传感器显示名列表靠历史数据发现传感器键，进页时刷新一次再渲染
  if (tabID === 'tab-debug') requestAnimationFrame(() => { fetchHistory().then(renderSensorNamesList); });
  // 开关机计划页：进页拉一次状态（RTC/计划写入状态随时间变化）
  if (tabID === 'tab-schedule') requestAnimationFrame(loadPowerSchedule);
}

function setupTabs() {
  // 同 activateTab：只接管主导航标签的点击与方向键导航
  const tabs = [...document.querySelectorAll('.app-tabs [role="tab"]')];
  tabs.forEach((tab, index) => {
    tab.addEventListener('click', () => activateTab(tab.id));
    tab.addEventListener('keydown', (event) => {
      let target = index;
      if (event.key === 'ArrowLeft') target = (index - 1 + tabs.length) % tabs.length;
      else if (event.key === 'ArrowRight') target = (index + 1) % tabs.length;
      else if (event.key === 'Home') target = 0;
      else if (event.key === 'End') target = tabs.length - 1;
      else return;
      event.preventDefault();
      activateTab(tabs[target].id, true);
    });
  });
}

function setupHealthTooltip() {
  const summary = document.querySelector('.health-summary');
  const badge = $('health');
  const tooltip = $('health-tooltip');
  const finePointer = () => window.matchMedia('(hover: hover) and (pointer: fine)').matches;
  const show = () => tooltip.classList.add('visible');
  const hide = () => tooltip.classList.remove('visible');
  const place = (clientX, clientY) => {
    const gap = 14;
    const margin = 8;
    const rect = tooltip.getBoundingClientRect();
    let left = clientX + gap;
    if (left + rect.width > window.innerWidth - margin) left = clientX - rect.width - gap;
    const top = Math.min(
      Math.max(margin, clientY - rect.height / 2),
      Math.max(margin, window.innerHeight - rect.height - margin),
    );
    tooltip.style.right = 'auto';
    tooltip.style.bottom = 'auto';
    tooltip.style.left = `${Math.max(margin, left)}px`;
    tooltip.style.top = `${top}px`;
  };
  const placeNearBadge = () => {
    const rect = badge.getBoundingClientRect();
    place(rect.right, rect.top + rect.height / 2);
  };
  summary.addEventListener('mouseenter', (event) => {
    show();
    place(event.clientX, event.clientY);
  });
  summary.addEventListener('mousemove', (event) => {
    show();
    place(event.clientX, event.clientY);
  });
  summary.addEventListener('mouseleave', hide);
  summary.addEventListener('focusin', () => {
    show();
    placeNearBadge();
  });
  summary.addEventListener('focusout', (event) => {
    if (!summary.contains(event.relatedTarget)) hide();
  });
  summary.addEventListener('click', (event) => {
    if (finePointer()) return;
    event.preventDefault();
    event.stopPropagation();
    if (tooltip.classList.contains('visible')) hide();
    else {
      show();
      placeNearBadge();
    }
  });
  document.addEventListener('click', (event) => {
    if (!summary.contains(event.target)) hide();
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') hide();
  });
}

function updateGPIOEnabledState() {
  const enabled = $('gpio-enabled').checked;
  document.querySelectorAll('.gpio-action').forEach((select) => { select.disabled = !enabled; });
}

// --- 刷新 SD 卡（重新识别 USB 读卡器）--------------------------------------
// 与「按一下网络键」是同一个动作，这里给一个不用跑到机器跟前的手动入口。

let sdcardRefreshBusy = false;

function describeStorageInsert(event = {}) {
  const devices = Array.isArray(event.devices) && event.devices.length ? event.devices.join('、') : '';
  const when = event.detected_at ? new Date(event.detected_at).toLocaleTimeString() : '';
  return [event.kind_label || '', devices, event.chime ? `（${event.chime}）` : '', when].filter(Boolean).join(' ');
}

// 存储设备插入提示音：显示开关、正在监视的设备数以及最近几次插入事件。
function renderStorageNotifyStatus(storageNotify = {}) {
  const target = $('gpio-storage-notify-status');
  if (!target) return;
  const count = (value) => Number(value) || 0;
  const parts = [
    storageNotify.enabled ? '提示音已开启' : '提示音已关闭',
    `正在监视 ${count(storageNotify.devices)} 个存储设备（移动存储 ${count(storageNotify.removable)}、内置存储 ${count(storageNotify.internal)}）`,
  ];
  const recent = Array.isArray(storageNotify.recent) ? storageNotify.recent : [];
  if (recent.length) {
    parts.push(`最近：${describeStorageInsert(recent[0])}`);
  } else {
    parts.push('还没有识别到新插入的存储设备');
  }
  if (storageNotify.last_error) parts.push(storageNotify.last_error);
  target.textContent = parts.join(' · ');
  target.className = `inline-status${storageNotify.last_error ? ' error' : ''}`;
  target.title = recent.map(describeStorageInsert).join('\n');
}

// 刷新 SD 卡：按键映射页与硬盘面板「移动设备」区块共用这一条路径（同一个
// 后端动作，只是按钮与状态行各自归属自己的卡片）。刷新完再拉一次状态，
// 新认出来的卡立刻变成图标，不用等下一拍轮询。
async function refreshSDCardManually(button = $('gpio-refresh-sdcard'), status = $('gpio-sdcard-status')) {
  if (sdcardRefreshBusy) return;
  sdcardRefreshBusy = true;
  button.disabled = true;
  const originalLabel = button.textContent;
  const rawIdle = status?.dataset?.idle;
  const idleText = typeof rawIdle === 'string' ? rawIdle : '';
  // 记住自己那一行的基类（并剔掉上一次失败留下的 error），刷新结束后按原样恢复。
  const rawClass = typeof status?.className === 'string' ? status.className : '';
  const statusClass = rawClass.split(/\s+/).filter((name) => name && name !== 'error').join(' ');
  button.textContent = '正在刷新…';
  if (status) {
    status.textContent = '正在重新识别 USB 读卡器，请稍候（通常几秒）…';
    status.className = statusClass;
  }
  try {
    const result = await request('api/gpio/refresh-sdcard', { method: 'POST', body: '{}' });
    if (status) {
      // 后端只报「真的读到东西」的结果：读卡器重识别了但没介质时 summary 为空，
      // 这时回到自己那一行的初始说明（硬盘面板没有说明就是空白），不堆提示。
      status.textContent = result.summary || idleText;
      status.className = statusClass;
    }
    await refresh(true);
  } catch (error) {
    if (status) {
      status.textContent = error.message;
      status.className = `${statusClass} error`;
    }
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
    sdcardRefreshBusy = false;
  }
}

function fillGPIOInputs(gpio = {}) {
  $('gpio-enabled').checked = Boolean(gpio.enabled);
  // 提示音开关缺省为开：老配置里没有这个字段，不能因此显示成关闭。
  $('gpio-storage-notify').checked = gpio.storage_notify !== false;
  // 按键声光反馈开关缺省为开：老配置里没有这个字段，不能因此显示成关闭。
  $('gpio-feedback').checked = gpio.feedback !== false;
  // 开机/关机提示音开关缺省为开：老配置里没有这个字段，同样按开启显示。
  $('gpio-power-chime').checked = gpio.power_chime !== false;
  gpioMigrations = Array.isArray(gpio.migrations) ? gpio.migrations.map(String) : [];
  gpioScripts = Array.isArray(gpio.scripts)
    ? gpio.scripts.map((script) => ({ id: String(script.id), name: String(script.name), body: String(script.body) }))
    : [];
  gpioBackups = Array.isArray(gpio.backups)
    ? gpio.backups.map((job) => ({
      id: String(job.id),
      name: String(job.name),
      source: normalizeBackupLocation(job.source),
      target: normalizeBackupLocation(job.target),
      mode: String(job.mode || 'copy'),
      overwrite: job.overwrite !== false,
      keep: Number(job.keep) || 0,
      marker: String(job.marker || ''),
    }))
    : [];
  gpioAutos = Array.isArray(gpio.autos)
    ? gpio.autos.map((rule) => ({
      id: String(rule.id),
      name: String(rule.name),
      direction: String(rule.direction || 'from_volume'),
      fixed: normalizeBackupLocation(rule.fixed),
      volume_sub: String(rule.volume_sub || ''),
      mode: String(rule.mode || 'archive'),
      overwrite: rule.overwrite !== false,
      keep: Number(rule.keep) || 0,
      // 必须原样带回：丢了它，页面重新加载后再保存按键配置会被后端
      // validateGPIOAutoBackup 拒掉（「请在规则里勾选确认后再保存」），
      // 用户此后任何按键改动都存不上；编辑器回显也会变成没勾选。
      allow_move_to_volume: rule.allow_move_to_volume === true,
    }))
    : [];
  setupGPIOActions(gpioScripts, false, gpioBackups, gpioAutos);
  const buttons = new Map((gpio.buttons || []).map((button) => [button.id, button]));
  document.querySelectorAll('.gpio-table tbody tr').forEach((row) => {
    const actions = buttons.get(row.dataset.button)?.actions || {};
    row.querySelectorAll('.gpio-action').forEach((select) => {
      select.value = actions[select.dataset.stage] || 'none';
    });
  });
  closeGPIOScriptEditor();
  renderGPIOScriptList();
  closeBackupJobEditor();
  renderBackupJobList();
  closeAutoBackupEditor();
  renderAutoBackupList();
  refreshAutoBackupDevices();
  showGPIOScriptMessage(gpioScripts.length ? `已加载 ${gpioScripts.length} 个脚本。` : '尚未创建自定义脚本。');
  updateGPIOEnabledState();
  updateGPIOFeedbackStatus();
  updatePowerChimeStatus();
}

function gpioConfigFromInputs() {
  return {
    version: 1,
    enabled: $('gpio-enabled').checked,
    storage_notify: $('gpio-storage-notify').checked,
    feedback: $('gpio-feedback').checked,
    power_chime: $('gpio-power-chime').checked,
    migrations: [...gpioMigrations],
    scripts: gpioScripts.map((script) => ({ ...script })),
    backups: gpioBackups.map((job) => ({ ...job })),
    autos: gpioAutos.map((rule) => ({ ...rule })),
    buttons: [...document.querySelectorAll('.gpio-table tbody tr')].map((row) => {
      const actions = { short: 'none' };
      row.querySelectorAll('.gpio-action').forEach((select) => { actions[select.dataset.stage] = select.value; });
      return { id: row.dataset.button, actions };
    }),
  };
}

function curveFromInputs(kind = 'cpu') {
  return curveEditors[kind].curve.map((point) => ({ ...point }));
}

function clamp(value, minimum, maximum) {
  return Math.max(minimum, Math.min(maximum, value));
}

function fanMinimumPWM() {
  // 关停档：0% 也要能填进曲线，最低转速不再是地板
  if (fanOffMode) return 0;
  const raw = $('fan-min').value;
  const value = Number(raw);
  // 空框（用户正在改）或非法输入按默认最低转速走，30% 是硬性下限而不是默认值
  if (raw === '' || raw === null || raw === undefined || !Number.isFinite(value)) return DEFAULT_MIN_PWM_PERCENT;
  return clamp(Math.round(value), 30, 100);
}

function fanEmergencyTemperature() {
  const value = Number($('fan-emergency').value);
  return Number.isFinite(value) && value > 0 ? clamp(Math.round(value), 70, 100) : 100;
}

function curvePointLimits(kind, index) {
  const editor = curveEditors[kind];
  const previous = editor.curve[index - 1];
  const next = editor.curve[index + 1];
  return {
    minTemp: previous ? Math.floor(previous.temp_c) + 1 : 20,
    maxTemp: next ? Math.ceil(next.temp_c) - 1 : fanEmergencyTemperature(),
    minPWM: Math.max(fanMinimumPWM(), previous ? Math.round(previous.pwm_percent) : 0),
    maxPWM: next ? Math.round(next.pwm_percent) : 100,
  };
}

function setCurvePoint(kind, index, temperature, pwm) {
  const editor = curveEditors[kind];
  if (!editor.curve[index]) return;
  const limits = curvePointLimits(kind, index);
  const minTemp = Math.min(limits.minTemp, limits.maxTemp);
  const maxTemp = Math.max(limits.minTemp, limits.maxTemp);
  const minPWM = Math.min(limits.minPWM, limits.maxPWM);
  const maxPWM = Math.max(limits.minPWM, limits.maxPWM);
  editor.curve[index] = {
    temp_c: clamp(Math.round(temperature), minTemp, maxTemp),
    pwm_percent: clamp(Math.round(pwm), minPWM, maxPWM),
  };
}

function normalizeCurveToControls(kind) {
  const editor = curveEditors[kind];
  let priorPWM = fanMinimumPWM();
  editor.curve = editor.curve.map((point) => {
    const pwm = clamp(Math.max(Math.round(point.pwm_percent), priorPWM), priorPWM, 100);
    priorPWM = pwm;
    return { temp_c: Math.round(point.temp_c), pwm_percent: pwm };
  });
  const maximum = fanEmergencyTemperature();
  for (let index = editor.curve.length - 1; index >= 0; index -= 1) {
    const cap = maximum - (editor.curve.length - 1 - index);
    editor.curve[index].temp_c = Math.min(editor.curve[index].temp_c, cap);
  }
  for (let index = 0; index < editor.curve.length; index += 1) {
    const floor = index === 0 ? 20 : editor.curve[index - 1].temp_c + 1;
    editor.curve[index].temp_c = Math.max(editor.curve[index].temp_c, floor);
  }
  renderFanChart(kind);
}

function findCurveAddCandidate(kind) {
  const curve = curveEditors[kind].curve;
  if (curve.length >= CURVE_MAX_POINTS) return null;
  const maximum = fanEmergencyTemperature();
  let best = null;
  for (let index = 0; index <= curve.length; index += 1) {
    const lower = index === 0 ? 20 : Math.floor(curve[index - 1].temp_c) + 1;
    const upper = index === curve.length ? maximum : Math.ceil(curve[index].temp_c) - 1;
    if (lower > upper) continue;
    const width = upper - lower;
    if (best && width <= best.width) continue;
    const previous = curve[index - 1];
    const next = curve[index];
    let pwm = fanMinimumPWM();
    if (previous && next) pwm = Math.round((previous.pwm_percent + next.pwm_percent) / 2);
    else if (previous) pwm = previous.pwm_percent;
    else if (next) pwm = next.pwm_percent;
    best = {
      index,
      width,
      point: { temp_c: Math.round((lower + upper) / 2), pwm_percent: Math.round(pwm) },
    };
  }
  return best;
}

function updateCurveControls(kind) {
  const editor = curveEditors[kind];
  editor.selectedIndex = clamp(editor.selectedIndex, 0, Math.max(0, editor.curve.length - 1));
  const selected = editor.curve[editor.selectedIndex];
  $(editor.selectedID).textContent = selected
    ? `节点 ${editor.selectedIndex + 1}/${editor.curve.length} · ${selected.temp_c} °C · ${selected.pwm_percent}%`
    : '—';
  $(editor.addID).disabled = uiBusy || !findCurveAddCandidate(kind);
  $(editor.removeID).disabled = uiBusy || editor.curve.length <= CURVE_MIN_POINTS;
}

function addCurvePoint(kind) {
  const editor = curveEditors[kind];
  const candidate = findCurveAddCandidate(kind);
  if (!candidate) return;
  editor.curve.splice(candidate.index, 0, candidate.point);
  editor.selectedIndex = candidate.index;
  renderFanChart(kind);
}

function removeSelectedCurvePoint(kind) {
  const editor = curveEditors[kind];
  if (editor.curve.length <= CURVE_MIN_POINTS) return;
  editor.curve.splice(editor.selectedIndex, 1);
  editor.selectedIndex = clamp(editor.selectedIndex, 0, editor.curve.length - 1);
  renderFanChart(kind);
}

// ---- 预设曲线模式：静音 / 平衡 / 性能 --------------------------------------
// 系统集成的三档参数，数值与后端 internal/powerguard/fan.go 的 builtinFanCurves
// 一一对应，改一边必须同步另一边。出厂默认就是「平衡」这一档。
// 点一下替换的是三条曲线的全部节点（界面上的，未保存），保存风扇控制后才下发；
// 低于「最低转速」的节点会在套用时按最低转速抬起来。
const CURVE_PRESETS = {
  quiet: {
    label: '静音',
    summary: '低转速优先，晚一点拉高',
    points: () => ({
      hdd: [[20, 40], [30, 40], [35, 43], [45, 43], [50, 55], [52, 70], [55, 80], [60, 90]],
      nvme: [[20, 40], [45, 40], [55, 55], [60, 60]],
      cpu: [[20, 40], [42, 40], [44, 43], [60, 43], [70, 50], [75, 60], [80, 80], [85, 100]],
    }),
  },
  balanced: {
    label: '平衡',
    summary: '常温低速、升温及时提速（出厂默认）',
    points: () => ({
      hdd: [[20, 40], [30, 40], [35, 47], [45, 47], [50, 60], [52, 75], [55, 80], [60, 100]],
      nvme: [[20, 40], [45, 40], [55, 60], [60, 70]],
      cpu: [[20, 40], [42, 40], [44, 47], [60, 47], [70, 55], [75, 65], [80, 85], [85, 100]],
    }),
  },
  performance: {
    label: '性能',
    summary: '高转速优先，温度一上来就压住',
    points: () => ({
      hdd: [[20, 40], [30, 40], [35, 47], [40, 50], [50, 65], [52, 80], [55, 90], [60, 100]],
      nvme: [[20, 40], [45, 40], [55, 65], [60, 75]],
      cpu: [[20, 47], [45, 47], [50, 60], [55, 70], [60, 85], [85, 100]],
    }),
  },
};
const CURVE_PRESET_ORDER = ['quiet', 'balanced', 'performance'];

// ---- 曲线模式状态 ----------------------------------------------------------
// activeCurveMode 只驱动按键选中态与文案：quiet/balanced/performance/temp/
// full/off/manual/slot-1..3。fanOffMode 决定曲线能不能写 0%（关停档）。
let activeCurveMode = 'temp';
let fanOffMode = false;
// 进入全速／关停之前记下的三条曲线，点「临时」可以原样回来
const TEMP_CURVE_STORAGE_KEY = 'tad-fan-curve-temp';
let tempCurveBackup = null;
// 后端在进入关停档时留的曲线快照：换浏览器/清过本地存储时也能恢复原选定曲线
let offModeCurveBackup = null;

function loadTempCurveBackup() {
  try {
    const raw = window.localStorage.getItem(TEMP_CURVE_STORAGE_KEY);
    const parsed = raw ? JSON.parse(raw) : null;
    return validCurveSet(parsed);
  } catch (error) {
    return null;
  }
}

function backupTempCurves() {
  // 只在从「正常曲线」切到极端档时备份，免得把全 0／全 100 当成临时曲线
  if (activeCurveMode === 'full' || activeCurveMode === 'off') return;
  tempCurveBackup = currentCurveSet();
  try {
    window.localStorage.setItem(TEMP_CURVE_STORAGE_KEY, JSON.stringify(tempCurveBackup));
  } catch (error) { /* 本地存储不可用时只留内存 */ }
}

function setCurveMode(mode) {
  activeCurveMode = mode || 'temp';
  fanOffMode = activeCurveMode === 'off';
  renderCurveModeStates();
}

function renderCurveModeStates() {
  document.querySelectorAll('[data-curve-mode]').forEach((button) => {
    const active = button.dataset.curveMode === activeCurveMode;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', active ? 'true' : 'false');
  });
  for (let slot = 1; slot <= CURVE_SLOT_COUNT; slot += 1) {
    const button = $(`curve-custom-${slot}`);
    if (!button) continue;
    const active = activeCurveMode === `slot-${slot}`;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', active ? 'true' : 'false');
  }
}

const CURVE_MODE_FIELDS = { hdd: 'hdd_curve', nvme: 'nvme_curve', cpu: 'curve' };

// 用后端配置反推当前属于哪一档：关停 → 预设 → 全速 → 手动（三条都同一恒定值）→ 其余按临时算
function detectCurveMode(fan = {}) {
  if (fan.off_mode) return 'off';
  const samePoints = (curve, rows) => Array.isArray(curve) && curve.length === rows.length
    && curve.every((point, index) => Math.round(Number(point.temp_c)) === rows[index][0]
      && Math.round(Number(point.pwm_percent)) === rows[index][1]);
  for (const mode of CURVE_PRESET_ORDER) {
    const preset = CURVE_PRESETS[mode].points();
    if (CURVE_KINDS.every((kind) => samePoints(fan[CURVE_MODE_FIELDS[kind]], preset[kind]))) return mode;
  }
  const flatValue = (curve) => {
    if (!Array.isArray(curve) || !curve.length) return null;
    const first = Math.round(Number(curve[0].pwm_percent));
    if (!Number.isFinite(first)) return null;
    return curve.every((point) => Math.round(Number(point.pwm_percent)) === first) ? first : null;
  };
  const flat = CURVE_KINDS.map((kind) => flatValue(fan[CURVE_MODE_FIELDS[kind]]));
  if (flat.every((value) => value === 100)) return 'full';
  // 三条曲线都是同一个恒定百分比（且不是 0／100）＝手动档
  if (flat.every((value) => value !== null && value === flat[0]) && flat[0] > 0 && flat[0] < 100) return 'manual';
  return 'temp';
}

// 把一套曲线填进三个编辑器：先切模式再收口，关停档的 0% 才不会被最低转速抬起来
function applyCurveSetToEditors(curves) {
  if (!curves) return;
  CURVE_KINDS.forEach((kind) => {
    if (!curves[kind]) return;
    const editor = curveEditors[kind];
    editor.curve = curves[kind].map((point) => ({ ...point }));
    editor.selectedIndex = 0;
    normalizeCurveToControls(kind);
  });
}

// 平线：保留温度节点，只把所有转速改成同一个百分比
function flatCurveSet(percent) {
  const curves = currentCurveSet();
  CURVE_KINDS.forEach((kind) => {
    curves[kind] = curves[kind].map((point) => ({ temp_c: point.temp_c, pwm_percent: percent }));
  });
  return curves;
}

function curvePresetPoints(mode, kind) {
  const preset = CURVE_PRESETS[mode];
  if (!preset) return null;
  const rows = preset.points()[kind];
  return Array.isArray(rows) ? rows.map(([temp_c, pwm_percent]) => ({ temp_c, pwm_percent })) : null;
}

// 模式弹窗统一出口：主按钮只改界面，副按钮改完顺手下发保存
async function applyModeCurves(mode, curves, doneMessage) {
  setCurveMode(mode);
  applyCurveSetToEditors(curves);
  if (doneMessage) showMessage(doneMessage, false, 'message-fan');
}

// 静音 / 平衡 / 性能：三档系统参数，应用并保存一体
async function applyCurvePreset(mode) {
  const preset = CURVE_PRESETS[mode];
  if (!preset) return;
  const curves = {};
  for (const kind of CURVE_KINDS) {
    const points = curvePresetPoints(mode, kind);
    if (!points) return;
    curves[kind] = points;
  }
  const result = await showAppConfirm({
    title: `应用「${preset.label}」曲线`,
    message: `会用「${preset.label}」的节点替换三条曲线的全部节点，界面上还没保存的手动调整也会被覆盖。`,
    confirmText: '应用',
    secondaryText: '应用并保存风扇控制',
  });
  if (!result) return;
  backupTempCurves();
  await applyModeCurves(mode, curves, `已应用「${preset.label}」曲线（${preset.summary}）。`);
  if (result.secondary) await saveFanConfig();
}

// 全速：三条曲线全部 100%
async function applyFullSpeed() {
  const result = await showAppConfirm({
    title: '全部风扇全速运转',
    message: '三条曲线的全部节点会拉满到 100%，风扇立刻按最高转速运行；噪音与功耗都会明显上升。',
    confirmText: '应用',
    secondaryText: '应用并保存风扇控制',
  });
  if (!result) return;
  backupTempCurves();
  await applyModeCurves('full', flatCurveSet(100), '已把三条曲线全部拉到 100%；风扇进入全速。');
  if (result.secondary) await saveFanConfig();
}

// 恢复关停后回读权威服务端状态：只有服务端明确报告已退出关停档才返回 true。
// 回读失败、响应损坏（request 已对坏 JSON 抛错）或 fan 段缺失/仍是关停档，
// 一律按不确定处理——保留未知提示并停止后续压测启动。
async function confirmFanOffModeReleased() {
  const text = $('stress-status-text');
  let status;
  try {
    status = await request('api/status');
  } catch (error) {
    if (text) {
      text.textContent = `恢复结果未知：无法回读服务端状态（${error.message}），已取消压测启动。`;
      text.className = 'message error';
    }
    return false;
  }
  const fan = status && status.fan_control;
  // off_mode === true 时服务端明确上报关停；false/缺省即未处于关停档。
  // 整个 fan_control 段缺失说明状态不可信，同样按不确定处理。
  if (fan && typeof fan === 'object' && fan.off_mode !== true) return true;
  if (text) {
    text.textContent = '恢复结果未知：服务端仍报告风扇处于关停档，已取消压测启动。';
    text.className = 'message error';
  }
  return false;
}

// 恢复关停前记录的三条曲线并保存：压测拦截与再次点击「关停」共用这一条路。
// 后端 5 分钟到期自动起转后，本地快照仍可用来恢复。
async function restoreFanOffState({ skipConfirm = false } = {}) {
  if (!skipConfirm) {
    const result = await showAppConfirm({
      title: '恢复原选定曲线',
      message: '解除关停档，按关停前记下的三条曲线恢复并保存，风扇会立刻重新起转。',
      confirmText: '恢复并保存',
    });
    if (!result) return false;
  }
  const fallback = clamp(Number($('fan-min').value) || DEFAULT_MIN_PWM_PERCENT, 30, 100);
  const curves = tempCurveBackup || offModeCurveBackup || flatCurveSet(fallback);
  await applyModeCurves('temp', curves, '已恢复关停前的曲线，正在下发起转…');
  return saveFanConfig();
}

// 关停：全部 0%，优先级高于最低转速。第二次确认只留一个红色动作——
// 关停必须落盘才算数，去掉「只改界面不保存」的主按钮，免得用户以为已经停了。
// 关停档最长 5 分钟，后端到期自动恢复原曲线并起转；再次点击同一按钮也会
// 直接退出关停、恢复原选定曲线。
async function applyShutdown() {
  if (fanOffMode) {
    await restoreFanOffState();
    return;
  }
  const first = await showAppConfirm({
    title: '关停全部风扇',
    message: '绑定风扇会立刻停转（0%），最低转速保护被绕过。温度会快速上升，只适合短时间检修或换盘；关停最长 5 分钟，到期自动恢复原选定曲线并起转。',
    confirmText: '继续',
    danger: true,
  });
  if (!first) return;
  const second = await showAppConfirm({
    title: '再确认一次：确定要关停风扇？',
    message: '停转期间硬盘与 NVMe 温度会持续升高。关停最多持续 5 分钟，到期自动恢复原选定曲线并起转；此前的紧急温度兜底也不再生效，只会鸣响蜂鸣器报警——维修时请先断电并做好散热防护。',
    hideConfirm: true,
    secondaryText: '关停并保存风扇控制',
    secondaryDanger: true,
  });
  if (!second || !second.secondary) return;
  backupTempCurves();
  await applyModeCurves('off', flatCurveSet(0), '已把三条曲线全部设为 0%，正在下发关停…');
  await saveFanConfig();
}

// 手动：自己填一个百分比，三条曲线变成同值平线
async function applyManualSpeed() {
  const suggested = fanOffMode ? DEFAULT_MIN_PWM_PERCENT : fanMinimumPWM();
  const result = await showAppConfirm({
    title: '手动设定转速',
    message: '填一个百分比，三条曲线的全部节点都会设成这个值。',
    input: { label: '转速百分比（30–100）', value: String(suggested), placeholder: '例如 55', maxLength: 3 },
    confirmText: '应用',
    secondaryText: '应用并保存风扇控制',
  });
  if (!result) return;
  const raw = result.secondary ? String(result.value || suggested) : String(result);
  const percent = Number(raw);
  if (!Number.isFinite(percent) || percent < 30 || percent > 100) {
    showMessage('转速百分比需在 30–100 之间。', true, 'message-fan');
    return;
  }
  const value = Math.round(percent);
  backupTempCurves();
  await applyModeCurves('manual', flatCurveSet(value), `已把三条曲线全部设为 ${value}%。`);
  if (result.secondary) await saveFanConfig();
}

// 临时：回到自己调的那版曲线
async function applyTempMode() {
  if (tempCurveBackup) {
    const result = await showAppConfirm({
      title: '回到临时曲线',
      message: '会用进入全速／关停前记下的三条曲线覆盖编辑器。',
      confirmText: '恢复',
      secondaryText: '应用并保存风扇控制',
    });
    if (!result) return;
    await applyModeCurves('temp', tempCurveBackup, '已恢复临时曲线。');
    if (result.secondary) await saveFanConfig();
    return;
  }
  const wasOff = fanOffMode;
  await applyModeCurves('temp', null, '已切到「临时」：按当前曲线下发，点「保存风扇控制」生效。');
  // 从关停回来又没有临时备份：把 0% 抬回最低转速，免得一保存就被校验拦下
  if (wasOff) CURVE_KINDS.forEach(normalizeCurveToControls);
}

// ---- 自定义曲线按键（三个）：每个按键独立一套曲线 -------------------------
// 数据结构：{ "1": { name, curves: {hdd,nvme,cpu} | null }, ... }
// 存在本浏览器（localStorage）；导出成 JSON 可备份、换机搬用。
// 点按键不直接载入，而是打开操作弹窗：应用 / 保存 / 导出 / 导入 / 改名 / 取消。
const CURVE_SLOT_COUNT = 3;
const CURVE_SLOT_STORAGE_KEY = 'tad-fan-curve-slots';
const CURVE_SLOT_DEFAULT_NAMES = { 1: '自定义 1', 2: '自定义 2', 3: '自定义 3' };
let curveSlots = loadCurveSlots();
let curveSlotTarget = 0;

function currentCurveSet() {
  return {
    hdd: curveFromInputs('hdd'),
    nvme: curveFromInputs('nvme'),
    cpu: curveFromInputs('cpu'),
  };
}

// 槽位内容按当前三条曲线的形状校验：任一档不是 2–8 个合法节点就整槽作废，
// 免得坏数据在载入时把曲线编辑器搞崩。
function validCurveSet(curves) {
  if (!curves || typeof curves !== 'object') return null;
  const result = {};
  for (const kind of CURVE_KINDS) {
    const points = curves[kind];
    if (!Array.isArray(points) || points.length < CURVE_MIN_POINTS || points.length > CURVE_MAX_POINTS) return null;
    const parsed = [];
    for (const point of points) {
      const temp = Number(point?.temp_c);
      const pwm = Number(point?.pwm_percent);
      if (!Number.isFinite(temp) || !Number.isFinite(pwm)) return null;
      parsed.push({ temp_c: temp, pwm_percent: pwm });
    }
    result[kind] = parsed;
  }
  return result;
}

function curveSlotKey(slot) {
  return String(slot);
}

// 只有存了曲线的按键才算「已保存」；只改过名的空按键不算。
function curveSlotEntry(slot) {
  const record = curveSlots[curveSlotKey(slot)];
  return record && record.curves ? record : null;
}

function curveSlotRecord(slot) {
  return curveSlots[curveSlotKey(slot)] || null;
}

function curveSlotName(slot) {
  const record = curveSlotRecord(slot);
  return (record && record.name) || CURVE_SLOT_DEFAULT_NAMES[slot];
}

// 旧版本只存了三条曲线本身（没有 name 字段），读进来时补默认名。
function normalizeSlotRecord(item, slot) {
  if (!item || typeof item !== 'object') return null;
  const defaultName = CURVE_SLOT_DEFAULT_NAMES[slot];
  const rawName = typeof item.name === 'string' ? item.name.trim() : '';
  const name = rawName || defaultName;
  const legacyCurves = (item.hdd || item.nvme || item.cpu) ? item : null;
  const curves = validCurveSet(item.curves && typeof item.curves === 'object' ? item.curves : legacyCurves);
  // 名字是默认名、又没有曲线 → 当空按键，不必占位
  if (!curves && name === defaultName) return null;
  return { name, curves: curves || null };
}

function loadCurveSlots() {
  const slots = {};
  try {
    const raw = window.localStorage.getItem(CURVE_SLOT_STORAGE_KEY);
    const parsed = raw ? JSON.parse(raw) : null;
    if (parsed && typeof parsed === 'object') {
      for (let slot = 1; slot <= CURVE_SLOT_COUNT; slot += 1) {
        const record = normalizeSlotRecord(parsed[String(slot)], slot);
        if (record) slots[String(slot)] = record;
      }
    }
  } catch (error) { /* 解析失败＝当作三个空按键 */ }
  return slots;
}

function persistCurveSlots() {
  try {
    window.localStorage.setItem(CURVE_SLOT_STORAGE_KEY, JSON.stringify(curveSlots));
  } catch (error) {
    showMessage('浏览器本地存储不可用（隐私模式？），自定义曲线只在本次会话有效。', true, 'message-fan');
  }
}

function renderCurveSlotStates() {
  for (let slot = 1; slot <= CURVE_SLOT_COUNT; slot += 1) {
    const button = $(`curve-custom-${slot}`);
    if (!button) continue;
    const filled = Boolean(curveSlotEntry(slot));
    button.textContent = curveSlotName(slot);
    // 已保存曲线的按键不加描边/变色：按键只显示名字，有没有内容看弹窗里的说明
    button.title = filled
      ? `打开「${curveSlotName(slot)}」的操作弹窗（已保存曲线）`
      : `「${curveSlotName(slot)}」还是空的：点开后可把当前曲线保存进来，或导入 JSON`;
  }
}

const CURVE_KIND_LABELS = { hdd: 'HDD/SATA', nvme: 'NVMe', cpu: 'CPU' };

function curveSlotSummary(curves) {
  return CURVE_KINDS
    .map((kind) => `${CURVE_KIND_LABELS[kind]} ${curves[kind].length} 点`)
    .join(' · ');
}

function openCurveSlotDialog(slot) {
  if (!slot) return;
  curveSlotTarget = slot;
  const entry = curveSlotEntry(slot);
  const name = curveSlotName(slot);
  $('curve-slot-title').textContent = name;
  $('curve-slot-summary').textContent = entry
    ? `已保存三条曲线：${curveSlotSummary(entry.curves)}。`
    : '这个按键还是空的：可以「保存」当前曲线进来，也可以「导入」一个 JSON 文件。';
  $('curve-slot-apply').disabled = !entry;
  $('curve-slot-apply-save').disabled = !entry;
  $('curve-slot-export').disabled = !entry;
  $('curve-slot-modal').hidden = false;
  requestAnimationFrame(() => { $('curve-slot-apply').focus(); });
}

function closeCurveSlotDialog() {
  curveSlotTarget = 0;
  const modal = $('curve-slot-modal');
  if (modal) modal.hidden = true;
}

// 应用：把按键里的曲线填进编辑器（不动硬件，仍需「保存风扇控制」）
async function applyCurveSlot(slot) {
  const entry = curveSlotEntry(slot);
  if (!entry) {
    showMessage(`「${curveSlotName(slot)}」还是空的，没有可应用的曲线。`, true, 'message-fan');
    return;
  }
  const name = curveSlotName(slot);
  const result = await showAppConfirm({
    title: `应用「${name}」`,
    message: '会用这个按键保存的三条曲线替换编辑器里的节点，界面上还没保存的手动调整也会被覆盖。',
    confirmText: '应用到曲线',
    secondaryText: '应用并保存风扇控制',
  });
  if (!result) return;
  closeCurveSlotDialog();
  backupTempCurves();
  await applyModeCurves(`slot-${slot}`, entry.curves, `已把「${name}」的曲线填进编辑器；点「保存风扇控制」后生效。`);
  if (result.secondary) await saveFanConfig();
}

// 应用并保存：按键里的曲线填进编辑器后立刻下发，不用再回主界面点保存
async function applyCurveSlotAndSave(slot) {
  const entry = curveSlotEntry(slot);
  if (!entry) {
    showMessage(`「${curveSlotName(slot)}」还是空的，没有可应用的曲线。`, true, 'message-fan');
    return;
  }
  const name = curveSlotName(slot);
  closeCurveSlotDialog();
  backupTempCurves();
  await applyModeCurves(`slot-${slot}`, entry.curves, `已应用「${name}」的曲线。`);
  await saveFanConfig();
}

// 保存：用当前三条曲线覆盖按键
async function saveCurveSlot(slot) {
  const name = curveSlotName(slot);
  const existed = Boolean(curveSlotEntry(slot));
  const ok = await showAppConfirm({
    title: existed ? `覆盖「${name}」` : `保存到「${name}」`,
    message: existed
      ? '这个按键里已经有曲线了，保存会用当前三条曲线覆盖它（按键名字不变）。'
      : '会把当前三条曲线（含界面上还没保存的调整）存进这个按键，之后可以随时「应用到曲线」回来。',
    confirmText: existed ? '覆盖保存' : '保存',
  });
  if (!ok) return;
  curveSlots[curveSlotKey(slot)] = { name, curves: currentCurveSet() };
  persistCurveSlots();
  renderCurveSlotStates();
  closeCurveSlotDialog();
  showMessage(`当前三条曲线已存入「${name}」；点「保存风扇控制」后这套曲线才真正生效。`, false, 'message-fan');
}

// ---- 曲线参数导出 / 导入 ---------------------------------------------------
// 导出文件格式（version 2）：
// { app, kind:'fan-curve-slots', slots:[{ slot, name, curves:{hdd,nvme,cpu} }], ... }
// 单键导出只是 slots 里只有一项；旧版单曲线文件（kind:'fan-curves'）也能导入。
function curvePayloadHeader() {
  return {
    app: 'tad-module',
    kind: 'fan-curve-slots',
    version: 2,
    exported_at: new Date().toISOString(),
    app_version: currentStatus?.version || '',
    min_pwm_percent: Number($('fan-min').value),
    emergency_temp_c: Number($('fan-emergency').value),
    poll_seconds: Number($('fan-poll').value),
  };
}

function downloadCurveSlots(slots, filePrefix, doneMessage) {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const payload = Object.assign(curvePayloadHeader(), { slots });
  const text = JSON.stringify(payload, null, 2);
  const blob = new Blob([text], { type: 'application/json;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = `${filePrefix}-${stamp}.json`;
  link.rel = 'noopener';
  document.body.append(link);
  link.click();
  link.remove();
  // 立刻 revoke 会让部分内置浏览器拿到空文件，留几秒再回收
  setTimeout(() => URL.revokeObjectURL(url), 4000);
  showMessage(doneMessage, false, 'message-fan');
}

function exportCurveSlot(slot) {
  const entry = curveSlotEntry(slot);
  if (!entry) {
    showMessage(`「${curveSlotName(slot)}」还是空的，没有可导出的曲线。`, true, 'message-fan');
    return;
  }
  downloadCurveSlots(
    [{ slot, name: entry.name, curves: entry.curves }],
    `tad-fan-curve-custom-${slot}`,
    `「${entry.name}」的曲线参数已导出为 JSON 文件。`,
  );
}

// 已保存曲线的按键（导出、批量时用；空按键不算）
function savedCurveSlots() {
  const slots = [];
  for (let slot = 1; slot <= CURVE_SLOT_COUNT; slot += 1) {
    const entry = curveSlotEntry(slot);
    if (entry) slots.push({ slot, name: entry.name, curves: entry.curves });
  }
  return slots;
}

function parseCurveSlotFile(text) {
  let data = null;
  try { data = JSON.parse(text); } catch (error) { return { error: '不是合法的 JSON 文件。' }; }
  if (!data || typeof data !== 'object') return { error: '文件里没有曲线参数。' };
  if (Array.isArray(data.slots) && data.slots.length) {
    const first = data.slots[0];
    const curves = validCurveSet(first && first.curves);
    if (!curves) return { error: '文件里的曲线节点不合法：每条曲线要 2–8 个点，温度递增、转速不递减。' };
    const name = typeof first.name === 'string' ? first.name.trim() : '';
    return { curves, name, count: data.slots.length };
  }
  // 兼容旧版：{ kind:'fan-curves', curves:{...} } 或直接就是三条曲线
  const curves = validCurveSet(data.curves && typeof data.curves === 'object' ? data.curves : data);
  if (!curves) return { error: '文件里没有可识别的曲线：每条曲线要 2–8 个点，温度递增、转速不递减。' };
  return { curves, name: '', count: 1 };
}

async function importCurveSlotFile(slot, file) {
  if (!slot || !file) return;
  let text = '';
  try {
    text = await file.text();
  } catch (error) {
    showMessage(`读取文件失败：${error.message}`, true, 'message-fan');
    return;
  }
  const parsed = parseCurveSlotFile(text);
  if (parsed.error) {
    showMessage(parsed.error, true, 'message-fan');
    return;
  }
  const name = parsed.name || curveSlotName(slot);
  const extra = parsed.count > 1 ? `\n文件里有 ${parsed.count} 个按键，只导入第一个。` : '';
  const ok = await showAppConfirm({
    title: `导入到「${curveSlotName(slot)}」`,
    message: `会用文件里的三条曲线覆盖这个按键${parsed.name ? `，并把按键改名为「${parsed.name}」` : '（按键名字不变）'}。${extra}`,
    confirmText: '导入',
  });
  if (!ok) return;
  curveSlots[curveSlotKey(slot)] = { name, curves: parsed.curves };
  persistCurveSlots();
  renderCurveSlotStates();
  closeCurveSlotDialog();
  showMessage(`已把文件里的曲线导入「${name}」；点这个按键「应用到曲线」可填进编辑器，再点「保存风扇控制」才会下发。`, false, 'message-fan');
}

async function renameCurveSlot(slot) {
  const current = curveSlotName(slot);
  const value = await showAppConfirm({
    title: '给自定义按键改名',
    message: `当前名字：${current}。改名只影响显示和导出文件里的名字，不动曲线内容。`,
    input: {
      label: '新名字（最多 16 个字）',
      value: current === CURVE_SLOT_DEFAULT_NAMES[slot] ? '' : current,
      placeholder: CURVE_SLOT_DEFAULT_NAMES[slot],
      maxLength: 16,
    },
    confirmText: '改名',
  });
  if (value === null || value === undefined) return;
  // 留空不报错：直接退回这个按键的默认名（自定义 1/2/3），符合「清空即复位」的直觉
  const fallback = CURVE_SLOT_DEFAULT_NAMES[slot];
  const name = String(value).trim() || fallback;
  const key = curveSlotKey(slot);
  const record = curveSlotRecord(slot);
  if (record) {
    record.name = name;
  } else if (name !== fallback) {
    curveSlots[key] = { name, curves: null };
  }
  persistCurveSlots();
  renderCurveSlotStates();
  $('curve-slot-title').textContent = name;
  showMessage(name === fallback
    ? `没有填写内容，按键已恢复默认名「${name}」。`
    : `按键已改名为「${name}」。`, false, 'message-fan');
}

function setupCurvePresets() {
  tempCurveBackup = loadTempCurveBackup();
  document.querySelectorAll('[data-curve-mode]').forEach((button) => {
    const mode = button.dataset.curveMode;
    button.addEventListener('click', () => {
      if (mode === 'quiet' || mode === 'balanced' || mode === 'performance') {
        applyCurvePreset(mode);
      } else if (mode === 'full') {
        applyFullSpeed();
      } else if (mode === 'off') {
        applyShutdown();
      } else if (mode === 'manual') {
        applyManualSpeed();
      } else {
        applyTempMode();
      }
    });
  });
  for (let slot = 1; slot <= CURVE_SLOT_COUNT; slot += 1) {
    const loadButton = $(`curve-custom-${slot}`);
    if (loadButton) loadButton.addEventListener('click', () => openCurveSlotDialog(slot));
  }
  $('curve-slot-apply').addEventListener('click', () => applyCurveSlot(curveSlotTarget));
  $('curve-slot-apply-save').addEventListener('click', () => applyCurveSlotAndSave(curveSlotTarget));
  $('curve-slot-save').addEventListener('click', () => saveCurveSlot(curveSlotTarget));
  $('curve-slot-export').addEventListener('click', () => exportCurveSlot(curveSlotTarget));
  $('curve-slot-rename').addEventListener('click', () => renameCurveSlot(curveSlotTarget));
  $('curve-slot-import').addEventListener('click', () => $('curve-slot-file').click());
  $('curve-slot-file').addEventListener('change', (event) => {
    const file = event.target.files && event.target.files[0];
    event.target.value = '';
    importCurveSlotFile(curveSlotTarget, file);
  });
  document.querySelectorAll('[data-curve-slot-close]').forEach((element) => {
    element.addEventListener('click', closeCurveSlotDialog);
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && !$('curve-slot-modal').hidden) closeCurveSlotDialog();
  });
  renderCurveSlotStates();
  renderCurveModeStates();
}

function svgElement(name, attributes = {}, text = '') {
  const element = document.createElementNS('http://www.w3.org/2000/svg', name);
  Object.entries(attributes).forEach(([key, value]) => element.setAttribute(key, String(value)));
  if (text) element.textContent = text;
  return element;
}

const CURVE_VIEWBOX = { width: 640, height: 260 };
const CURVE_TEXT_PX = 12;

function curveChartScale(svg) {
  const width = svg?.clientWidth || 0;
  const height = svg?.clientHeight || 0;
  if (!width || !height) return 1;
  return Math.max(0.25, Math.min(width / CURVE_VIEWBOX.width, height / CURVE_VIEWBOX.height));
}

// 轴刻度换算成 viewBox 单位后的实际宽度：窄屏时 SVG 内文字字号按 1/chartScale
// 放大以保持物理尺寸，留白必须按同一比例跟着放大。用 canvas 按页面字体栈测量。
let historyAxisMeasureCtx = null;
function historyAxisTextWidth(text, fontSize) {
  if (!historyAxisMeasureCtx) historyAxisMeasureCtx = document.createElement('canvas').getContext('2d');
  historyAxisMeasureCtx.font = `${fontSize}px "PingFang SC", "Microsoft YaHei UI", "Microsoft YaHei", system-ui, sans-serif`;
  return historyAxisMeasureCtx.measureText(text).width;
}

function updateCurveChartTextScale(svg) {
  if (!svg) return;
  const scale = curveChartScale(svg);
  const fontSize = CURVE_TEXT_PX / scale;
  svg.querySelectorAll('text').forEach((text) => text.setAttribute('font-size', fontSize));
  svg.querySelectorAll('.curve-node-label').forEach((label) => {
    // CSS class sets a presentation default; inline user-unit style wins and
    // keeps the halo at a steady visual width as the SVG is resized.
    label.style.strokeWidth = `${4 / scale}`;
  });
}

function renderFanChart(kind = 'cpu') {
  const editor = curveEditors[kind];
  const svg = $(editor.chartID);
  svg.classList.add(`curve-chart-${kind}`);
  const curve = curveFromInputs(kind);
  if (curve.some((point) => !Number.isFinite(point.temp_c) || !Number.isFinite(point.pwm_percent))) return;
  const { right, top, bottom } = CHART;
  const chartScale = curveChartScale(svg);
  const textSize = CURVE_TEXT_PX / chartScale;
  // 左轴最宽刻度固定是 100%；窄屏下 SVG 内字号放大，左留白同步放大，
  // 否则转速刻度被 viewBox 裁掉。三个曲线编辑器几何一致，fanPlotBox 共用。
  const axisPad = 8 / chartScale;
  const left = Math.max(CHART.left, axisPad + historyAxisTextWidth('100%', textSize) + 2 / chartScale);
  fanPlotBox = { left, right, top, bottom };
  const x = (temp) => left + ((clamp(temp, 20, 100) - 20) / 80) * (right - left);
  const y = (speed) => bottom - ((clamp(speed, 30, 100) - 30) / 70) * (bottom - top);
  svg.replaceChildren();

  // 横轴标签行高随字号放大，低于绘图区底线时下移，避免顶到网格线；
  // 标签行与纵轴刻度有垂直重叠时，端部标签压到刻度列就向图内锚定
  const xLabelBase = Math.max(CURVE_VIEWBOX.height - 14 / chartScale, bottom + textSize * 0.75 + 1 / chartScale);
  const xRowNearGutter = xLabelBase < bottom + textSize * 1.07;
  [20, 40, 60, 80, 100].forEach((temp) => {
    svg.append(svgElement('line', { x1: x(temp), y1: top, x2: x(temp), y2: bottom, class: 'chart-grid-line' }));
    const label = `${temp}°`;
    const lx = x(temp);
    const halfLabel = historyAxisTextWidth(label, textSize) / 2;
    let anchor = 'middle';
    if (xRowNearGutter && lx - halfLabel < left - axisPad) anchor = 'start';
    else if (xRowNearGutter && lx + halfLabel > right + axisPad) anchor = 'end';
    svg.append(svgElement('text', { x: lx, y: xLabelBase, class: 'chart-axis-text', 'font-size': textSize, 'text-anchor': anchor }, label));
  });
  [30, 50, 70, 100].forEach((speed) => {
    svg.append(svgElement('line', { x1: left, y1: y(speed), x2: right, y2: y(speed), class: 'chart-grid-line' }));
    svg.append(svgElement('text', { x: left - axisPad, y: y(speed) + textSize * 0.35, class: 'chart-axis-text', 'font-size': textSize, 'text-anchor': 'end' }, `${speed}%`));
  });
  // 线宽与节点尺寸除以 chartScale：viewBox 会被容器拉伸，除以缩放后才是目标真实像素（同文字的处理）
  svg.append(svgElement('polyline', {
    points: curve.map((point) => `${x(point.temp_c)},${y(point.pwm_percent)}`).join(' '),
    fill: 'none', stroke: editor.color, 'stroke-width': 2.5 / chartScale, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
  }));
  const actualTemperatures = {
    cpu: currentStatus?.fan_control?.cpu_temperature_c ?? currentStatus?.fan_control?.temperature_c,
    hdd: currentStatus?.fan_control?.hdd_temperature_c ?? currentStatus?.fan_control?.disk_temperature_c,
    nvme: currentStatus?.fan_control?.nvme_temperature_c,
  };
  const actualTemp = Number(actualTemperatures[kind]);
  if (Number.isFinite(actualTemp) && actualTemp > 0) {
    const currentLabel = `当前 ${actualTemp.toFixed(1)}°C`;
    const currentLabelWidth = Math.max(72 / chartScale, currentLabel.length * textSize * 0.6);
    const currentX = clamp(x(actualTemp), currentLabelWidth / 2 + 4 / chartScale, CURVE_VIEWBOX.width - currentLabelWidth / 2 - 4 / chartScale);
    svg.append(svgElement('line', { x1: x(actualTemp), y1: top, x2: x(actualTemp), y2: bottom, class: 'chart-now-line', 'stroke-width': 1.5 / chartScale, 'stroke-dasharray': '6 5' }));
    svg.append(svgElement('text', { x: currentX, y: textSize + 4 / chartScale, class: 'chart-now-label', 'font-size': textSize, 'text-anchor': 'middle' }, currentLabel));
  }
  curve.forEach((point, index) => {
    const selected = index === editor.selectedIndex;
    const nodeX = x(point.temp_c);
    const nodeY = y(point.pwm_percent);
    const hitTarget = svgElement('circle', {
      cx: nodeX, cy: nodeY, r: 20,
      fill: 'transparent', stroke: 'transparent', 'pointer-events': 'all',
      class: 'curve-node curve-node-hit', 'data-index': index,
      'aria-hidden': true, focusable: false,
    });
    svg.append(hitTarget);
    const node = svgElement('circle', {
      // 真实半径约 4px（选中 5px）；命中热区 r=20 刻意不缩放，保证触控
      cx: nodeX, cy: nodeY, r: (selected ? 5 : 4) / chartScale,
      stroke: editor.color, 'stroke-width': 2 / chartScale,
      class: `curve-node curve-node-control${selected ? ' selected' : ''}`, 'data-index': index,
      tabindex: 0, role: 'button', 'aria-label': `节点 ${index + 1}，${point.temp_c} 摄氏度，转速 ${point.pwm_percent}%`,
    });
    svg.append(node);
    const label = `${point.temp_c}° · ${point.pwm_percent}%`;
    const labelWidth = Math.max(72 / chartScale, label.length * textSize * 0.6);
    const labelX = clamp(nodeX, 8 / chartScale + labelWidth / 2, CURVE_VIEWBOX.width - 8 / chartScale - labelWidth / 2);
    const labelY = nodeY < 50 / chartScale ? nodeY + 26 / chartScale : nodeY - 16 / chartScale;
    svg.append(svgElement('text', {
      x: labelX, y: labelY,
      'font-size': 12, 'font-weight': selected ? 800 : 600, 'text-anchor': 'middle', class: `curve-node-label${selected ? ' selected' : ''}`,
    }, label));
  });
  updateCurveChartTextScale(svg);
  updateCurveControls(kind);
}

/* ---- 历史温度：后台每分钟采样（GET api/history?range=小时数），服务端按峰值聚合 ---- */

// 父类分组：勾选父类 = 整组曲线显隐；▾ 弹窗勾选组内子类（CPU/SATA/NVMe 弹窗
// 内含“取最高”聚合项）。
const HISTORY_GROUPS = [
  { key: 'cpu', label: 'CPU', color: '#3f6ff5', aggregateLabel: 'CPU最高' },
  { key: 'sata', label: 'SATA', color: '#18a779', aggregateLabel: 'SATA最高' },
  { key: 'nvme', label: 'NVMe', color: '#a25bd7', aggregateLabel: 'NVMe最高' },
  { key: 'gpu', label: 'GPU', color: '#e05252' },
  { key: 'nic', label: '网卡', color: '#e6a53c' },
  { key: 'other', label: '其它', color: '#2aa8a8' },
  { key: 'fan', label: '风扇', color: '#6e7780' }, // 中性灰：与温度传感器色系（蓝/绿/紫/红/琥珀/青）明显区分
];
const HISTORY_DEFAULT_HOURS = 0.5; // 默认 30 分钟档；用户选过则优先用记住的档位
const HISTORY_FETCH_TTL = 45000;
const HISTORY_RANGE_STORAGE_KEY = 'tad-history-range';
// 子类色从父类色派生：同色相、明度阶梯 ±8%、小幅交替色相偏移，
// 保证同组曲线同色系且可区分。
function historyHexToHsl(hex) {
  const value = hex.replace('#', '');
  const r = parseInt(value.slice(0, 2), 16) / 255;
  const g = parseInt(value.slice(2, 4), 16) / 255;
  const b = parseInt(value.slice(4, 6), 16) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  if (max === min) return { h: 0, s: 0, l };
  const d = max - min;
  const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
  let h;
  if (max === r) h = ((g - b) / d + (g < b ? 6 : 0)) / 6;
  else if (max === g) h = ((b - r) / d + 2) / 6;
  else h = ((r - g) / d + 4) / 6;
  return { h: h * 360, s, l };
}

function historyChildShade(baseHex, index) {
  const { h, s, l } = historyHexToHsl(baseHex);
  const step = Math.ceil((index + 1) / 2) * 0.08; // 0.08, 0.16, 0.24 …
  const lighten = index % 2 === 0;
  const lightness = Math.min(0.85, Math.max(0.15, l + (lighten ? step : -step)));
  const hue = (h + (lighten ? 1 : -1) * Math.min(Math.ceil((index + 1) / 2), 3) * 5 + 360) % 360;
  return `hsl(${Math.round(hue)}, ${Math.round(Math.max(0.25, s) * 100)}%, ${Math.round(lightness * 100)}%)`;
}

// 风扇组子类不走 HSL 派生：灰色基色会被饱和度下限（0.25）强行染色，
// 改用独立灰色阶梯按子类序号取色。前四档（TAD6S4N 四风扇）保证深浅主题
// 都清晰：深色背景 rgb(20,21,25) 下过暗的灰（如 #4a5058）会与网格线混在一起。
const HISTORY_FAN_SHADES = ['#6e7780', '#97a0ab', '#b6bec8', '#8a919c', '#aab2bc', '#5b626b', '#cfd4da', '#454c54'];
const HISTORY_FAN_MAX_PERCENT = 100;

let historyCache = null;
let historyFetchedAt = 0;
// 先给默认值，真实档位在下方 HISTORY_ALL_STOPS 等常量声明完之后再加载：
// loadHistoryRangeHours 依赖的 normalize 会读那些 const（无提升），在声明前
// 调用会抛 ReferenceError 被 try/catch 吞掉——localStorage 记住的档位就失效了
let historyRangeHours = HISTORY_DEFAULT_HOURS;
let historyRangedSamples = [];   // 当前范围采样（与图上折线一致）
let historyYBoundsState = { lo: 20, hi: 90 }; // 渲染时缓存，供十字线取点
let historyPlotBox = { ...CHART }; // 渲染时缓存绘图区（窄屏下左右边距会随字号放大），供十字线换算
let fanPlotBox = { ...CHART }; // 曲线编辑器渲染时缓存绘图区（窄屏下左边距随字号放大），供拖拽换算
let historyLastCursorEvent = null; // 最后悬停位置，自动刷新重绘后恢复十字线
let historyOpenPopoverKey = null; // 当前展开的父类弹窗
let historyLegendIdentity = '';   // 图例内容标识，未变化时不重建（保护展开弹窗）

let historySeriesEnabled = loadHistorySeriesEnabled(); // 父类开关；未记录的组走默认（见 defaultHistoryGroupEnabled）
// 历史曲线显隐默认（用户确认）：七个父类默认全开；CPU/SATA/NVMe 只画「最高」
// 聚合线，GPU 全画，网卡与「其它」默认只画最关心的两个传感器（见下面的
// defaultHistoryChildSelection）——一张图不被几十条明细曲线铺满。
function defaultHistoryGroupEnabled(groupKey) {
  return HISTORY_GROUPS.some((group) => group.key === groupKey);
}
// 只读访问器（渲染与测试用）：直接写 historySeriesEnabled 的路径集中在 setHistoryGroupEnabled。
function historySeriesEnabledFor(groupKey) {
  const value = historySeriesEnabled[groupKey];
  return value === undefined ? defaultHistoryGroupEnabled(groupKey) : value;
}
let historyChildSelection = loadHistoryChildSelection(); // 组内勾选；未记录的组走默认（见 defaultHistoryChildSelection）

// 默认勾选改版（v2）：CPU/SATA/NVMe 改名「…最高」、GPU/网卡/其它默认打开。
// 老浏览器里存着上一版偏好（只开 CPU/SATA/NVMe、其它组不画），不清掉就永远
// 看不到新默认，所以按版本号一次性清空显隐与组内勾选，并把新默认推回后端。
const HISTORY_DEFAULTS_VERSION = 2;
const HISTORY_DEFAULTS_VERSION_KEY = 'tad-history-defaults-version';
let historyDefaultsMigrated = false;

function migrateHistoryDefaults() {
  let stored = 0;
  let hadLocalPrefs = false;
  try {
    stored = Number(window.localStorage.getItem(HISTORY_DEFAULTS_VERSION_KEY)) || 0;
    hadLocalPrefs = Boolean(
      window.localStorage.getItem('tad-history-series')
      || window.localStorage.getItem('tad-history-children')
      || window.localStorage.getItem('tad-history-disks'),
    );
  } catch (error) {
    stored = 0;
  }
  if (stored >= HISTORY_DEFAULTS_VERSION) return false;
  try { window.localStorage.setItem(HISTORY_DEFAULTS_VERSION_KEY, String(HISTORY_DEFAULTS_VERSION)); }
  catch (error) { /* 隐私模式下写不进去：本会话仍按新默认画 */ }
  // 全新浏览器没有旧偏好要清，直接记版本号：让服务端 ui_prefs 照常采纳，
  // 换设备看到的仍是账号里的勾选。
  if (!hadLocalPrefs) return false;
  historySeriesEnabled = {};
  historyChildSelection = {};
  saveHistorySeriesEnabled();
  saveHistoryChildSelection();
  historyDefaultsMigrated = true;
  return true;
}

migrateHistoryDefaults();

// ---- 窗口式回看（30 分钟–24 小时各档，监控录像机式拖动窗口）----
// 横轴永远只显示所选档位的时间跨度（窗口），背后拉取约 8 倍跨度的数据，
// 在图上左右拖动即回看更早的记录，拖回最右跟随实时。
// 拉取密度：窗口式请求带 max_points=2000（服务端聚合桶取峰值），24 小时
// 窗口约 240 点（6 分钟/点）；30 分钟档拉 7.5 小时分钟级原始数据（451 点，
// 落在默认 480 上限内不聚合）。7 天/30 天仍为全览，不走窗口。
const HISTORY_SCRUB_FETCH_HOURS = 7.5;   // 30 分钟档拉取跨度
const HISTORY_WINDOW_MAX_HOURS = 24;     // ≤24 小时的档位均为窗口式
const HISTORY_WINDOW_LOOKBACK = 8;       // 窗口式档位拉取 8 倍跨度回看
const HISTORY_WINDOW_MAX_POINTS = 2000;  // 窗口式拉取的 max_points（服务端钳 10–2000）
let historyScrubEndTs = null;      // 回看窗口末端采样 ts；null=贴最新（实时跟随）
let historyIntervalSeconds = 60;   // 渲染时缓存的服务端采样间隔，拖动吸附换算用
let historyXDomain = { start: 0, end: 0 }; // 渲染时缓存的横轴时间域，供十字线换算
let historyScrubDragging = false;  // 拖动中：抑制十字线与提示框

function loadHistorySeriesEnabled() {
  try {
    const raw = JSON.parse(window.localStorage.getItem('tad-history-series') || '{}');
    return raw && typeof raw === 'object' ? raw : {};
  } catch (error) { return {}; }
}

function saveHistorySeriesEnabled() {
  try { window.localStorage.setItem('tad-history-series', JSON.stringify(historySeriesEnabled)); }
  catch (error) { /* 忽略写入失败 */ }
}

function loadHistoryChildSelection() {
  try {
    const raw = JSON.parse(window.localStorage.getItem('tad-history-children') || '{}');
    const state = raw && typeof raw === 'object' ? raw : {};
    // 迁移旧的单盘勾选（sata/nvme 组）
    if (!state.sata && !state.nvme) {
      const legacy = JSON.parse(window.localStorage.getItem('tad-history-disks') || 'null');
      if (Array.isArray(legacy)) {
        state.sata = legacy.filter((id) => id.startsWith('front-'));
        state.nvme = legacy.filter((id) => id.startsWith('m2-'));
      }
    }
    return state;
  } catch (error) { return {}; }
}

function saveHistoryChildSelection() {
  try {
    const plain = {};
    for (const [key, value] of Object.entries(historyChildSelection)) {
      plain[key] = value === null ? null : [...value];
    }
    window.localStorage.setItem('tad-history-children', JSON.stringify(plain));
  } catch (error) { /* 忽略写入失败 */ }
}

// ---- 时间范围：拖动条（30分–2时无极 + 固定挡位，7天/30天并入挡位）----

// 滑杆双段设计（用户确认）：前 30% 是 30 分钟–2 小时的无极区（按 1 分钟
// 粒度吸附，0.3 分钟/位），后 70% 每 140 位一个固定挡位（6时/12时/24时 +
// 按保存天数显隐的 7天/30天），拇指落入挡位段即吸附到段中心的停靠位，
// 刻度画在停靠位上。挡位总数由 updateHistoryRangeAvailability 维护。
const HISTORY_CONT_POSITIONS = 400; // 无极区占用的滑杆位置数（总行程的 40%）
const HISTORY_STOP_SPAN = 120;      // 每个固定挡位占用的位置数
const HISTORY_CONT_MIN_HOURS = 0.5;
const HISTORY_CONT_MAX_HOURS = 2;
const HISTORY_BASE_STOPS = [6, 12, 24];
const HISTORY_ALL_STOPS = [6, 12, 24, 168, 720];

// 档位记忆的加载点：依赖上面的常量都已声明（见顶部 historyRangeHours 的说明）
historyRangeHours = loadHistoryRangeHours();

// 当前滑杆上的固定挡位数量（3=仅基础挡；保存天数 ≥7 加 7 天，≥30 加 30 天）
let historySliderStopCount = HISTORY_ALL_STOPS.length;

function historyStopsFor(stopCount) {
  return HISTORY_ALL_STOPS.slice(0, clamp(Math.round(stopCount) || HISTORY_ALL_STOPS.length, 3, HISTORY_ALL_STOPS.length));
}

// 挡位序号 → 拇指停靠位（挡位段中心）
function historyStopCenterPos(stopIndex) {
  return HISTORY_CONT_POSITIONS + stopIndex * HISTORY_STOP_SPAN + HISTORY_STOP_SPAN / 2;
}

// 滑杆位置 → 小时。无极区按 1 分钟粒度吸附；挡位区整段吸附到对应挡位。
function historyPosToHours(pos) {
  const max = historySliderStopCount > 0 ? historyStopCenterPos(historySliderStopCount - 1) : HISTORY_CONT_POSITIONS;
  const position = Math.round(clamp(Number(pos) || 0, 0, max));
  if (position <= HISTORY_CONT_POSITIONS) {
    const minutes = Math.round(HISTORY_CONT_MIN_HOURS * 60 + (position / HISTORY_CONT_POSITIONS) * (HISTORY_CONT_MAX_HOURS - HISTORY_CONT_MIN_HOURS) * 60);
    return minutes / 60;
  }
  const stops = historyStopsFor(historySliderStopCount);
  const index = Math.min(stops.length - 1, Math.floor((position - HISTORY_CONT_POSITIONS - 1) / HISTORY_STOP_SPAN));
  return stops[index];
}

// 小时 → 滑杆位置。无极区反解到吸附位（1 分钟粒度往返一致）；挡位取停靠位。
function historyHoursToPos(hours) {
  const value = Number(hours);
  if (!Number.isFinite(value)) return 0;
  if (value <= HISTORY_CONT_MAX_HOURS) {
    const minutes = clamp(value, HISTORY_CONT_MIN_HOURS, HISTORY_CONT_MAX_HOURS) * 60;
    return Math.round((minutes - HISTORY_CONT_MIN_HOURS * 60) / ((HISTORY_CONT_MAX_HOURS - HISTORY_CONT_MIN_HOURS) * 60 / HISTORY_CONT_POSITIONS));
  }
  const stops = historyStopsFor(historySliderStopCount);
  let best = 0;
  let bestDelta = Infinity;
  stops.forEach((stop, index) => {
    const delta = Math.abs(stop - value);
    if (delta < bestDelta) { best = index; bestDelta = delta; }
  });
  return historyStopCenterPos(best);
}

// 范围值钳制到合法值：7 天/30 天原样保留；≤2 小时按 1 分钟粒度钳进无极区；
// 2 小时以上的旧值（含旧版半小时值）吸附到最近固定挡位。这里用全套挡位，
// 与保存天数显隐无关——超出保留期的值由 updateHistoryRangeAvailability 回落。
function normalizeHistoryRangeHours(hours) {
  const value = Number(hours);
  if (Number.isFinite(value) && (value === 168 || value === 720)) return value;
  if (!Number.isFinite(value)) return HISTORY_DEFAULT_HOURS;
  if (value <= HISTORY_CONT_MAX_HOURS) {
    const minutes = clamp(value, HISTORY_CONT_MIN_HOURS, HISTORY_CONT_MAX_HOURS) * 60;
    return Math.round(minutes) / 60;
  }
  let best = HISTORY_BASE_STOPS[0];
  let bestDelta = Infinity;
  HISTORY_ALL_STOPS.forEach((stop) => {
    const delta = Math.abs(stop - value);
    if (delta < bestDelta) { best = stop; bestDelta = delta; }
  });
  return best;
}

function loadHistoryRangeHours() {
  try {
    const raw = window.localStorage.getItem(HISTORY_RANGE_STORAGE_KEY);
    if (raw === null || raw === '') return HISTORY_DEFAULT_HOURS;
    return normalizeHistoryRangeHours(parseFloat(raw));
  } catch (error) { return HISTORY_DEFAULT_HOURS; }
}

function saveHistoryRangeHours(hours) {
  try { window.localStorage.setItem(HISTORY_RANGE_STORAGE_KEY, String(hours)); }
  catch (error) { /* 隐私模式等场景下仅本次生效 */ }
  // 档位同时记到后端配置（随 /api/status 的 config.ui_prefs 下发，跨设备一致）
  saveUIPrefsQuietly();
}

// ui_prefs 是整段替换：任何保存点都必须带上全部四个字段（档位/调试卡显隐/
// 历史曲线父类开关/组内勾选），漏带会把其它偏好抹回默认。overrides 用于
// 保存点自身的新值尚未落到全局变量时（如风扇卡开关的先改后存）。
function uiPrefsBody(overrides = {}) {
  const children = {};
  for (const [groupKey, selection] of Object.entries(historyChildSelection)) {
    children[groupKey] = selection === null ? null : [...selection]; // null=全部；空数组=组开但一条不画
  }
  return {
    history_range_hours: historyRangeHours,
    fan_debug_visible: uiFanDebugVisible,
    history_series: { ...historySeriesEnabled },
    history_children: children,
    ...overrides,
  };
}

// 勾选/档位类偏好保存：失败静默——localStorage 镜像兜底，不打扰用户。
function saveUIPrefsQuietly(overrides = {}) {
  try {
    return request('api/config/ui-prefs', { method: 'POST', body: JSON.stringify(uiPrefsBody(overrides)) }).catch(() => {});
  } catch (error) { return Promise.resolve(); }
}

// 范围标签：不足 1 小时显示分钟；无极区非整小时显示"N 小时 M 分"；
// 固定挡位显示 N 小时 / N 天。
function historyRangeLabel(hours) {
  if (hours === 168) return '7 天';
  if (hours === 720) return '30 天';
  if (hours < 1) return `${Math.round(hours * 60)} 分钟`;
  if (Number.isInteger(hours)) return `${hours} 小时`;
  const whole = Math.floor(hours);
  const minutes = Math.round((hours - whole) * 60);
  return minutes > 0 ? `${whole} 小时 ${minutes} 分` : `${whole} 小时`;
}

// 组内默认（用户确认）：CPU/SATA/NVMe 只勾「最高」聚合项（__agg__）；
// GPU 全部显示（null）；网卡默认两条业务口/光模块；「其它」默认主板与 PCB 温度。
// 未记录的组（新设备/清缓存/默认版本升级）走这里，显式记录优先。
const HISTORY_DEFAULT_CHILDREN = {
  cpu: ['__agg__'],
  sata: ['__agg__'],
  nvme: ['__agg__'],
  nic: ['mlx5:sensor0', 'mlx5:Module1'],
  other: ['spd5118:temp1', 'it8613:temp2'],
};

function defaultHistoryChildSelection(groupKey) {
  const ids = HISTORY_DEFAULT_CHILDREN[groupKey];
  return ids ? new Set(ids) : null;
}

function historyChildSelectionFor(groupKey) {
  const stored = historyChildSelection[groupKey];
  if (stored === undefined) return defaultHistoryChildSelection(groupKey);
  if (stored === null) return null; // 显式"全部显示"
  return stored instanceof Set ? stored : new Set(stored); // 统一为 Set，调用方用 has()
}

// 生效的勾选：走默认值、而默认键在这台机器上一条都不存在时（换了网卡、
// 没插光模块），退回「全部显示」——否则整组默认空着，看起来像坏了。
// 用户自己勾过的组不猜（哪怕勾成空集合也是他的选择）。
function historyEffectiveSelection(groupKey, childIDs) {
  const selection = historyChildSelectionFor(groupKey);
  if (selection === null) return null;
  if (historyChildSelection[groupKey] === undefined && !childIDs.some((id) => selection.has(id))) {
    return null;
  }
  return selection;
}

function setHistoryChildSelection(groupKey, selection) {
  historyChildSelection[groupKey] = selection;
  saveHistoryChildSelection();
  saveUIPrefsQuietly();
}

// 风扇 ID 缩写：it8613:it87.2608:fan3 → fan3
function historyFanLabel(fanID) {
  return String(fanID || '').slice(String(fanID).lastIndexOf(':') + 1);
}

// 保留最近 rangeHours 小时内的采样（ts 为 unix 秒）。
function historyFilterRange(samples, rangeHours, nowTs) {
  const cutoff = nowTs - rangeHours * 3600;
  return samples.filter((sample) => sample.ts >= cutoff);
}

// 窗口式档位的拉取范围：30 分钟档取 7.5 小时原始数据，其余 ≤24 小时档位
// 拉 8 倍跨度供拖动回看；7 天/30 天全览原样。
function historyIsWindowed(rangeHours) {
  return rangeHours <= HISTORY_WINDOW_MAX_HOURS;
}

// 当前档位的历史拉取地址；窗口式档位带大 max_points 保住窗口内密度
function historyRequestUrl() {
  return historyIsWindowed(historyRangeHours)
    ? `api/history?range=${historyFetchHoursFor(historyRangeHours)}&max_points=${HISTORY_WINDOW_MAX_POINTS}`
    : `api/history?range=${historyFetchHoursFor(historyRangeHours)}`;
}

function historyFetchHoursFor(rangeHours) {
  if (rangeHours === 0.5) return HISTORY_SCRUB_FETCH_HOURS;
  if (historyIsWindowed(rangeHours)) return Math.min(720, rangeHours * HISTORY_WINDOW_LOOKBACK);
  return rangeHours;
}

// 回看窗口：横轴固定覆盖 spanSeconds 时间跨度，末端吸附采样网格（以最旧
// 样本为基准）并夹在 [最旧+span, 最新] 内。数据不足一个窗口时窗口覆盖全部
// 数据。纯函数，node --test 直接断言。
function historyScrubWindow(samples, intervalSeconds, endTs, spanSeconds) {
  const interval = Math.max(1, intervalSeconds);
  if (!samples.length) return { start: 0, end: 0, atLive: true };
  const oldest = samples[0].ts;
  const latest = samples[samples.length - 1].ts;
  const span = Math.max(interval, spanSeconds);
  if (latest - oldest < span) return { start: oldest, end: latest, atLive: true };
  const snapped = oldest + Math.round((endTs - oldest) / interval) * interval;
  const end = Math.min(latest, Math.max(oldest + span, snapped));
  return { start: end - span, end, atLive: end >= latest };
}

// 兜底抽稀（正常情况下服务端已聚合到 ≤480 点）：超出上限按固定步长取样
// 并始终保留最后一个点。
function historyThinOut(samples, maxPoints = 480) {
  if (samples.length <= maxPoints) return samples.slice();
  const stride = Math.ceil(samples.length / maxPoints);
  const out = [];
  for (let i = 0; i < samples.length; i += stride) out.push(samples[i]);
  const last = samples[samples.length - 1];
  if (out[out.length - 1] !== last) out.push(last);
  return out;
}

// 渲染抽稀：2 小时以上的范围按 5 抽 1、6 小时以上按 15 抽 1 收敛折线点数
// （每条曲线少一个数量级的 DOM 节点）；2 小时以内（无极区）保持全精度。
// 必须在断口切分之后、按段执行——先抽稀会把点距拉大过断口阈值，停机断口
// 会被误并回连续线。
function historyDecimationStride(rangeHours) {
  if (rangeHours > 6) return 15;
  if (rangeHours > 2) return 5;
  return 1;
}

// 按步长抽稀一段折线：保留第 0/stride/2×stride… 个点，且始终保留最后一个
// 点（曲线末端反映最新状态）。
function historyThinByStride(samples, stride) {
  if (stride <= 1 || samples.length <= stride) return samples;
  const out = samples.filter((_, index) => index % stride === 0);
  const last = samples[samples.length - 1];
  if (out[out.length - 1] !== last) out.push(last);
  return out;
}

// 数据断口（插件停运/重启）：相邻点间隔超过 2.5 倍采样周期时断开折线。
// 必须在抽稀前按原始间隔判断，抽稀会人为拉大点距。
function historySplitSegments(samples, intervalSeconds) {
  const segments = [];
  let current = [];
  for (let i = 0; i < samples.length; i++) {
    if (current.length && samples[i].ts - current[current.length - 1].ts > intervalSeconds * 2.5) {
      segments.push(current);
      current = [];
    }
    current.push(samples[i]);
  }
  if (current.length) segments.push(current);
  return segments;
}

// 生成覆盖 [lo, hi] 的好看刻度（步长取 1/2/5 × 10^n）。
function historyNiceTicks(lo, hi, targetCount = 5) {
  if (!(hi > lo)) return [];
  const rawStep = (hi - lo) / Math.max(1, targetCount);
  const magnitude = Math.pow(10, Math.floor(Math.log10(rawStep)));
  let step = magnitude;
  for (const multiplier of [1, 2, 5, 10]) {
    if (magnitude * multiplier >= rawStep) { step = magnitude * multiplier; break; }
  }
  const ticks = [];
  for (let value = Math.ceil(lo / step) * step; value <= hi + 1e-9; value += step) {
    ticks.push(Math.round(value * 1e6) / 1e6);
  }
  return ticks;
}

// 温度轴范围：无数据用兜底区间；有数据时下界再放 5 度并按 5 度取整
// （最低 35 度 → 从 30 开始），上界留 2 度余量。
function historyYBounds(values, fallbackLo, fallbackHi) {
  const defined = values.filter((value) => Number.isFinite(value) && value > 0);
  if (!defined.length) return { lo: fallbackLo, hi: fallbackHi };
  const min = Math.min(...defined);
  const max = Math.max(...defined);
  const lo = Math.max(0, Math.floor((min - 5) / 5) * 5);
  let hi = Math.ceil((max + 2) / 5) * 5;
  if (hi - lo < 10) hi = lo + 10;
  return { lo, hi };
}

// 时间轴刻度：各档位固定步长（30 分钟→1 分钟、1 小时→10 分钟、2 小时→30 分钟、
// 6 小时→1 小时、12 小时→3 小时、24 小时→2 小时、7 天→6 小时、30 天→1 天），
// 未知范围退回自动算法。2/12 小时步长按 4–5 条网格线取，与其它档位密度一致。
const HISTORY_TICK_STEP_SECONDS = { 0.5: 60, 1: 600, 2: 1800, 6: 3600, 12: 10800, 24: 7200, 168: 21600, 720: 86400 };
function historyTimeTicks(startSec, endSec, rangeHours) {
  if (!(endSec > startSec)) return [];
  let step = HISTORY_TICK_STEP_SECONDS[rangeHours];
  if (!step) {
    const steps = [60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 14400, 21600, 43200, 86400, 172800, 259200, 432000];
    step = steps[steps.length - 1];
    for (const candidate of steps) {
      if ((endSec - startSec) / candidate <= 7) { step = candidate; break; }
    }
  }
  const ticks = [];
  for (let ts = Math.ceil(startSec / step) * step; ts <= endSec; ts += step) ticks.push(ts);
  return ticks;
}

function historyFormatClock(ts) {
  const date = new Date(ts * 1000);
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

// 刻度步长 ≥ 1 天时显示日期（如 9/15），否则显示时分。
function historyTickLabel(ts, stepSeconds) {
  if (stepSeconds >= 86400) {
    const date = new Date(ts * 1000);
    return `${date.getMonth() + 1}/${date.getDate()}`;
  }
  return historyFormatClock(ts);
}

// 单系列转点集：value 缺失（0/NaN，休眠或空置）的点直接剔除。
// 风扇组例外：0% 是关停档的正常读数，当成缺失会让线在停转那一刻断掉（整段 0 干脆没了）。
function historySeriesPoints(samples, pick, allowZero = false) {
  return samples.filter((sample) => {
    const value = pick(sample);
    return Number.isFinite(value) && (allowZero ? value >= 0 : value > 0);
  }).map((sample) => ({ ts: sample.ts, value: pick(sample) }));
}

// ---- 分组子类 ----
// 返回每个父类当前数据下的子类 ID 列表（含特殊聚合项 __agg__）。

function historyDiskIDs(samples) {
  const ids = [];
  samples.forEach((sample) => (sample.disks || []).forEach((disk) => {
    if (disk.id && !ids.includes(disk.id)) ids.push(disk.id);
  }));
  return ids.sort();
}

// 有过 rpm>0 或 pwm>0 的才算有效风扇（没接风扇的空通道全 0，排除）。
// 整段都在关停档时所有通道都是 0：这时退回全部通道，否则「风扇」组一条线都画不出来。
function historyFanIDs(samples) {
  const fanIDs = [];
  const hasSignal = {};
  samples.forEach((sample) => (sample.fans || []).forEach((fan) => {
    if (!fan.id) return;
    if (!fanIDs.includes(fan.id)) fanIDs.push(fan.id);
    if (fan.rpm > 0 || fan.pwm_percent > 0) hasSignal[fan.id] = true;
  }));
  const signaled = fanIDs.filter((id) => hasSignal[id]);
  return signaled.length ? signaled : fanIDs;
}

function historySensorsOfGroup(samples, group) {
  const ids = [];
  samples.forEach((sample) => (sample.sensors || []).forEach((sensor) => {
    if (sensor.group === group && sensor.key && !ids.includes(sensor.key)) ids.push(sensor.key);
  }));
  return ids.sort();
}

function historyGroupChildIDs(groupKey, samples) {
  switch (groupKey) {
    case 'cpu': return ['__agg__', ...historySensorsOfGroup(samples, 'cpu')];
    case 'sata': return ['__agg__', ...historyDiskIDs(samples).filter((id) => id.startsWith('front-'))];
    case 'nvme': return ['__agg__', ...historyDiskIDs(samples).filter((id) => id.startsWith('m2-'))];
    case 'gpu': return historySensorsOfGroup(samples, 'gpu');
    case 'nic': return historySensorsOfGroup(samples, 'nic');
    case 'other': return historySensorsOfGroup(samples, 'other');
    case 'fan': return historyFanIDs(samples);
    default: return [];
  }
}

// 子类取值：__agg__ → 组聚合值；其余 → 按各自数据源取值（缺失剔除）。
function historyChildValue(groupKey, childID, sample) {
  if (childID === '__agg__') {
    if (groupKey === 'cpu') return sample.cpu_c;
    if (groupKey === 'sata') return sample.hdd_c;
    if (groupKey === 'nvme') return sample.nvme_c;
    return NaN;
  }
  if (groupKey === 'cpu' || groupKey === 'gpu' || groupKey === 'nic' || groupKey === 'other') {
    const sensor = (sample.sensors || []).find((item) => item.group === groupKey && item.key === childID);
    return sensor ? sensor.c : NaN;
  }
  if (groupKey === 'sata' || groupKey === 'nvme') {
    const disk = (sample.disks || []).find((item) => item.id === childID);
    return disk ? disk.c : NaN;
  }
  if (groupKey === 'fan') {
    const fan = (sample.fans || []).find((item) => item.id === childID);
    return fan ? fan.pwm_percent : NaN;
  }
  return NaN;
}

function historyChildLabel(groupKey, childID) {
  if (childID === '__agg__') {
    const group = HISTORY_GROUPS.find((item) => item.key === groupKey);
    return group?.aggregateLabel || '取最高';
  }
  if (groupKey === 'cpu' || groupKey === 'gpu' || groupKey === 'nic' || groupKey === 'other') {
    const custom = sensorDisplayName(childID);
    if (custom) return custom;
    return groupKey === 'cpu' ? childID : String(childID).split(':').pop() + `（${String(childID).split(':')[0] || groupKey}）`;
  }
  if (groupKey === 'sata' || groupKey === 'nvme') return historySlotLabel(childID);
  if (groupKey === 'fan') return historyFanLabel(childID);
  return childID;
}

// 传感器显示名：用户在调试页设置（存后端配置），未设置时退回默认短名。
function sensorDisplayName(sensorKey) {
  const custom = currentStatus?.config?.sensor_names?.[sensorKey];
  return custom && custom.trim() ? custom.trim() : null;
}

// 槽位短名：front-2 → 前置2，m2-1 → M.2 1
function historySlotLabel(slotID) {
  const match = /^(front|m2)-([1-9])$/.exec(slotID || '');
  if (!match) return slotID;
  return match[1] === 'front' ? `前置${match[2]}` : `M.2 ${match[2]}`;
}

function historyChildColor(groupKey, childID, allIDs) {
  const group = HISTORY_GROUPS.find((item) => item.key === groupKey);
  const base = group ? group.color : '#3f6ff5';
  if (childID === '__agg__') return base;
  const index = allIDs.indexOf(childID);
  // 风扇组走独立灰色阶梯（中性灰不参与 HSL 派生）
  if (groupKey === 'fan') return HISTORY_FAN_SHADES[Math.max(0, index) % HISTORY_FAN_SHADES.length];
  return historyChildShade(base, index);
}

// 组曲线：父类开关 + 子类勾选共同决定可见性；聚合项视为一个普通子类。
// 窗口式档位虽然拉取数倍跨度数据，但只画窗口内的点（≤约 240 点），不做
// 渲染抽稀——按全览档位抽稀会把窗口内容抽成稀疏点。
function historyGroupSeries(groupKey, samples, intervalSeconds) {
  const windowed = historyIsWindowed(historyRangeHours);
  const childIDs = historyGroupChildIDs(groupKey, samples);
  const selection = historyEffectiveSelection(groupKey, childIDs);
  const visible = (childID) => selection === null || selection.has(childID);
  return childIDs
    .filter((childID) => visible(childID))
    .map((childID) => {
      const points = historySeriesPoints(
        samples,
        (sample) => historyChildValue(groupKey, childID, sample),
        groupKey === 'fan',
      );
      return {
        id: `${groupKey}:${childID}`,
        color: historyChildColor(groupKey, childID, childIDs),
        segments: historySplitSegments(points, intervalSeconds)
          .map((segment) => historyThinByStride(segment, historyDecimationStride(windowed ? 0.5 : historyRangeHours)))
          .map((segment) => historyThinOut(segment, windowed ? HISTORY_WINDOW_MAX_POINTS : undefined)),
      };
    });
}

// 单图双轴：左轴温度（动态范围），右轴风扇 0-100%。
function renderHistoryChart() {
  const svg = $('history-chart');
  if (!svg) return;
  const status = $('history-status');
  const data = historyCache;
  const samples = data?.samples || [];
  if (!samples.length) {
    svg.replaceChildren();
    renderHistoryLegend();
    renderHistoryScrubBar(false, null);
    if (status) status.textContent = '正在采样，曲线会随时间慢慢生成。';
    return;
  }
  const intervalSeconds = Math.max(1, Number(data.interval_seconds) || 60);
  const nowTs = Math.floor(Date.now() / 1000);
  const ranged = historyFilterRange(samples, historyFetchHoursFor(historyRangeHours), nowTs);
  historyRangedSamples = ranged;
  historyIntervalSeconds = intervalSeconds;
  // 窗口式回看（30 分钟–24 小时档）：横轴域是所选档位跨度的滑动窗口；
  // 末端贴最新时解除钉住，让自动刷新继续跟随实时
  const scrub = historyIsWindowed(historyRangeHours) && ranged.length > 0;
  if (scrub && historyScrubEndTs !== null && historyScrubEndTs >= ranged[ranged.length - 1].ts) {
    historyScrubEndTs = null;
  }
  const win = scrub
    ? historyScrubWindow(ranged, intervalSeconds, historyScrubEndTs ?? ranged[ranged.length - 1].ts, historyRangeHours * 3600)
    : null;
  const domain = win ? { start: win.start, end: win.end } : { start: nowTs - historyRangeHours * 3600, end: nowTs };
  historyXDomain = domain;

  // 逐父类构建曲线；温度类（CPU/SATA/NVMe/网卡/其它）走左轴，风扇走右轴
  const tempLines = [];
  const fanLines = [];
  HISTORY_GROUPS.forEach((group) => {
    if (!historySeriesEnabledFor(group.key)) return;
    const lines = historyGroupSeries(group.key, ranged, intervalSeconds);
    lines.forEach((line) => {
      line.yScale = group.key === 'fan' ? 'fan' : 'temp';
      (group.key === 'fan' ? fanLines : tempLines).push(line);
    });
  });

  // 窗口式回看只按窗口内的点定轴范围与画线：拉取的 8 倍跨度数据若混进
  // Y 轴统计，久远的温度峰值会把当前窗口的曲线整体压扁
  const inWindow = (point) => !win || (point.ts >= win.start && point.ts <= win.end);
  const tempValues = tempLines.flatMap((line) => line.segments.flat().filter(inWindow).map((point) => point.value));
  const bounds = historyYBounds(tempValues, 20, 90);
  historyYBoundsState = bounds;

  const chartScale = curveChartScale(svg);
  const textSize = CURVE_TEXT_PX / chartScale;
  const tempTicks = historyNiceTicks(bounds.lo, bounds.hi, 4);
  // 左右留白按最宽轴刻度的实际宽度放大（右轴最宽固定是 100%）；
  // 桌面 scale≥1 时不小于 CHART 原值，布局保持不变。
  const axisPad = 8 / chartScale;
  const leftLabelWidth = Math.max(0, ...tempTicks.map((tick) => historyAxisTextWidth(`${tick}°`, textSize)));
  const plot = {
    left: Math.max(CHART.left, axisPad + leftLabelWidth),
    right: Math.min(CHART.right, CURVE_VIEWBOX.width - axisPad - historyAxisTextWidth('100%', textSize)),
    top: CHART.top,
    bottom: CHART.bottom,
  };
  historyPlotBox = plot;
  svg.classList.toggle('scrub', Boolean(win)); // 仅回看模式显示拖动光标与手势
  const x = (ts) => plot.left + ((clamp(ts, domain.start, domain.end) - domain.start) / Math.max(1, domain.end - domain.start)) * (plot.right - plot.left);
  const yTemp = (value) => plot.bottom - ((clamp(value, bounds.lo, bounds.hi) - bounds.lo) / (bounds.hi - bounds.lo)) * (plot.bottom - plot.top);
  const yFan = (percent) => plot.bottom - (clamp(percent, 0, HISTORY_FAN_MAX_PERCENT) / HISTORY_FAN_MAX_PERCENT) * (plot.bottom - plot.top);
  svg.replaceChildren();

  tempTicks.forEach((tick) => {
    svg.append(svgElement('line', { x1: plot.left, y1: yTemp(tick), x2: plot.right, y2: yTemp(tick), class: 'chart-grid-line' }));
    svg.append(svgElement('text', { x: plot.left - axisPad, y: yTemp(tick) + textSize * 0.35, class: 'chart-axis-text', 'font-size': textSize, 'text-anchor': 'end' }, `${tick}°`));
  });
  [0, 25, 50, 75, 100].forEach((tick) => {
    svg.append(svgElement('text', { x: plot.right + axisPad, y: yFan(tick) + textSize * 0.35, class: 'chart-axis-text', 'font-size': textSize, 'text-anchor': 'start' }, `${tick}%`));
  });
  const timeTicks = historyTimeTicks(domain.start, domain.end, historyRangeHours);
  const tickStep = timeTicks.length > 1 ? timeTicks[1] - timeTicks[0] : 0;
  // 网格线按档位步长全画；刻度文字过密时抽稀，可容纳的标签数随缩放比例减少
  const maxLabels = Math.min(10, Math.max(5, Math.round(10 * chartScale)));
  const labelEvery = Math.max(1, Math.ceil(timeTicks.length / maxLabels));
  // 窄屏下横轴标签行高随字号放大，低于绘图区底线时下移，避免顶到网格线；
  // 标签行与两侧轴刻度有垂直重叠时，端部标签压到刻度列就向图内锚定
  const xLabelBase = Math.max(CURVE_VIEWBOX.height - 14 / chartScale, plot.bottom + textSize * 0.75 + 1 / chartScale);
  const xRowNearGutter = xLabelBase < plot.bottom + textSize * 1.07;
  timeTicks.forEach((tick, index) => {
    svg.append(svgElement('line', { x1: x(tick), y1: plot.top, x2: x(tick), y2: plot.bottom, class: 'chart-grid-line' }));
    if (index % labelEvery !== 0) return;
    const label = historyTickLabel(tick, tickStep);
    const lx = x(tick);
    const halfLabel = historyAxisTextWidth(label, textSize) / 2;
    let anchor = 'middle';
    if (xRowNearGutter && lx - halfLabel < plot.left - axisPad) anchor = 'start';
    else if (xRowNearGutter && lx + halfLabel > plot.right + axisPad) anchor = 'end';
    svg.append(svgElement('text', { x: lx, y: xLabelBase, class: 'chart-axis-text', 'font-size': textSize, 'text-anchor': anchor }, label));
  });

  const drawSeries = (line, yScale) => {
    line.segments.forEach((segment) => {
      // 回看模式只画窗口内的点：窗口外的点若靠 clamp 会堆在绘图区两缘
      const visible = segment.filter(inWindow);
      if (visible.length < 2) return;
      svg.append(svgElement('polyline', {
        points: visible.map((point) => `${x(point.ts)},${yScale(point.value)}`).join(' '),
        fill: 'none', stroke: line.color, 'stroke-width': 2 / chartScale, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
      }));
    });
  };
  tempLines.forEach((line) => drawSeries(line, yTemp));
  fanLines.forEach((line) => drawSeries(line, yFan));

  renderHistoryLegend();
  renderHistoryScrubBar(scrub, win);

  if (status) status.textContent = '';
  // 自动刷新重绘会清掉十字线；若鼠标仍悬停在图上，按原位置恢复
  if (historyLastCursorEvent && !$('history-tooltip')?.hidden) showHistoryCursor(historyLastCursorEvent);
}

// 图例：六个父类行 = 复选框（整组显隐）+ 色点 + ▾（展开子类勾选弹窗）。
// 父类勾选开关：取消时连带清空组内全部子类勾选（存为空 Set）——可见性是
// "父类开 && 子类勾"两层与运算，父类关时子类残留会违背直觉；重新勾上父类
// 后子类仍是空的，要恢复数据再手动勾或点聚合项。偏好同时落服务端 ui_prefs
// 与 localStorage（镜像兜底），跨浏览器/设备一致。
function setHistoryGroupEnabled(groupKey, enabled) {
  historySeriesEnabled[groupKey] = enabled;
  // 直接写组内选择（不走 setHistoryChildSelection）：清空与组开关是同一次
  // 用户操作，合并成一次 ui_prefs 保存
  if (!enabled) historyChildSelection[groupKey] = new Set();
  saveHistorySeriesEnabled();
  saveHistoryChildSelection();
  saveUIPrefsQuietly();
  renderHistoryChart();
}

function renderHistoryLegend() {
  const legend = $('history-legend');
  if (!legend) return;
  const identity = JSON.stringify(HISTORY_GROUPS.map((group) => ({
    key: group.key,
    children: historyGroupChildIDs(group.key, historyRangedSamples),
    empty: (historyCache?.samples || []).length === 0,
  })));
  if (identity !== historyLegendIdentity || !legend.children.length) {
    historyLegendIdentity = identity;
    legend.replaceChildren();
    HISTORY_GROUPS.forEach((group) => {
      const item = document.createElement('span');
      item.className = 'history-legend-item';
      item.dataset.seriesKey = group.key;
      const box = document.createElement('input');
      box.type = 'checkbox';
      box.checked = historySeriesEnabledFor(group.key);
      box.addEventListener('change', () => {
        setHistoryGroupEnabled(group.key, box.checked);
      });
      const dot = document.createElement('i');
      dot.className = 'history-dot';
      dot.style.background = group.color;
      item.append(box, dot, document.createTextNode(group.label));
      const childIDs = historyGroupChildIDs(group.key, historyRangedSamples);
      // 所有父类都可展开：无数据的组弹窗里显示“暂无数据”
      const arrow = document.createElement('button');
      arrow.type = 'button';
      arrow.className = 'history-popover-arrow';
      arrow.textContent = '▾';
      arrow.setAttribute('aria-label', `选择${group.label}曲线`);
      arrow.addEventListener('click', (event) => {
        event.stopPropagation();
        toggleHistoryPopover(group.key, item);
      });
      item.append(arrow);
      // 父类名字整行也可点击展开；父类勾选框和弹窗内部的点击（子类勾选等）
      // 不触发展开/折叠——弹窗在行内，冒泡到这里的点击若不排除会把弹窗
      // 立即折叠，且子类复选框随 DOM 移除导致 change 丢失、勾选不生效
      item.addEventListener('click', (event) => {
        if (event.target === box) return;
        if (event.target.closest?.('.history-popover')) return;
        toggleHistoryPopover(group.key, item);
      });
      legend.append(item);
    });
    if (historyOpenPopoverKey) {
      const anchor = legend.querySelector(`.history-legend-item[data-series-key="${historyOpenPopoverKey}"]`);
      if (anchor) {
        const popover = document.createElement('div');
        popover.className = 'history-popover';
        popover.dataset.seriesKey = historyOpenPopoverKey;
        anchor.append(popover);
        fillHistoryPopover(historyOpenPopoverKey, popover);
      } else {
        historyOpenPopoverKey = null;
      }
    }
  }
}

// 父类弹窗：温度组首行为“取最高”，其余行为组内子类；勾选即改曲线显隐。
function toggleHistoryPopover(groupKey, anchorItem) {
  const existing = anchorItem.querySelector('.history-popover');
  closeHistoryPopovers();
  if (existing) return;
  historyOpenPopoverKey = groupKey;
  const popover = document.createElement('div');
  popover.className = 'history-popover';
  popover.dataset.seriesKey = groupKey;
  anchorItem.append(popover);
  fillHistoryPopover(groupKey, popover);
}

function closeHistoryPopovers() {
  historyOpenPopoverKey = null;
  document.querySelectorAll('.history-popover').forEach((node) => node.remove());
}

function fillHistoryPopover(groupKey, popover) {
  const childIDs = historyGroupChildIDs(groupKey, historyRangedSamples);
  const selection = historyEffectiveSelection(groupKey, childIDs);
  const visible = (childID) => selection === null || selection.has(childID);
  popover.replaceChildren();
  if (!childIDs.length) {
    const empty = document.createElement('div');
    empty.className = 'history-popover-empty';
    empty.textContent = '该组暂无数据';
    popover.append(empty);
    return;
  }
  childIDs.forEach((childID) => {
    const row = document.createElement('label');
    row.className = 'history-popover-row';
    const box = document.createElement('input');
    box.type = 'checkbox';
    box.checked = visible(childID);
    box.addEventListener('change', () => {
      // 起点必须是「当前屏幕上看到的勾选」（可能来自默认值兜底出的全部），
      // 否则先取消一条时会把默认那两条又当成全集，勾出反直觉的结果。
      const effective = historyEffectiveSelection(groupKey, childIDs);
      const current = effective === null ? new Set(childIDs) : new Set(effective);
      if (box.checked) current.add(childID);
      else current.delete(childID);
      setHistoryChildSelection(groupKey, current);
      renderHistoryChart();
    });
    const dot = document.createElement('i');
    dot.className = 'history-dot';
    dot.style.background = historyChildColor(groupKey, childID, childIDs); // 与曲线同色，所见即所得
    row.append(box, dot, document.createTextNode(historyChildLabel(groupKey, childID)));
    popover.append(row);
  });
}

// 点击图外区域收起弹窗。
document.addEventListener('click', (event) => {
  if (!event.target.closest?.('.history-legend-item')) closeHistoryPopovers();
});

async function fetchHistory(force = false) {
  if (!force && historyCache && Date.now() - historyFetchedAt < HISTORY_FETCH_TTL) {
    renderHistoryChart();
    return historyCache;
  }
  try {
    const query = historyRequestUrl();
    const data = await request(query);
    // 快速切换范围时丢弃过期响应：慢的旧请求不能覆盖当前范围的缓存
    if (query !== historyRequestUrl()) return historyCache;
    if (data && Array.isArray(data.samples)) {
      historyCache = data;
      historyFetchedAt = Date.now();
      renderHistoryChart();
    }
  } catch (error) {
    const status = $('history-status');
    if (status) status.textContent = `历史数据读取失败：${error.message}`;
  }
  return historyCache;
}

// 把当前 historyRangeHours 同步到滑杆与按钮：固定挡位停在吸附位中心，
// 动一下滑杆（input/change）就回到滑杆值。
// 值标签固定在滑杆正中（CSS 里写死 left:50%），不跟拇指跑。
function syncHistoryRangeUI() {
  const slider = $('history-range-slider');
  if (slider) {
    slider.value = String(historyHoursToPos(historyRangeHours));
    slider.setAttribute('aria-valuetext', historyRangeLabel(historyRangeHours));
  }
  const label = $('history-range-value');
  if (label) label.textContent = historyRangeLabel(historyRangeHours);
}

function setHistoryRange(hours) {
  historyRangeTouched = true; // 本会话用户已手动选择，此后不被后端档位覆盖
  historyRangeHours = hours;
  historyScrubEndTs = null; // 换档退出回看，窗口回到实时
  saveHistoryRangeHours(hours);
  syncHistoryRangeUI();
  fetchHistory(true);
}

// 后端配置的档位只在首次状态到达时采纳一次：localStorage 只是首屏快速起效
// 的缓存，跨设备的最终一致性以后端为准（换浏览器/换设备也生效）；用户本会话
// 已操作过档位则不打扰。
let historyRangeTouched = false;
let historyRangeAdopted = false;

// 历史曲线显隐偏好（父类开关+组内勾选）从服务端 ui_prefs 采纳：与档位同款，
// 首次 render 后不再覆盖（用户进页后立刻点勾选不被轮询回来的旧值冲掉）。
// 采纳后写 localStorage 镜像并重置图例标识，下次图例渲染用新勾选状态。
let historySeriesAdopted = false;
function applyBackendHistorySeries(prefs) {
  if (historySeriesAdopted) return;
  historySeriesAdopted = true;
  if (historyDefaultsMigrated) {
    // 本地刚升到新默认：把空显隐推回后端（服务端旧勾选不能覆盖回来）
    historyLegendIdentity = '';
    saveUIPrefsQuietly();
    return;
  }
  const series = prefs?.history_series;
  const children = prefs?.history_children;
  if (!series && !children) return;
  if (series && typeof series === 'object') historySeriesEnabled = { ...series };
  if (children && typeof children === 'object') {
    const merged = { ...historyChildSelection };
    for (const [groupKey, list] of Object.entries(children)) {
      merged[groupKey] = list === null ? null : new Set(list);
    }
    historyChildSelection = merged;
  }
  saveHistorySeriesEnabled();
  saveHistoryChildSelection();
  historyLegendIdentity = '';
}

function applyBackendHistoryRange(prefs) {
  if (historyRangeAdopted || historyRangeTouched) return;
  historyRangeAdopted = true;
  const hours = Number(prefs?.history_range_hours);
  if (!Number.isFinite(hours) || hours <= 0) return;
  const normalized = normalizeHistoryRangeHours(hours);
  if (normalized === historyRangeHours) return;
  historyRangeHours = normalized;
  historyScrubEndTs = null;
  try { window.localStorage.setItem(HISTORY_RANGE_STORAGE_KEY, String(normalized)); }
  catch (error) { /* 缓存镜像失败可忽略 */ }
  syncHistoryRangeUI();
  fetchHistory(true);
}

// 保留天数收窄可查窗口：7 天/30 天挡位按保存天数显隐（小于 7 天两个都不
// 显示，7–29 天只显示 7 天），滑杆 max 随挡位数变化；当前记忆的范围超出
// 保留期则回落到默认档（30 分钟）。
function updateHistoryRangeAvailability(retentionDays) {
  const days = Math.round(Number(retentionDays) || 30);
  historySliderStopCount = 3 + (days >= 7 ? 1 : 0) + (days >= 30 ? 1 : 0);
  const slider = $('history-range-slider');
  // 滑杆 max = 末挡停靠位：拖到最右恰好填满整条轨道（进度条 100%），
  // 吸附回拉不会让尾巴留一段灰色。
  if (slider) slider.max = String(historyStopCenterPos(historySliderStopCount - 1));
  const ticks = document.querySelector('.history-range-ticks');
  if (ticks) {
    ticks.classList.remove('layout-5', 'layout-6', 'layout-7');
    ticks.classList.add(`layout-${historySliderStopCount + 2}`); // 30分/2时 两个恒显刻度 + 挡位数
    const weekTick = ticks.querySelector('[data-tick="7d"]');
    const monthTick = ticks.querySelector('[data-tick="30d"]');
    if (weekTick) weekTick.hidden = days < 7;
    if (monthTick) monthTick.hidden = days < 30;
  }
  if (historyRangeHours > days * 24) setHistoryRange(HISTORY_DEFAULT_HOURS);
}

function setupHistoryPanel() {
  const slider = $('history-range-slider');
  if (slider) {
    // 拖动中只实时刷新标签，松手（change）才取数，避免半路连续请求；
    // 滑杆位置经双段映射换成小时（无极区 1 分钟粒度 / 挡位区整段吸附）。
    slider.addEventListener('input', () => {
      const hours = historyPosToHours(parseFloat(slider.value));
      const label = $('history-range-value');
      if (label) label.textContent = historyRangeLabel(hours);
    });
    slider.addEventListener('change', () => setHistoryRange(historyPosToHours(parseFloat(slider.value))));
  }
  syncHistoryRangeUI(); // 应用 localStorage 记忆的范围
  setupHistoryCursor();
  setupHistoryScrub();
}

// ---- 悬浮十字线：竖虚线 + 各曲线在该时刻的数值提示框 ----

function historyCursorTimeAt(event) {
  const svg = $('history-chart');
  if (!svg) return null;
  const matrix = svg.getScreenCTM();
  if (!matrix) return null;
  const point = svg.createSVGPoint();
  point.x = event.clientX;
  point.y = event.clientY;
  const local = point.matrixTransform(matrix.inverse());
  const raw = historyXDomain.start
    + ((local.x - historyPlotBox.left) / (historyPlotBox.right - historyPlotBox.left)) * (historyXDomain.end - historyXDomain.start);
  return Math.round(Math.min(historyXDomain.end, Math.max(historyXDomain.start, raw)));
}

function historyNearestSample(ts) {
  let best = null;
  let bestDelta = Infinity;
  for (const sample of historyRangedSamples) {
    const delta = Math.abs(sample.ts - ts);
    if (delta < bestDelta) { best = sample; bestDelta = delta; }
  }
  return best;
}

// 十字线时刻各系列的取值行：与图上可见曲线同色同序。
function historyCursorRows(sample) {
  const rows = [];
  HISTORY_GROUPS.forEach((group) => {
    if (!historySeriesEnabledFor(group.key)) return;
    const childIDs = historyGroupChildIDs(group.key, historyRangedSamples);
    const selection = historyEffectiveSelection(group.key, childIDs);
    childIDs.forEach((childID) => {
      if (!(selection === null || selection.has(childID))) return;
      const value = historyChildValue(group.key, childID, sample);
      if (!(Number.isFinite(value) && value > 0)) return;
      const color = historyChildColor(group.key, childID, childIDs);
      const label = historyChildLabel(group.key, childID);
      if (group.key === 'fan') {
        const rpm = (sample.fans || []).find((fan) => fan.id === childID)?.rpm ?? 0;
        rows.push({ color, label, text: `${rpm} RPM · ${Math.round(value)}%`, isFan: true, value });
      } else {
        rows.push({ color, label, text: `${value.toFixed(1)}°C`, isFan: false, value });
      }
    });
  });
  return rows;
}

function drawHistoryCrosshair(sample) {
  const svg = $('history-chart');
  if (!svg) return;
  svg.querySelectorAll('.history-crosshair').forEach((node) => node.remove());
  if (!sample) return;
  const plot = historyPlotBox;
  const span = Math.max(1, historyXDomain.end - historyXDomain.start);
  const cx = plot.left + ((clamp(sample.ts, historyXDomain.start, historyXDomain.end) - historyXDomain.start) / span) * (plot.right - plot.left);
  // 竖虚线用 non-scaling-stroke（SVG 属性，CSP 安全）：viewBox 拉伸时线宽保持
  // styles.css 里设定的 1.5 屏幕像素，不会随窗口放大变粗
  svg.append(svgElement('line', { x1: cx, y1: plot.top, x2: cx, y2: plot.bottom, class: 'history-crosshair history-crosshair-line', 'vector-effect': 'non-scaling-stroke' }));
  const { lo, hi } = historyYBoundsState;
  const yTemp = (value) => plot.bottom - ((clamp(value, lo, hi) - lo) / (hi - lo)) * (plot.bottom - plot.top);
  const yFan = (percent) => plot.bottom - (clamp(percent, 0, HISTORY_FAN_MAX_PERCENT) / HISTORY_FAN_MAX_PERCENT) * (plot.bottom - plot.top);
  // 圆点半径除以缩放（同折线 stroke-width 的处理）：viewBox 随容器宽度拉伸，
  // 固定 viewBox 单位会让悬停圆点等比变大；curveChartScale 自带 0.25 下限防 0
  const scale = curveChartScale(svg);
  historyCursorRows(sample).forEach((row) => {
    svg.append(svgElement('circle', { cx, cy: row.isFan ? yFan(row.value) : yTemp(row.value), r: 4 / scale, class: 'history-crosshair', fill: row.color }));
  });
}

function showHistoryCursor(event) {
  if (historyScrubDragging || !historyRangedSamples.length) return;
  historyLastCursorEvent = { clientX: event.clientX, clientY: event.clientY };
  const ts = historyCursorTimeAt(event);
  if (ts == null) return;
  const sample = historyNearestSample(ts);
  if (!sample) return;
  drawHistoryCrosshair(sample);
  const tooltip = $('history-tooltip');
  if (!tooltip) return;
  tooltip.replaceChildren();
  const time = document.createElement('b');
  time.textContent = new Date(sample.ts * 1000).toLocaleString('zh-CN', { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  tooltip.append(time);
  historyCursorRows(sample).forEach((row) => {
    const item = document.createElement('div');
    item.className = 'history-tip-row';
    const dot = document.createElement('i');
    dot.className = 'history-dot';
    dot.style.background = row.color;
    item.append(dot, document.createTextNode(`${row.label} ${row.text}`));
    tooltip.append(item);
  });
  tooltip.hidden = false;
  tooltip.classList.add('visible');
  const gap = 14;
  const margin = 8;
  const rect = tooltip.getBoundingClientRect();
  let left = event.clientX + gap;
  if (left + rect.width > window.innerWidth - margin) left = event.clientX - rect.width - gap;
  const top = Math.min(Math.max(margin, event.clientY - rect.height / 2), Math.max(margin, window.innerHeight - rect.height - margin));
  tooltip.style.left = `${Math.max(margin, left)}px`;
  tooltip.style.top = `${top}px`;
}

function hideHistoryCursor() {
  historyLastCursorEvent = null;
  const svg = $('history-chart');
  if (svg) svg.querySelectorAll('.history-crosshair').forEach((node) => node.remove());
  const tooltip = $('history-tooltip');
  if (tooltip) {
    tooltip.classList.remove('visible');
    tooltip.hidden = true;
  }
}

function setupHistoryCursor() {
  const svg = $('history-chart');
  if (!svg) return;
  svg.addEventListener('mousemove', showHistoryCursor);
  svg.addEventListener('mouseleave', hideHistoryCursor);
}

// ---- 窗口式回看：状态条 + 图表拖动 ----
// 图表固定显示所选档位跨度的窗口；在图上左右拖动（鼠标/触摸）移动窗口，
// 拖回最右跟随实时。回看状态条显示窗口时段，离开实时边缘时提供“回到最新”。

function renderHistoryScrubBar(active, win) {
  const bar = $('history-scrub-bar');
  if (!bar) return;
  if (!active || !win) {
    bar.hidden = true;
    return;
  }
  bar.hidden = false;
  const label = $('history-scrub-range');
  if (label) {
    // 窗口跨天（如 24 小时档）时两端时钟可能相同，补日期消歧
    const sameDay = new Date(win.start * 1000).toDateString() === new Date(win.end * 1000).toDateString();
    const fmt = (ts) => {
      const date = new Date(ts * 1000);
      return sameDay ? historyFormatClock(ts) : `${date.getMonth() + 1}/${date.getDate()} ${historyFormatClock(ts)}`;
    };
    const span = `${fmt(win.start)}–${fmt(win.end)}`;
    label.textContent = win.atLive
      ? `实时 ${span} · 在图上左右拖动回看历史`
      : `回看 ${span}`;
  }
  const live = $('history-scrub-live');
  if (live) live.hidden = win.atLive;
}

function setupHistoryScrub() {
  const svg = $('history-chart');
  if (!svg) return;
  let drag = null; // { pointerId, startX, baseEndTs, chartScale }
  // 内容跟手换算：event.clientX 是屏幕像素，historyPlotBox 是 SVG viewBox
  // 单位，按 curveChartScale（屏幕 px / viewBox 单位）折算后均摊窗口时间
  // 跨度——否则桌面拖动偏快、窄屏偏慢一个缩放系数。按下时测量并缓存。
  const pxPerSecond = (chartScale) => ((historyPlotBox.right - historyPlotBox.left) * chartScale) / Math.max(60, historyRangeHours * 3600);
  svg.addEventListener('pointerdown', (event) => {
    if (!historyIsWindowed(historyRangeHours) || event.button > 0) return;
    if (!historyRangedSamples.length) return;
    drag = {
      pointerId: event.pointerId,
      startX: event.clientX,
      baseEndTs: historyScrubEndTs ?? historyRangedSamples[historyRangedSamples.length - 1].ts,
      chartScale: curveChartScale(svg) || 1,
    };
    historyScrubDragging = true;
    hideHistoryCursor();
    try { svg.setPointerCapture(event.pointerId); } catch (error) { /* 已释放的指针 */ }
    svg.classList.add('scrubbing');
  });
  svg.addEventListener('pointermove', (event) => {
    if (!drag || event.pointerId !== drag.pointerId) return;
    const oldest = historyRangedSamples[0].ts;
    // 内容跟手：向右拖 = 看更早的数据，窗口末端随位移左移并吸附采样网格
    const rawEnd = drag.baseEndTs - (event.clientX - drag.startX) / pxPerSecond(drag.chartScale);
    historyScrubEndTs = oldest + Math.round((rawEnd - oldest) / historyIntervalSeconds) * historyIntervalSeconds;
    renderHistoryChart();
  });
  const endDrag = (event) => {
    if (!drag || event.pointerId !== drag.pointerId) return;
    drag = null;
    historyScrubDragging = false;
    svg.classList.remove('scrubbing');
  };
  svg.addEventListener('pointerup', endDrag);
  svg.addEventListener('pointercancel', endDrag);
  const live = $('history-scrub-live');
  if (live) {
    live.addEventListener('click', () => {
      historyScrubEndTs = null;
      fetchHistory();
      renderHistoryChart();
    });
  }
}


function curvePositionFromPointer(kind, event) {
  const svg = $(curveEditors[kind].chartID);
  const matrix = svg.getScreenCTM();
  if (!matrix) return null;
  const point = svg.createSVGPoint();
  point.x = event.clientX;
  point.y = event.clientY;
  const local = point.matrixTransform(matrix.inverse());
  return {
    temperature: 20 + ((local.x - fanPlotBox.left) / (fanPlotBox.right - fanPlotBox.left)) * 80,
    pwm: 30 + ((fanPlotBox.bottom - local.y) / (fanPlotBox.bottom - fanPlotBox.top)) * 70,
  };
}

function updateCurveFromPointer(kind, event) {
  const editor = curveEditors[kind];
  if (editor.draggedIndex < 0) return;
  const position = curvePositionFromPointer(kind, event);
  if (!position) return;
  setCurvePoint(kind, editor.draggedIndex, position.temperature, position.pwm);
  renderFanChart(kind);
}

function populateFanDevices(status, keepInputs) {
  const select = $('fan-device');
  const fans = connectedFans(status.fan_control);
  const config = status.config?.fan || {};
  const signature = fanListSignature(fans);
  const rebuild = signature !== fanSelectorSignature;
  if (rebuild) {
    fanSelectorSignature = signature;
    Object.values(FAN_CURVE_GROUPS).forEach((group) => {
      const container = $(group.containerID);
      container.replaceChildren();
      fans.forEach((fan) => {
        const label = document.createElement('label');
        label.className = 'curve-fan-option';
        label.dataset.fanId = fan.id;
        const input = document.createElement('input');
        input.type = 'checkbox';
        input.value = fan.id;
        const name = document.createElement('b');
        name.textContent = fanLabel(fan, fans);
        const rpm = document.createElement('small');
        label.append(input, name, rpm);
        container.append(label);
      });
      if (!fans.length) container.textContent = '未检测到有转速反馈的风扇';
    });
    select.replaceChildren();
    fans.forEach((fan) => {
      const option = document.createElement('option');
      option.value = fan.id;
      option.textContent = fanLabel(fan, fans);
      select.append(option);
    });
  }
  Object.values(FAN_CURVE_GROUPS).forEach((group) => {
    fans.forEach((fan) => {
      const label = $(group.containerID).querySelector(`[data-fan-id="${fan.id}"]`);
      if (label) label.querySelector('small').textContent = `${fan.rpm} RPM`;
    });
  });
  if (!keepInputs || rebuild) {
    Object.values(FAN_CURVE_GROUPS).forEach((group) => {
      // 默认绑定所有风扇：配置里没有（新装、或从没动过）时三条曲线都全选，
      // 用户手动取消过某几个就照配置来。
      const configured = Array.isArray(config[group.configField]) && config[group.configField].length
        ? config[group.configField]
        : fans.map((fan) => fan.id);
      const selected = new Set(configured);
      $(group.containerID).querySelectorAll('input').forEach((input) => { input.checked = selected.has(input.value); });
    });
  }
  select.value = config.device_id || selectedCurveFanIDs('cpu')[0] || fans[0]?.id || '';
}

function selectedCurveFanIDs(kind) {
  const group = FAN_CURVE_GROUPS[kind];
  return [...$(group.containerID).querySelectorAll('input[type="checkbox"]:checked')].map((input) => input.value);
}

function fillFanInputs(fan = {}) {
  $('fan-enabled').checked = Boolean(fan.enabled);
  $('fan-min').value = fan.min_pwm_percent ?? DEFAULT_MIN_PWM_PERCENT;
  $('fan-emergency').value = fan.emergency_temp_c ?? 85;
  $('fan-poll').value = fan.poll_seconds ?? 2;
  const fillCurve = (kind, curve, fallback) => {
    const source = Array.isArray(curve) && curve.length >= CURVE_MIN_POINTS && curve.length <= CURVE_MAX_POINTS
      && curve.every((point) => Number.isFinite(Number(point.temp_c)) && Number.isFinite(Number(point.pwm_percent)))
      ? curve : fallback;
    const editor = curveEditors[kind];
    editor.curve = source.map((point) => ({ temp_c: Number(point.temp_c), pwm_percent: Number(point.pwm_percent) }));
    editor.selectedIndex = clamp(editor.selectedIndex, 0, editor.curve.length - 1);
  };
  fillCurve('cpu', fan.curve, DEFAULT_CPU_CURVE);
  fillCurve('hdd', fan.hdd_curve || fan.disk_curve, DEFAULT_STORAGE_CURVE);
  fillCurve('nvme', fan.nvme_curve, DEFAULT_STORAGE_CURVE);
  fillFanSlotSelections(fan);
  // 后端留的关停前曲线快照：本地临时曲线缺失时，再次点击「关停」用它恢复
  const offBackup = fan.off_mode_backup;
  offModeCurveBackup = offBackup
    ? validCurveSet({ hdd: offBackup.hdd_curve, nvme: offBackup.nvme_curve, cpu: offBackup.curve })
    : null;
  // 打开页面时按配置反推当前选中的曲线模式（关停／预设／全速／临时）
  setCurveMode(detectCurveMode(fan));
}

function fillHistoryInputs(history = {}) {
  const enabled = history.enabled !== false; // 后端旧配置无 history 段，默认启用
  $('history-enabled').checked = enabled;
  const maxSize = Number(history.max_size_mb) || 64;
  $('history-max-size').value = maxSize;
  // 旧后端配置无 retention_days 字段时按 30 天兜底
  const retentionDays = Number(history.retention_days) || 30;
  $('history-retention-days').value = retentionDays;
  $('history-archive-enabled').checked = Boolean(history.archive_enabled);
  $('history-archive-dir').value = history.archive_dir || '';
  updateHistoryRangeAvailability(retentionDays);
  updateHistoryDisabledNotice(enabled);
  // 运行日志大小在 render() 的 status 上下文里单独回填（见 syncRunLogInputs）
}

function syncRunLogInputs(status = {}) {
  $('runlog-max-size').value = Number(status.config?.log?.max_size_mb) || 16;
}

function updateHistoryDisabledNotice(enabled) {
  $('history-disabled-notice').hidden = enabled;
}

$('history-enabled').addEventListener('change', () => {
  updateHistoryDisabledNotice($('history-enabled').checked);
});

// 历史设置保存前的取值校验：返回错误提示文案，通过时返回 null
function historySettingsError(maxSize, retentionDays) {
  if (!Number.isFinite(maxSize) || maxSize < 8 || maxSize > 1024) return '数据库大小上限需在 8–1024 MB 之间。';
  if (!Number.isFinite(retentionDays) || retentionDays < 1) return '保存天数需至少为 1 天。';
  return null;
}

$('save-history').addEventListener('click', async () => {
  const maxSizeInput = $('history-max-size');
  const retentionInput = $('history-retention-days');
  if (!maxSizeInput.reportValidity() || !retentionInput.reportValidity()) return;
  const maxSize = Number(maxSizeInput.value);
  const retentionDays = Number(retentionInput.value);
  const archiveEnabled = $('history-archive-enabled').checked;
  const archiveDir = $('history-archive-dir').value.trim();
  if (archiveEnabled && !archiveDir) {
    showMessage('开启长期记录需先填写保存位置。', true, 'message-history');
    return;
  }
  const error = historySettingsError(maxSize, retentionDays);
  if (error) {
    showMessage(error, true, 'message-history');
    return;
  }
  setBusy(true);
  try {
    render(await request('api/config/history', { method: 'POST', body: JSON.stringify({ enabled: $('history-enabled').checked, max_size_mb: maxSize, retention_days: retentionDays, archive_enabled: archiveEnabled, archive_dir: archiveEnabled ? archiveDir : '' }) }), true);
    showMessage($('history-enabled').checked ? '历史设置已保存；后台每分钟继续写入采样。' : '历史设置已保存；后台已停止写入新采样。', false, 'message-history');
  } catch (error) {
    showMessage(`保存失败：${error.message}`, true, 'message-history');
  } finally {
    setBusy(false);
  }
});

// 清空数据库属危险操作：按钮在调试页"历史温度设定"卡片的操作行（导出之后）。
// 点击瞬间先补一次长期记录冲刷（不论二次确认结果如何，缓冲里的旧数据先落到
// 归档盘），然后经 window.confirm 二次确认；确认后才真正清空。冲刷失败时在
// 弹窗里明示后果，由用户决定是否仍要清空。
$('history-clear').addEventListener('click', async () => {
  setBusy(true);
  let flushError = '';
  let flushed = false;
  try {
    const result = await request('api/history/archive', { method: 'POST', body: '{}' });
    flushed = Boolean(result.flushed);
  } catch (error) {
    flushError = error.message;
  }
  const message = flushError
    ? `长期记录冲刷失败（${flushError}），未冲刷的数据将随清空丢失。仍要清空？`
    : flushed
      ? '长期记录的缓冲已补归档。清空数据库将删除全部在线历史采样，且无法恢复。确定清空？'
      : '清空数据库将删除全部历史采样，且无法恢复。确定清空？';
  const confirmed = await showAppConfirm({
    title: '清空历史数据库',
    message,
    confirmText: '清空数据库',
    danger: true,
  });
  if (!confirmed) {
    setBusy(false);
    return; // 取消：补冲刷已生效（或已尝试），在线数据原样保留
  }
  try {
    await request('api/history/clear', { method: 'POST', body: '{}' });
    // 强制绕过 TTL 缓存重新拉取，曲线立即清空（与保存传感器名后的刷新一致）
    await fetchHistory(true);
    renderHistoryChart();
    showMessage('历史数据库已清空。', false, 'message-history');
  } catch (error) {
    showMessage(`清空失败：${error.message}`, true, 'message-history');
  } finally {
    setBusy(false);
  }
});

// ---- 运行日志：独立大小设置、导出与清空 ----

function runlogMessage(message, error = false) {
  showMessage(message, error, 'runlog-status');
}

$('save-runlog').addEventListener('click', async () => {
  const input = $('runlog-max-size');
  if (!input.reportValidity()) return;
  const maxSize = Number(input.value);
  if (!Number.isFinite(maxSize) || maxSize < 1 || maxSize > 256) {
    runlogMessage('日志大小上限需在 1–256 MB 之间。', true);
    return;
  }
  setBusy(true);
  try {
    render(await request('api/config/log', { method: 'POST', body: JSON.stringify({ max_size_mb: maxSize }) }), true);
    runlogMessage(`日志大小上限已保存为 ${maxSize} MB；超过后自动截断并保留一代备份。`);
  } catch (error) {
    runlogMessage(`保存失败：${error.message}`, true);
  } finally {
    setBusy(false);
  }
});

$('runlog-export').addEventListener('click', () => {
  const stamp = new Date().toISOString().slice(0, 10);
  const link = document.createElement('a');
  link.href = baseUrl('api/log/export');
  link.download = `tad-module-log-${stamp}.txt`;
  document.body.append(link);
  link.click();
  link.remove();
  runlogMessage('日志下载已开始。');
});

$('runlog-clear').addEventListener('click', async () => {
  const ok = await showAppConfirm({
    title: '清空运行日志',
    message: '清空运行日志将删除当前日志与上一代备份，且无法恢复。确定清空？',
    confirmText: '清空日志',
    danger: true,
  });
  if (!ok) return;
  setBusy(true);
  try {
    await request('api/log/clear', { method: 'POST', body: '{}' });
    runlogMessage('运行日志已清空。');
  } catch (error) {
    runlogMessage(`清空失败：${error.message}`, true);
  } finally {
    setBusy(false);
  }
});

// ---- 风扇调试控制（调试页勾选显示;调试期间暂停温控曲线） ----

const FAN_DEBUG_VISIBLE_KEY = 'tad-fan-debug-visible'; // 旧版 localStorage 键,仅一次性迁移用
let fanDebugPollTimer = 0;
// 调试卡显隐偏好(服务端 ui_prefs 持久,跨浏览器一致);status 未到时按 false
let uiFanDebugVisible = false;

function fanDebugVisiblePref() {
  return uiFanDebugVisible;
}

function setFanDebugVisible(visible) {
  const previous = uiFanDebugVisible;
  uiFanDebugVisible = visible;
  applyFanDebugVisible();
  // 与档位同款:偏好落服务端(随 config.ui_prefs 下发,跨浏览器一致);
  // 失败静默恢复——开关状态以保存成功为准
  request('api/config/ui-prefs', {
    method: 'POST',
    body: JSON.stringify(uiPrefsBody({ fan_debug_visible: visible })),
  }).catch(() => {
    uiFanDebugVisible = previous;
    applyFanDebugVisible();
  });
}

function applyFanDebugVisible() {
  const visible = fanDebugVisiblePref();
  $('fan-debug-visible').checked = visible;
  $('fan-debug-card').hidden = !visible;
  if (visible) startFanDebugPoll(); else stopFanDebugPoll();
}

// status 到达:采纳后端偏好;首次升级做一次 localStorage 迁移——旧版只存
// 本地,后端从未有值,迁移"开着"的用户;迁移后清键,此后关开都走服务端
function syncFanDebugVisibleFromStatus() {
  const serverVisible = currentStatus?.config?.ui_prefs?.fan_debug_visible === true;
  if (serverVisible === uiFanDebugVisible) {
    if (serverVisible) { try { window.localStorage.removeItem(FAN_DEBUG_VISIBLE_KEY); } catch (error) { /* 忽略 */ } }
    return;
  }
  let legacy = false;
  try { legacy = window.localStorage.getItem(FAN_DEBUG_VISIBLE_KEY) === '1'; } catch (error) { /* 忽略 */ }
  if (!serverVisible && legacy) {
    // 旧版开着 → 迁移为服务端开启(带档位,同一次保存),成功后清旧键
    try { window.localStorage.removeItem(FAN_DEBUG_VISIBLE_KEY); } catch (error) { /* 忽略 */ }
    saveUIPrefsQuietly({ fan_debug_visible: true }).then(() => { uiFanDebugVisible = true; applyFanDebugVisible(); });
    return;
  }
  uiFanDebugVisible = serverVisible;
  applyFanDebugVisible();
}

// 页面加载即按记忆的偏好恢复显隐(勾选过则卡片直接可见);status 到达后
// syncFanDebugVisibleFromStatus 以服务端值为准
applyFanDebugVisible();

function startFanDebugPoll() {
  if (fanDebugPollTimer) return;
  const poll = async () => {
    try {
      renderFanDebug(await request('api/fans/debug'));
    } catch (error) {
      $('fan-debug-status').textContent = `调试状态读取失败：${error.message}`;
    }
  };
  poll();
  fanDebugPollTimer = window.setInterval(poll, 2000);
}

function stopFanDebugPoll() {
  if (fanDebugPollTimer) { window.clearInterval(fanDebugPollTimer); fanDebugPollTimer = 0; }
}

function fanDebugStatus(message, error = false) {
  showMessage(message, error, 'fan-debug-status');
}

// 汇总状态行文案:紧急覆盖优先,其次列出正在递增/已完成的风扇;无事可说返回 null
function fanDebugStatusInfo(state) {
  if (state.emergency) {
    return { error: true, text: '⚠ CPU 超过紧急温度，已强制全部风扇 100%（覆盖调试转速）' };
  }
  const fans = state.fans || [];
  const label = (fan) => fan.name || fan.id;
  const running = fans.filter((fan) => fan.auto_running).map(label);
  if (running.length) {
    return { text: `自动递增进行中：${running.join('、')} 按各自的递增值与间隔独立推进，到上限自动完成并保持。` };
  }
  const done = fans.filter((fan) => fan.auto_done).map(label);
  if (done.length) {
    return { text: `已完成自动递增：${done.join('、')} 已停在上限；重新打开行内开关可从 0 重跑。` };
  }
  return null;
}

function applyFanDebugStatus(state) {
  const info = fanDebugStatusInfo(state);
  if (info) fanDebugStatus(info.text, Boolean(info.error));
  else fanDebugStatus('');
}

// 风扇调试单位表:转速 RPM 按 2000 RPM=100% 线性换算;PWM 原始值不带符号
const FAN_DEBUG_UNITS = [
  { value: 'rpm', label: '转速 RPM', max: 2000, suffix: 'RPM' },
  { value: 'percent', label: '百分比 %', max: 100, suffix: '%' },
  { value: 'pwm', label: 'PWM', max: 255, suffix: '' },
];

function fanDebugUnit(value) {
  return FAN_DEBUG_UNITS.find((unit) => unit.value === value) || FAN_DEBUG_UNITS[1];
}

// 单位间线性换算(按各自上限等比):50% ↔ 128 PWM ↔ 1000 RPM。
// rpmMax 是该风扇的满转基准(后端全速实测自动标定,0/缺省用名义值 2000)。
function convertDebugValue(value, fromUnit, toUnit, rpmMax = 0) {
  const toMax = toUnit === 'rpm' && rpmMax ? rpmMax : fanDebugUnit(toUnit).max;
  const fromMax = fromUnit === 'rpm' && rpmMax ? rpmMax : fanDebugUnit(fromUnit).max;
  return Math.round((value * toMax) / fromMax);
}

// 调试值输入框 + 单位后缀;unitDef 形如 FAN_DEBUG_UNITS 项({max, suffix})。
// 外层包 inline-flex 容器:td 不能直接设 flex(会脱离 table-cell 布局,列错位)。
function buildFanDebugInput(unitDef, className, kind, fanId, ariaLabel) {
  const input = document.createElement('input');
  input.type = 'number';
  input.min = kind === 'interval' ? '1' : '0';
  if (Number.isFinite(unitDef.max)) input.max = String(unitDef.max);
  input.step = '1';
  input.inputMode = 'numeric';
  input.className = className;
  input.dataset.fanId = fanId;
  input.dataset.kind = kind;
  input.setAttribute('aria-label', ariaLabel);
  const wrap = document.createElement('span');
  wrap.className = 'fan-debug-inline';
  wrap.append(input);
  if (unitDef.suffix) {
    const mark = document.createElement('span');
    mark.className = 'fan-debug-suffix';
    mark.textContent = unitDef.suffix;
    wrap.append(mark);
  }
  return { input, wrap };
}

// 渲染调试状态;每行一个自动测试滑动开关,各风扇独立启停互不影响
function renderFanDebug(state) {
  const body = $('fan-debug-body');
  // 正在行内编辑数字/单位时不重绘表格,避免 2 秒轮询打断输入(状态行照常更新)
  const active = document.activeElement;
  if (active && body.contains(active) && (active.type === 'number' || active.tagName === 'SELECT')) {
    applyFanDebugStatus(state);
    return;
  }
  const pending = {};
  body.querySelectorAll('input[data-fan-id][data-kind], select[data-fan-id][data-kind]').forEach((field) => {
    pending[field.dataset.fanId + ':' + field.dataset.kind] = field.value;
  });
  body.replaceChildren();
  (state.fans || []).forEach((fan) => {
    const row = document.createElement('tr');
    const takeCell = document.createElement('td');
    const takeBox = document.createElement('input');
    takeBox.type = 'checkbox';
    takeBox.dataset.fanId = fan.id;
    takeBox.checked = fan.taken_over;
    takeBox.setAttribute('aria-label', `接管 ${fan.id}`);
    takeBox.addEventListener('change', () => applyFanTakeover(fan.id, takeBox.checked));
    takeCell.append(takeBox);
    const name = document.createElement('td');
    name.textContent = fan.name;
    const channel = document.createElement('td');
    channel.textContent = fan.channel;
    const rpm = document.createElement('td');
    rpm.textContent = `${fan.rpm} RPM`;
    const pwm = document.createElement('td');
    pwm.textContent = `${fan.pwm_percent}%`;
    const mode = document.createElement('td');
    mode.textContent = fan.mode === 1 ? '手动' : fan.mode === 0 ? '全速' : `自动(${fan.mode})`;
    const debugCell = document.createElement('td');
    debugCell.className = 'fan-debug-pwm-cell';
    const stepCell = document.createElement('td');
    stepCell.className = 'fan-debug-step-cell';
    const intervalCell = document.createElement('td');
    intervalCell.className = 'fan-debug-step-cell';
    const autoCell = document.createElement('td');
    autoCell.className = 'fan-debug-auto-cell';
    if (fan.taken_over) {
      const unit = document.createElement('select');
      unit.className = 'fan-debug-unit';
      unit.dataset.fanId = fan.id;
      unit.dataset.kind = 'unit';
      for (const def of FAN_DEBUG_UNITS) {
        const option = document.createElement('option');
        option.value = def.value;
        option.textContent = def.label;
        unit.append(option);
      }
      unit.value = pending[fan.id + ':unit'] ?? fan.debug_unit ?? 'percent';
      // 自动递增进行中/已完成时输入框跟随后端实际值,避免停留在旧的手动值
      const followsAuto = fan.auto_running || fan.auto_done;
      const debugUnit = fanDebugUnit(unit.value);
      // 该风扇的满转基准:RPM 单位的输入上限与换算分母(后端全速实测标定)
      const rpmMax = fan.rpm_max || 2000;
      const unitMax = (def) => (def.value === 'rpm' ? rpmMax : def.max);
      const debugWrap = buildFanDebugInput({ ...debugUnit, max: unitMax(debugUnit) }, 'fan-debug-pwm-input', 'pwm', fan.id, `调试转速 ${fan.id}`);
      const input = debugWrap.input;
      input.value = (followsAuto ? String(fan.debug_percent ?? 0) : pending[fan.id + ':pwm']) ?? String(fan.debug_percent ?? 0);
      const apply = document.createElement('button');
      apply.type = 'button';
      apply.textContent = '应用';
      apply.className = 'fan-debug-apply';
      apply.addEventListener('click', () => applyFanDebugPWM(fan.id, Number(input.value), unit.value, undefined, rpmMax));
      // 标定满转:全速运转至读数稳态(最多约 9 秒),学特性表最高档与满转基准
      const calibrate = document.createElement('button');
      calibrate.type = 'button';
      calibrate.textContent = '标定';
      calibrate.className = 'fan-debug-apply fan-debug-calibrate';
      calibrate.title = '全速运转数秒,实测满转转速并更新该风扇的 RPM 换算基准';
      calibrate.addEventListener('click', async () => {
        calibrate.disabled = true;
        apply.disabled = true;
        fanDebugStatus(`正在标定 ${fan.id} 的满转转速，风扇将全速运转数秒…`);
        try {
          const result = await request('api/fans/debug/calibrate', { method: 'POST', body: JSON.stringify({ id: fan.id }) });
          fanDebugStatus(`标定完成：${fan.id} 满转基准 ${result.base} RPM（RPM 单位上限已同步）。`);
        } catch (error) {
          fanDebugStatus(`标定失败：${error.message}`, true);
        } finally {
          calibrate.disabled = false;
          apply.disabled = false;
        }
      });
      let prevUnit = unit.value;
      unit.addEventListener('change', () => {
        const to = fanDebugUnit(unit.value);
        const from = fanDebugUnit(prevUnit);
        // 输入框与递增值按比例换算到新单位(RPM/%/PWM 上限等比)
        const v = Number(input.value);
        if (Number.isFinite(v)) {
          input.value = String(convertDebugValue(v, from.value, to.value, rpmMax));
        }
        const s = Number(stepInput.value);
        if (Number.isFinite(s) && s > 0) {
          stepInput.value = String(convertDebugValue(s, from.value, to.value, rpmMax));
        }
        input.max = String(unitMax(to));
        stepInput.max = String(unitMax(to));
        setCellSuffix(debugCell, to.suffix);
        setCellSuffix(stepCell, to.suffix);
        prevUnit = unit.value;
        // 同步后端:基准值按新单位存储(同物理输出,只是换算存储),递增/完成判定跟随新单位
        if (Number.isFinite(v)) {
          applyFanDebugPWM(fan.id, Number(input.value), unit.value, `单位已切换为${to.label}，已按新单位同步调试值。`, rpmMax);
        }
      });
      const stepWrap = buildFanDebugInput({ ...debugUnit, max: unitMax(debugUnit) }, 'fan-debug-auto-input', 'step', fan.id, `递增转速 ${fan.id}`);
      const stepInput = stepWrap.input;
      stepInput.value = pending[fan.id + ':step'] ?? String(fan.auto_step || 5);
      const intervalWrap = buildFanDebugInput({ suffix: '秒' }, 'fan-debug-auto-input', 'interval', fan.id, `递增间隔 ${fan.id}`);
      const intervalInput = intervalWrap.input;
      intervalInput.value = pending[fan.id + ':interval'] ?? String(fan.auto_interval || 10);
      // 自动测试运行中锁定整行参数(单位/调试值/应用/标定/递增/间隔):中途
      // 改动会破坏递增进程;开关本身保持可操作(随时可停)。完成后(auto_done)
      // 解锁。锁定随 2 秒轮询的重绘持续生效,绕过 UI 的改动还有后端拒绝兜底。
      const autoLocked = Boolean(fan.auto_running);
      unit.disabled = autoLocked;
      input.disabled = autoLocked;
      apply.disabled = autoLocked;
      calibrate.disabled = autoLocked;
      stepInput.disabled = autoLocked;
      intervalInput.disabled = autoLocked;
      debugCell.append(unit, debugWrap.wrap, apply, calibrate);
      stepCell.append(stepWrap.wrap);
      intervalCell.append(intervalWrap.wrap);
      // 自动测试滑动开关:开=按该行步进/间隔/单位从当前调试值递增;到上限自动弹回并标记完成
      const autoToggle = document.createElement('label');
      autoToggle.className = 'toggle fan-debug-auto-toggle';
      const autoBox = document.createElement('input');
      autoBox.type = 'checkbox';
      autoBox.dataset.fanId = fan.id;
      autoBox.checked = fan.auto_running;
      autoBox.setAttribute('aria-label', `${fan.id} 自动递增测试${fan.auto_done ? '（已完成，重新打开可从 0 重跑）' : ''}`);
      autoBox.addEventListener('change', () => applyFanDebugAuto(fan.id, autoBox.checked, Number(stepInput.value) || 5, Number(intervalInput.value) || 10, unit.value));
      autoToggle.append(autoBox, document.createElement('span'));
      autoCell.append(autoToggle);
      if (fan.auto_done) {
        const done = document.createElement('span');
        done.className = 'fan-debug-done';
        done.textContent = '✓ 已完成';
        autoCell.append(done);
      }
    }
    row.append(takeCell, name, channel, rpm, pwm, mode, debugCell, stepCell, intervalCell, autoCell);
    body.append(row);
  });
  applyFanDebugStatus(state);
}

// 替换单元格里输入框后面的单位后缀(RPM/%/秒;空串=移除)
function setCellSuffix(cell, text) {
  let mark = cell.querySelector('.fan-debug-suffix');
  if (!text) {
    if (mark) mark.remove();
    return;
  }
  if (!mark) {
    mark = document.createElement('span');
    mark.className = 'fan-debug-suffix';
    cell.append(mark);
  }
  mark.textContent = text;
}

$('fan-debug-visible').addEventListener('change', () => setFanDebugVisible($('fan-debug-visible').checked));

// ---- 长期记录保存位置：官方系统目录选择器（fnOS 应用 SDK pickSharedFile）----

// SDK 单例：动态 import vendor 的 @trimjs/web-app ESM 构建，ready() 握手
// 超时 8 秒防卡死；失败后下次点击重新初始化。
let fsSdkPromise = null;

function getFsSdk() {
  if (!fsSdkPromise) {
    fsSdkPromise = (async () => {
      const mod = await import('./fnos-web-app.js');
      const sdk = new mod.TrimApp();
      await Promise.race([
        sdk.ready(),
        new Promise((_, reject) => setTimeout(() => reject(new Error('初始化超时')), 8000)),
      ]);
      return sdk;
    })().catch((error) => {
      fsSdkPromise = null;
      throw error;
    });
  }
  return fsSdkPromise;
}

async function browseArchiveDir() {
  const button = $('history-archive-browse');
  button.disabled = true;
  try {
    const sdk = await getFsSdk();
    if (sdk.isStandaloneWeb) {
      showMessage('请在 fnOS 桌面内打开本插件后再选择目录，独立浏览器无法调起系统选择器。', true, 'message-history');
      return;
    }
    const result = await sdk.pickSharedFile({
      title: '选择长期记录保存位置',
      okText: '选择此目录',
      sidebarGroup: ['myFiles', 'otherShare', 'favorites', 'external'],
    });
    if (result && result.code !== 0) {
      showMessage(`目录选择失败：${result.msg || '未知错误'}`, true, 'message-history');
      return;
    }
    if (Array.isArray(result?.data) && result.data.length) {
      // 共享授权目录仅单选，取第一个路径回填
      $('history-archive-dir').value = result.data[0];
    }
  } catch (error) {
    showMessage(`系统目录选择器不可用：${error.message}。可直接手动输入路径；该功能需要 fnOS 1.2.0401 及以上版本。`, true, 'message-history');
  } finally {
    button.disabled = false;
  }
}

$('history-archive-browse').addEventListener('click', browseArchiveDir);

async function applyFanTakeover(id, taken) {
  setBusy(true);
  try {
    renderFanDebug(await request('api/fans/debug/takeover', { method: 'POST', body: JSON.stringify({ id, taken }) }));
    fanDebugStatus(taken ? `已接管 ${id}，该风扇脱离温控曲线。` : `已释放 ${id}，恢复曲线控制。`);
  } catch (error) {
    fanDebugStatus(`接管切换失败：${error.message}`, true);
    startFanDebugPoll(); // 立即轮询校正界面状态
  } finally {
    setBusy(false);
  }
}

// 单风扇自动递增的开/关:开=以当前调试值为基准,按该行步进/间隔/单位递增;
// 关=停在当前转速(参数保留,重新打开原样继续)。各风扇互不影响。
async function applyFanDebugAuto(id, running, step, interval, unit) {
  // 输入框 step=1 只约束步进点击,手输 1.5 这类小数会原样到这——后端字段
  // 是整数,不取整会 JSON decode 失败报 400
  step = Math.round(step);
  interval = Math.round(interval);
  const label = id.split(':').pop();
  setBusy(true);
  try {
    const state = running
      ? await request('api/fans/debug/auto', { method: 'POST', body: JSON.stringify({ fans: [{ id, step, interval, unit }] }) })
      : await request('api/fans/debug/auto/stop', { method: 'POST', body: JSON.stringify({ id }) });
    renderFanDebug(state);
    fanDebugStatus(running ? `${label} 自动递增已开始。` : `${label} 自动递增已停止，保持当前转速。`);
  } catch (error) {
    fanDebugStatus(`自动递增切换失败：${error.message}`, true);
    startFanDebugPoll(); // 立即轮询校正开关状态
  } finally {
    setBusy(false);
  }
}

// rpmMax 为该风扇实测满转基准(标定后 >2000):rpm 单位的上限按它校验,
// 否则标定过的高速风扇会被 2000 默认值误拒(单位切换的换算值同理)
async function applyFanDebugPWM(id, value, unit, successMessage, rpmMax) {
  value = Math.round(value); // 同 applyFanDebugAuto:手输小数不得原样发给整数后端
  const def = fanDebugUnit(unit);
  const max = unit === 'rpm' ? (rpmMax || def.max) : def.max;
  if (!Number.isFinite(value) || value < 0 || value > max) {
    fanDebugStatus(`调试值需在 0–${max} 之间。`, true);
    return;
  }
  setBusy(true);
  try {
    renderFanDebug(await request('api/fans/debug/pwm', { method: 'POST', body: JSON.stringify({ id, value, unit }) }));
    fanDebugStatus(successMessage || `已应用 ${id} ${def.label} ${value}。`);
  } catch (error) {
    fanDebugStatus(`应用失败：${error.message}`, true);
  } finally {
    setBusy(false);
  }
}

function renderFanRPMs(fanStatus = {}) {
  const target = $('fan-rpm-list');
  const fans = connectedFans(fanStatus, 4);
  const existing = [...target.querySelectorAll('[data-fan-id]')];
  if (existing.map((item) => item.dataset.fanId).join('|') !== fans.map((fan) => fan.id).join('|')) {
    target.replaceChildren();
    fans.forEach((fan) => {
      const item = document.createElement('p');
      item.dataset.fanId = fan.id;
      item.append(document.createElement('span'), document.createElement('strong'));
      target.append(item);
    });
    if (!fans.length) target.innerHTML = '<strong id="fan-rpm">不可用</strong>';
  }
  fans.forEach((fan) => {
    const item = target.querySelector(`[data-fan-id="${fan.id}"]`);
    item.querySelector('span').textContent = fanLabel(fan, fans);
    item.querySelector('strong').textContent = `${fan.rpm} RPM`;
  });
}

function resolvedPowerPreset(status = {}, mode) {
  const profile = status.profile || {};
  const preset = profile.power_presets?.[mode];
  if (!preset) return null;
  const minPL1 = Number(profile.min_pl1_w) || 1;
  const maxPL1 = Number(status.effective_max_pl1_w || profile.max_pl1_w) || Number.POSITIVE_INFINITY;
  const maxPL2 = Number(status.effective_max_pl2_w || profile.max_pl2_w) || Number.POSITIVE_INFINITY;
  const pl1W = Math.max(minPL1, Math.min(Number(preset.pl1_w), maxPL1));
  const pl2W = Math.max(pl1W, Math.min(Number(preset.pl2_w), maxPL2));
  return { pl1_w: pl1W, pl2_w: pl2W };
}

function detectedPowerMode(status = {}, config = {}) {
  for (const mode of ['saving', 'standard', 'performance']) {
    const preset = resolvedPowerPreset(status, mode);
    if (preset && Number(preset.pl1_w) === Number(config.pl1_w) && Number(preset.pl2_w) === Number(config.pl2_w)) return mode;
  }
  return 'custom';
}

function updatePowerMode(mode, applyPreset = true) {
  const selected = document.querySelector(`input[name="power-mode"][value="${mode}"]`)
    || document.querySelector('input[name="power-mode"][value="custom"]');
  selected.checked = true;
  const preset = resolvedPowerPreset(currentStatus || {}, selected.value);
  if (applyPreset && preset) {
    $('pl1').value = preset.pl1_w;
    $('pl2').value = preset.pl2_w;
  }
  const custom = selected.value === 'custom';
  $('pl1').readOnly = !custom;
  $('pl2').readOnly = !custom;
  $('power-custom-fields').classList.toggle('preset-active', !custom);
}

function render(status, keepInputs = false) {
  currentStatus = status;
  syncFanDebugVisibleFromStatus();
  const pkg = status.packages?.[0] || {};
  const cpuTemperature = status.cpu_temperature || {};
  const fanStatus = status.fan_control || {};
  const storageStatus = status.storage || {};
  const gpioStatus = status.gpio || {};
  const versionText = status.version ? `v${status.version}` : 'v—';
  $('app-version').textContent = versionText;
  $('about-version').textContent = versionText;
  $('device-name').textContent = status.device_name || 'TAD6S4N';
  $('os-version').textContent = [status.os_name, status.os_version].filter(Boolean).join(' ')
    || ({ fnos: 'fnOS', dsm: '群晖 DSM' }[status.platform] || 'fnOS');
  $('cpu-model').textContent = status.cpu_model || '未识别';
  $('cpu-display-label').textContent = cpuTemperature.display_source === 'package_fallback'
    ? 'CPU 温度（Package 回退）'
    : 'CPU 核心最高';
  $('cpu-display-temperature').textContent = formatTemperature(cpuTemperature.display_c, cpuTemperature.available);
  $('package-temperature').textContent = formatTemperature(cpuTemperature.package_max_c, Number(cpuTemperature.package_sensors) > 0);
  const powerUnavailable = status.power_supported === false ? '平台不支持 RAPL' : '不可用';
  $('current-pl1').textContent = pkg.has_pl1 ? `${pkg.pl1_w} W` : powerUnavailable;
  $('current-pl2').textContent = pkg.has_pl2 ? `${pkg.pl2_w} W` : powerUnavailable;
  renderFanRPMs(fanStatus);
  $('profile').textContent = status.profile?.display || '不支持';
  $('temperature-source').textContent = cpuTemperature.display_source === 'core_max_rr'
    ? `RR 核心最大值（${cpuTemperature.core_sensors} 个 Core）`
    : (cpuTemperature.display_source === 'package_fallback' ? '未找到 Core 标签，回退到 Package' : '未识别');
  $('last-apply').textContent = status.last_apply ? new Date(status.last_apply).toLocaleString() : '尚未应用';
  const diagnosticError = status.last_error || fanStatus.last_error || '';
  $('last-error').textContent = diagnosticError || '正常';
  $('last-error').className = diagnosticError ? 'diagnostic-error' : 'diagnostic-ok';
  renderDiagnostics(status, fanStatus);
  renderStorageVisual(storageStatus);
  renderStorageMobile(storageStatus);
  renderStorageTable(storageStatus);
  renderFanSlotTemperatures(storageStatus);
  renderFanHardwareWarning(fanStatus);
  syncFanAlarmMute(fanStatus);
  $('gpio-status').textContent = gpioStatus.enabled
    ? (gpioStatus.available ? `监听中${gpioStatus.last_event ? ` · 最近：${gpioStatus.last_event}` : ''}` : `已启用但不可用：${gpioStatus.last_error || '无法读取 /dev/port'}`)
    : (gpioStatus.available ? '硬件接口可用，按键映射尚未启用。' : '未检测到 GPIO 硬件接口。');
  $('gpio-status').className = `inline-status${gpioStatus.enabled && (!gpioStatus.available || gpioStatus.last_error) ? ' error' : ''}`;
  renderStorageNotifyStatus(status.storage_notify);
  const issues = healthIssues(status, fanStatus, storageStatus, gpioStatus);
  const healthy = issues.length === 0;
  const healthMessage = healthy
    ? '未发现需要处理的异常。'
    : `需要检查以下项目：\n${issues.map((issue) => `• ${issue}`).join('\n')}`;
  renderHealthBadge(healthy ? '运行正常' : '需要检查', healthy ? 'ok' : 'error', healthMessage);

  const profile = status.profile || {};
  const maxPL1 = status.effective_max_pl1_w || profile.max_pl1_w || 0;
  const maxPL2 = status.effective_max_pl2_w || profile.max_pl2_w || 0;
  $('pl1').min = profile.min_pl1_w || 1;
  $('pl1').max = maxPL1;
  $('pl2').min = profile.min_pl1_w || 1;
  $('pl2').max = maxPL2;
  $('pl1-range').textContent = `允许 ${$('pl1').min}–${maxPL1} W，推荐 ${profile.default_pl1_w || '—'} W`;
  $('pl2-range').textContent = `不低于 PL1，最高 ${maxPL2} W，推荐 ${profile.default_pl2_w || '—'} W`;
  populateFanDevices(status, keepInputs);
  if (!keepInputs) {
    $('enabled').checked = Boolean(status.config?.enabled);
    $('pl1').value = status.config?.pl1_w ?? profile.default_pl1_w ?? '';
    $('pl2').value = status.config?.pl2_w ?? profile.default_pl2_w ?? '';
    $('interval').value = status.config?.reapply_seconds ?? 30;
    updatePowerMode(detectedPowerMode(status, status.config || {}), false);
    fillFanInputs(status.config?.fan);
    fillGPIOInputs(status.config?.gpio);
    applyBackendHistoryRange(status.config?.ui_prefs); // 后端档位先采纳，保留天数钳制随后生效
    applyBackendHistorySeries(status.config?.ui_prefs); // 曲线显隐偏好同批采纳（跨浏览器一致）
    fillHistoryInputs(status.config?.history);
    syncRunLogInputs(status);
  }
  renderSerialSensor(status.serial);
  CURVE_KINDS.forEach(renderFanChart);
}

function showMessage(message, error = false, targetID = 'message-global') {
  const target = $(targetID);
  target.textContent = message;
  target.className = `message${error ? ' error' : ''}`;
}

function setDebugStatus(message, error = false) {
  const target = $('debug-status');
  target.textContent = message;
  target.className = `message${error ? ' error' : ''}`;
}

// 宿主窗口探测（仅采集结构信息，脱敏）：fnOS 桌面标题栏由宿主绘制、无官方
// 配色接口，深色模式下是否变深取决于宿主实现。此处从插件 iframe 向上最多
// 12 层记录祖先链的标签/id/类名与计算背景色，用于在真机上定位窗口外壳的
// 真实 DOM 结构；不采集任何文本内容与 URL。
function collectHostWindowInfo() {
  const info = {
    embedded: window.parent !== window,
    parentReadable: false,
    parentThemeMode: null,
    iframe: null,
    ancestors: [],
  };
  try {
    if (window.parent === window) return info;
    const parent = window.parent;
    const doc = parent.document;
    if (!doc || !doc.body) return info;
    info.parentReadable = true;
    info.parentThemeMode = (doc.body.getAttribute('theme-mode') || '').trim().toLowerCase() || null;
    let frame = null;
    doc.querySelectorAll('iframe').forEach((candidate) => {
      if (frame) return;
      try { if (candidate.contentWindow === window) frame = candidate; } catch (error) { /* 跨域框架跳过 */ }
    });
    if (!frame) return info;
    info.iframe = {
      id: frame.id || null,
      class: (`${frame.className || ''}`).slice(0, 120) || null,
    };
    let node = frame.parentElement;
    for (let depth = 0; node && depth < 12; depth += 1, node = node.parentElement) {
      const style = parent.getComputedStyle(node);
      info.ancestors.push({
        depth,
        tag: node.tagName.toLowerCase(),
        id: node.id || null,
        class: (`${node.className || ''}`).slice(0, 160) || null,
        backgroundColor: style.backgroundColor,
        color: style.color,
      });
    }
  } catch (error) {
    info.error = String((error && error.message) || error);
  }
  return info;
}

// 页面诊断：报告生成时刻的环境快照 + 采集到的页面错误（localStorage 里
// 跨刷新保留）。白屏类问题真机无控制台，靠这份快照定位：哪块面板消失、
// 是否有运行期异常。取走即清空，报告之间不重复累计。
function collectPageDiagnostics() {
  const diagnostics = {
    viewport: `${window.innerWidth}x${window.innerHeight}`,
    theme: String(document.documentElement.dataset.theme || '') || null,
    activePanels: [...document.querySelectorAll('.tab-panel')].filter((panel) => !panel.hidden).map((panel) => panel.id),
    shellAttached: Boolean(document.querySelector('main.shell')),
    pageErrors: [],
  };
  try {
    diagnostics.pageErrors = JSON.parse(window.localStorage.getItem(PAGE_ERROR_STORE_KEY) || '[]');
    window.localStorage.setItem(PAGE_ERROR_STORE_KEY, '[]');
  } catch (error) { /* 无存储时保持空数组 */ }
  return diagnostics;
}

function formatDebugReport(payload) {
  const generatedAt = new Date().toISOString();
  let serialized;
  try {
    serialized = JSON.stringify(payload, null, 2);
  } catch (error) {
    throw new Error(`报告格式化失败：${error.message}`);
  }
  let host;
  try {
    host = JSON.stringify(collectHostWindowInfo(), null, 2);
  } catch (error) {
    host = `{"error": ${JSON.stringify(String(error.message))}}`;
  }
  let page;
  try {
    page = JSON.stringify(collectPageDiagnostics(), null, 2);
  } catch (error) {
    page = `{"error": ${JSON.stringify(String(error.message))}}`;
  }
  return `# TAD6S4N10G 调试报告\n\n生成时间：${generatedAt}\n\n以下内容由 GET api/debug/report 返回，用于协助定位模块问题。请在公开发布前检查是否包含敏感信息。\n\n## 接口报告\n\n\`\`\`json\n${serialized}\n\`\`\`\n\n## 宿主窗口环境（页面采集，仅结构信息）\n\n\`\`\`json\n${host}\n\`\`\`\n\n## 页面诊断（错误采集与环境快照）\n\n\`\`\`json\n${page}\n\`\`\``;
}

function updateDebugReportActions(enabled) {
  ['debug-copy-report', 'debug-download-report', 'debug-open-issue'].forEach((id) => { $(id).disabled = !enabled; });
}

async function generateDebugReport() {
  const button = $('debug-generate-report');
  button.disabled = true;
  setDebugStatus('正在生成调试报告…');
  try {
    const payload = await request('api/debug/report');
    const text = formatDebugReport(payload);
    debugReportText = text;
    $('debug-report').value = text;
    $('debug-report-meta').textContent = `已生成 · ${text.length.toLocaleString()} 字符 · ${new Date().toLocaleString()}`;
    updateDebugReportActions(true);
    setDebugStatus('调试报告已生成，可复制、下载或打开 GitHub Issue。');
  } catch (error) {
    setDebugStatus(`生成失败：${error.message}`, true);
  } finally {
    button.disabled = false;
  }
}

async function copyDebugReport() {
  if (!debugReportText) return;
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(debugReportText);
      setDebugStatus('报告已复制到剪贴板。');
      return;
    }
    throw new Error('Clipboard API 不可用');
  } catch (error) {
    const helper = document.createElement('textarea');
    helper.value = debugReportText;
    helper.setAttribute('readonly', '');
    helper.style.position = 'fixed';
    helper.style.opacity = '0';
    document.body.append(helper);
    helper.select();
    let copied = false;
    try { copied = document.execCommand('copy'); } catch (copyError) { copied = false; }
    helper.remove();
    if (copied) {
      setDebugStatus('报告已复制到剪贴板。');
      return;
    }
    $('debug-report').focus();
    $('debug-report').select();
    setDebugStatus('自动复制失败，报告已选中，请按 Ctrl+C 手动复制。', true);
  }
}

// ---- 调试页：传感器显示名设置 ----

function setupSensorNames() {
  const saveButton = $('sensor-names-save');
  if (!saveButton) return;
  saveButton.addEventListener('click', saveSensorNames);
}

// 父类下拉可选项：与后端 sensorGroupValues 保持一致（gpu|nic|other）。
const SENSOR_GROUP_OPTIONS = [
  { value: 'gpu', label: 'GPU' },
  { value: 'nic', label: '网卡' },
  { value: 'other', label: '其它' },
];

// 从历史数据里发现全部传感器键，渲染成行：默认名 + 显示名输入框 + 父类下拉。
// CPU 组传感器（核心温度）固定归 CPU 父类，不下拉。
function renderSensorNamesList() {
  const wrap = $('sensor-names-list');
  if (!wrap) return;
  const samples = historyCache?.samples || [];
  const groupOf = new Map();
  samples.forEach((sample) => (sample.sensors || []).forEach((sensor) => {
    if (sensor.group && sensor.key && !groupOf.has(sensor.key)) groupOf.set(sensor.key, sensor.group);
  }));
  const keys = [...groupOf.keys()].sort();
  wrap.replaceChildren();
  if (!keys.length) {
    const empty = document.createElement('p');
    empty.className = 'debug-report-meta';
    empty.textContent = '暂未发现传感器，请先打开历史温度页等待数据加载。';
    wrap.append(empty);
    return;
  }
  const saved = currentStatus?.config?.sensor_names || {};
  const savedGroups = currentStatus?.config?.sensor_groups || {};
  keys.forEach((key) => {
    const row = document.createElement('div');
    row.className = 'sensor-name-row';
    const keyLabel = document.createElement('span');
    keyLabel.className = 'sensor-name-key';
    keyLabel.textContent = key;
    const input = document.createElement('input');
    input.type = 'text';
    input.className = 'sensor-name-input';
    input.dataset.sensorKey = key;
    input.value = saved[key] || '';
    input.placeholder = historyChildLabel(key.startsWith('Core ') || key.startsWith('Package') ? 'cpu' : 'other', key);
    input.maxLength = 40;
    if (groupOf.get(key) === 'cpu') {
      const fixed = document.createElement('span');
      fixed.className = 'sensor-name-group-fixed';
      fixed.textContent = 'CPU';
      row.append(keyLabel, input, fixed);
    } else {
      const select = document.createElement('select');
      select.className = 'sensor-name-group';
      select.dataset.sensorKey = key;
      // 默认归属随 /api/history 下发（default_groups）；选回默认组时保存
      // 端会自动清除覆盖，跟随驱动表自动归类。
      select.dataset.defaultGroup = historyCache?.default_groups?.[key] || groupOf.get(key);
      select.title = '归属父类';
      SENSOR_GROUP_OPTIONS.forEach(({ value, label }) => {
        const option = document.createElement('option');
        option.value = value;
        option.textContent = label;
        select.append(option);
      });
      // 当前生效父类：用户覆盖优先，否则即默认组
      select.value = savedGroups[key] || select.dataset.defaultGroup;
      row.append(keyLabel, input, select);
    }
    wrap.append(row);
  });
}

async function saveSensorNames() {
  const status = $('sensor-names-status');
  const names = {};
  document.querySelectorAll('.sensor-name-input').forEach((input) => {
    const value = input.value.trim();
    if (value) names[input.dataset.sensorKey] = value;
  });
  const groups = {};
  document.querySelectorAll('.sensor-name-group').forEach((select) => {
    if (select.value && select.value !== select.dataset.defaultGroup) groups[select.dataset.sensorKey] = select.value;
  });
  try {
    const updated = await request('api/config/sensor-names', { method: 'POST', body: JSON.stringify({ names, groups }) });
    render(updated, true);
    if (status) status.textContent = `已保存 ${Object.keys(names).length} 个显示名、${Object.keys(groups).length} 个父类归属。`;
    // 父类归属在读端生效：强制刷新历史数据，曲线立即搬到新父类
    await fetchHistory(true);
    renderHistoryChart();
  } catch (error) {
    if (status) status.textContent = `保存失败：${error.message}`;
  }
}

// ---- 调试页：USB 串口温度传感器（usb:tty:* 读数走「其它」分组，多设备） ----

const SERIAL_BAUD_RATES = [2400, 4800, 9600, 19200, 38400, 57600, 115200];

function setupSerialSensor() {
  const rows = $('serial-sensor-rows');
  if (!rows) return;
  $('serial-sensor-add').addEventListener('click', () => {
    addSerialRow(null);
    serialFormDirty = true;
  });
  $('serial-sensor-save').addEventListener('click', saveSerialSensor);
}

// renderSerialSensor 同步串口传感器卡：设备下拉选项每轮刷新（插拔自动出
// 现），配置行集合只在用户没有未保存修改时跟随服务端，各行状态文本常刷。
function renderSerialSensor(serial) {
  const rows = $('serial-sensor-rows');
  if (!rows) return;
  const configs = serial?.configs || [];
  const devices = serial?.devices || [];
  if (!serialFormDirty) {
    rows.replaceChildren();
    configs.forEach((config) => appendSerialRow(config, devices));
    if (!configs.length) {
      const empty = document.createElement('p');
      empty.className = 'serial-sensor-empty';
      empty.textContent = '尚未配置串口传感器。点击「添加传感器」后选择设备与波特率。';
      rows.append(empty);
    }
  } else {
    // 已有行只刷新设备选项与状态文本，保留用户未保存的选择
    [...rows.querySelectorAll('.serial-sensor-config')].forEach((row) => {
      const path = row.dataset.path;
      fillSerialDeviceOptions(row.querySelector('.serial-row-device'), devices, path);
      const status = serialSensorStatusText(configs.find((c) => c.path === path), Date.now());
      const line = row.querySelector('.serial-row-status');
      line.textContent = status;
    });
  }
}

// appendSerialRow 追加一行配置（config 为空表示新添加的空行）。
function appendSerialRow(config, devices) {
  const row = document.createElement('div');
  row.className = 'serial-sensor-config';
  row.dataset.path = config?.path || '';

  const toggle = document.createElement('label');
  toggle.className = 'toggle serial-row-enabled';
  const checkbox = document.createElement('input');
  checkbox.type = 'checkbox';
  checkbox.checked = Boolean(config?.enabled);
  checkbox.addEventListener('change', () => { serialFormDirty = true; });
  const span = document.createElement('span');
  toggle.append(checkbox, span);

  const device = document.createElement('select');
  device.className = 'serial-row-device';
  device.addEventListener('change', () => {
    row.dataset.path = device.value;
    serialFormDirty = true;
  });
  fillSerialDeviceOptions(device, devices, config?.path || '');

  const baud = document.createElement('select');
  baud.className = 'serial-row-baud';
  SERIAL_BAUD_RATES.forEach((rate) => {
    const option = document.createElement('option');
    option.value = String(rate);
    option.textContent = `${rate} bps`;
    if (rate === (config?.baud || 9600)) option.selected = true;
    baud.append(option);
  });
  baud.addEventListener('change', () => { serialFormDirty = true; });

  const remove = document.createElement('button');
  remove.type = 'button';
  remove.className = 'serial-row-remove';
  remove.textContent = '删除';
  remove.addEventListener('click', () => {
    row.remove();
    serialFormDirty = true;
  });

  const status = document.createElement('p');
  status.className = 'serial-row-status message';
  status.textContent = serialSensorStatusText(config, Date.now());

  row.append(toggle, device, baud, remove, status);
  $('serial-sensor-rows').append(row);
}

function addSerialRow() {
  appendSerialRow({ enabled: true, baud: 9600 }, currentStatus?.serial?.devices || []);
}

// fillSerialDeviceOptions 填充一行的设备下拉：候选设备 + 已配置但当前未检
// 测到的路径（保留选项避免误改配置）；selected 为当前应选中的路径。
function fillSerialDeviceOptions(select, devices, selected) {
  const options = [...devices.map((d) => ({ path: d.path, label: `${d.label}（${d.path}）` }))];
  if (selected && !devices.some((d) => d.path === selected)) {
    options.push({ path: selected, label: `${selected}（未检测到）` });
  }
  if (!options.length) {
    options.push({ path: '', label: '未发现串口设备（插入 USB 转串口后自动出现）' });
  }
  select.replaceChildren();
  options.forEach((option) => {
    const el = document.createElement('option');
    el.value = option.path;
    el.textContent = option.label;
    if (option.path === selected) el.selected = true;
    select.append(el);
  });
}

// serialSensorStatusText 把一行配置的运行态拼成一句话；now 参数便于离线
// 单测注入时钟。有错误但读数还新鲜（<60 秒）时按"瞬断重连中"表述——USB
// 串口偶发 EOF 会自动快速重连，不该吓唬用户；重连失败持续无读数才升级为
// "连接失败"。
function serialSensorStatusText(config, now = Date.now()) {
  if (!config) return '尚未保存。';
  if (!config.enabled) return '未启用（保留配置，不再读取）。';
  // 零值时间戳（旧后端下发的 0001-01-01）解析出负数毫秒，一并当无读数，
  // 避免渲染出“63927157204 秒前”这类纪元差值
  const lastAtMs = config.last_at ? new Date(config.last_at).getTime() : NaN;
  const age = Number.isFinite(lastAtMs) && lastAtMs > 0
    ? Math.max(0, Math.round((now - lastAtMs) / 1000))
    : null;
  const parts = [];
  if (config.open) parts.push('已连接');
  else if (config.last_error) {
    parts.push(age !== null && age <= 60
      ? `读数中断，正在自动重连（${config.last_error}）`
      : `连接失败：${config.last_error}`);
  } else parts.push('等待连接…');
  if (age !== null) {
    parts.push(`最近读数 ${formatTemperature(config.last_celsius, true)}（${age} 秒前）`);
  }
  if (config.key) parts.push(`曲线键 ${config.key}`);
  return `${parts.join('；')}。`;
}

async function saveSerialSensor() {
  const line = $('serial-sensor-status');
  const configs = [...document.querySelectorAll('.serial-sensor-config')].map((row) => ({
    enabled: row.querySelector('.serial-row-enabled input').checked,
    path: row.querySelector('.serial-row-device').value,
    baud: Number(row.querySelector('.serial-row-baud').value) || 9600,
  }));
  const enabledWithoutPath = configs.filter((c) => c.enabled && !c.path).length;
  if (enabledWithoutPath) {
    line.textContent = `有 ${enabledWithoutPath} 个启用的传感器未选择设备路径。`;
    line.className = 'message error';
    return;
  }
  const button = $('serial-sensor-save');
  button.disabled = true;
  try {
    const updated = await request('api/config/serial-sensor', {
      method: 'POST',
      body: JSON.stringify({ configs }),
    });
    serialFormDirty = false;
    render(updated, true);
    const enabled = configs.filter((c) => c.enabled).length;
    line.textContent = enabled
      ? `已保存 ${configs.length} 个配置（启用 ${enabled} 个）。读数出现在历史温度「其它」分组，键以 usb:tty: 开头。`
      : `已保存 ${configs.length} 个配置（全部停用）。`;
  } catch (error) {
    line.textContent = `保存失败：${error.message}`;
    line.className = 'message error';
  } finally {
    button.disabled = false;
  }
}

// ---- 调试页：历史温度数据导出（sql 快照 / csv 宽表） ----

function setupHistoryExport() {
  const toggle = $('history-export-toggle');
  const menu = $('history-export-menu');
  if (!toggle || !menu) return;
  toggle.addEventListener('click', (event) => {
    event.stopPropagation();
    menu.hidden = !menu.hidden;
    toggle.setAttribute('aria-expanded', String(!menu.hidden));
  });
  document.addEventListener('click', (event) => {
    if (!event.target.closest?.('.history-export-wrap')) {
      menu.hidden = true;
      toggle.setAttribute('aria-expanded', 'false');
    }
  });
  $('history-export-sql').addEventListener('click', () => downloadHistoryExport('sql'));
  $('history-export-csv').addEventListener('click', () => downloadHistoryExport('csv'));
}

function downloadHistoryExport(kind) {
  const status = $('debug-status'); // 状态提示复用调试面板的 aria-live 区
  const stamp = new Date().toISOString().slice(0, 10);
  const link = document.createElement('a');
  link.href = baseUrl(`api/history/export/${kind}`);
  link.download = kind === 'sql' ? `tad-module-history-${stamp}.db` : `tad-module-history-${stamp}.csv`;
  document.body.append(link);
  link.click();
  link.remove();
  if (status) status.textContent = kind === 'sql' ? '数据库快照下载已开始。' : 'CSV 导出下载已开始。';
}

function downloadDebugReport() {
  if (!debugReportText) return;
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  const blob = new Blob([debugReportText], { type: 'text/plain;charset=utf-8' });
  const link = document.createElement('a');
  link.href = URL.createObjectURL(blob);
  link.download = `tad-module-debug-report-${stamp}.txt`;
  document.body.append(link);
  link.click();
  link.remove();
  URL.revokeObjectURL(link.href);
  setDebugStatus('报告下载已开始。');
}

function openDebugIssue() {
  if (!debugReportText) return;
  const base = 'https://github.com/luodaoyi/TAD6S4N10G-fnos/issues/new';
  const title = 'TAD6S4N10G 模块问题反馈';
  const body = `请描述问题现象、复现步骤和预期结果。\n\n调试报告：\n\n${debugReportText}`;
  const issueURL = `${base}?${new URLSearchParams({ title, body }).toString()}`;
  if (issueURL.length > 1900) {
    window.open(base, '_blank', 'noopener,noreferrer');
    setDebugStatus('报告内容过长，已打开基础 Issue 页面；请先复制报告再粘贴。', true);
    return;
  }
  window.open(issueURL, '_blank', 'noopener,noreferrer');
  setDebugStatus('已打开 GitHub Issue 草稿，请检查内容后手动提交。');
}

function setBusy(busy) {
  uiBusy = busy;
  $('config-form').classList.toggle('busy', busy);
  document.querySelectorAll('button').forEach((button) => {
    if (button.closest('.app-modal')) return;
    button.disabled = busy;
  });
  if (!busy) updateDebugReportActions(Boolean(debugReportText));
  CURVE_KINDS.forEach(updateCurveControls);
}

async function refresh(keepInputs = false) {
  try {
    const status = await request('api/status');
    render(status, keepInputs);
  } catch (error) {
    showMessage(`读取状态失败：${error.message}`, true);
    renderHealthBadge('连接失败', 'error', `无法读取模块状态：${error.message}`);
  }
  const historyPanel = $('panel-fan');
  if (historyPanel && !historyPanel.hidden) fetchHistory();
}

function reportPanelValidity(panelID) {
  const controls = $(panelID).querySelectorAll('[required]');
  // NodeList 才有数值 length；测试桩里拿到的是不可迭代的占位对象，按“没有必填项”处理
  const list = controls && typeof controls.length === 'number' ? Array.from(controls) : [];
  const invalid = list
    .find((control) => !control.disabled && !control.checkValidity());
  if (!invalid) return true;
  invalid.reportValidity();
  return false;
}

function fanConfigFromInputs() {
  const cpuCurve = curveFromInputs('cpu');
  const hddCurve = curveFromInputs('hdd');
  const nvmeCurve = curveFromInputs('nvme');
  const cpuFanIDs = selectedCurveFanIDs('cpu');
  const hddFanIDs = selectedCurveFanIDs('hdd');
  const nvmeFanIDs = selectedCurveFanIDs('nvme');
  return {
    enabled: $('fan-enabled').checked,
    device_id: cpuFanIDs[0] || hddFanIDs[0] || nvmeFanIDs[0] || '',
    cpu_fan_ids: cpuFanIDs,
    hdd_fan_ids: hddFanIDs,
    nvme_fan_ids: nvmeFanIDs,
    min_pwm_percent: Number($('fan-min').value),
    emergency_temp_c: Number($('fan-emergency').value),
    poll_seconds: Number($('fan-poll').value),
    off_mode: fanOffMode,
    curve: cpuCurve,
    hdd_curve: hddCurve,
    nvme_curve: nvmeCurve,
    hdd_slot_ids: selectedFanSlotIDs('hdd'),
    nvme_slot_ids: selectedFanSlotIDs('nvme'),
  };
}

$('save-global').addEventListener('click', async () => {
  if (!reportPanelValidity('panel-power')) return;
  const config = {
    enabled: $('enabled').checked,
    pl1_w: Number($('pl1').value),
    pl2_w: Number($('pl2').value),
    reapply_seconds: Number($('interval').value),
  };
  if (config.pl2_w < config.pl1_w) {
    showMessage('PL2 不能低于 PL1。', true, 'message-global');
    return;
  }
  setBusy(true);
  try {
    render(await request('api/config/global', { method: 'POST', body: JSON.stringify(config) }), true);
    showMessage('全局配置已保存并应用；风扇和按钮配置未修改。', false, 'message-global');
  } catch (error) {
    showMessage(`保存失败：${error.message}`, true, 'message-global');
  } finally {
    setBusy(false);
  }
});

// 保存后该亮哪一档：先按落盘的曲线反推（手动拖节点会落回「临时」），但如果是
// 刚从某个自定义按键「应用并保存」过来的、曲线还跟按键里一致，就保住那个按键的选中态。
function curveModeAfterSave(config) {
  const detected = detectCurveMode(config);
  const match = /^slot-([1-9])$/.exec(activeCurveMode || '');
  if (detected !== 'temp' || !match) return detected;
  const entry = curveSlotEntry(Number(match[1]));
  if (!entry) return detected;
  const sameCurve = (left, right) => Array.isArray(left) && Array.isArray(right)
    && left.length === right.length
    && left.every((point, index) => Math.round(Number(point.temp_c)) === Math.round(Number(right[index].temp_c))
      && Math.round(Number(point.pwm_percent)) === Math.round(Number(right[index].pwm_percent)));
  const same = CURVE_KINDS.every((kind) => sameCurve(config[CURVE_MODE_FIELDS[kind]], entry.curves[kind]));
  return same ? activeCurveMode : detected;
}

// 保存并下发风扇控制：按钮与「应用并保存风扇控制」共用同一条路
async function saveFanConfig() {
  if (!reportPanelValidity('panel-fan')) return false;
  const config = fanConfigFromInputs();
  if (config.enabled && ![...config.cpu_fan_ids, ...config.hdd_fan_ids, ...config.nvme_fan_ids].length) {
    showMessage('启用风扇曲线前，至少需要为一条温度曲线选择风扇。', true, 'message-fan');
    return false;
  }
  const curves = [
    ['HDD/SATA', config.hdd_curve],
    ['NVMe', config.nvme_curve],
    ['CPU', config.curve],
  ];
  const pwmFloor = config.off_mode ? 0 : config.min_pwm_percent;
  for (const [label, curve] of curves) {
    if (curve.length < CURVE_MIN_POINTS || curve.length > CURVE_MAX_POINTS) {
      showMessage(`${label}曲线必须包含 ${CURVE_MIN_POINTS}–${CURVE_MAX_POINTS} 个节点。`, true, 'message-fan');
      return false;
    }
    if (curve.some((point, index) => point.pwm_percent < pwmFloor
        || (index > 0 && (point.temp_c <= curve[index - 1].temp_c || point.pwm_percent < curve[index - 1].pwm_percent)))) {
      showMessage(config.off_mode
        ? `${label}曲线温度必须严格递增，转速不能随温度升高而下降。`
        : `${label}曲线温度必须严格递增，转速不能随温度升高而下降，且不能低于最低转速。`, true, 'message-fan');
      return false;
    }
    if (config.emergency_temp_c < curve[curve.length - 1].temp_c) {
      showMessage(`紧急满速温度不能低于最后一个${label}曲线节点。`, true, 'message-fan');
      return false;
    }
  }
  setBusy(true);
  try {
    render(await request('api/config/fan', { method: 'POST', body: JSON.stringify(config) }), true);
    // 按刚落盘的配置反推选中档位：手动拖过节点会落回「临时」，关停／全速／预设各自对上
    setCurveMode(curveModeAfterSave(config));
    showMessage(config.off_mode
      ? '风扇控制已保存：关停档生效，绑定风扇已停转。'
      : '风扇控制与三条温控曲线已保存并应用；其他配置未修改。', false, 'message-fan');
    return true;
  } catch (error) {
    showMessage(`保存失败：${error.message}`, true, 'message-fan');
    return false;
  } finally {
    setBusy(false);
  }
}

$('save-fan').addEventListener('click', () => { saveFanConfig(); });

$('save-gpio').addEventListener('click', async () => {
  if (!commitGPIOScriptEditor()) return;
  if (!commitBackupJobEditor()) return;
  // 自动备份编辑器开着时，里面的草稿也要先落进 gpioAutos，否则保存后这条
  // 规则凭空消失，用户还以为是保存成功了。
  if (!commitAutoBackupEditor()) return;
  const config = gpioConfigFromInputs();
  setBusy(true);
  try {
    render(await request('api/config/gpio', { method: 'POST', body: JSON.stringify(config) }), true);
    showMessage('按钮控制配置已保存；全局和风扇配置未修改。', false, 'message-gpio');
  } catch (error) {
    showMessage(`保存失败：${error.message}`, true, 'message-gpio');
  } finally {
    setBusy(false);
  }
});

// 按键触发声光反馈开关：切换后立即落盘，不必再点「保存按钮控制」，
// 否则用户以为拨了开关就生效，实际配置没存下去、按键照样滴滴滴。
function updateGPIOFeedbackStatus() {
  const target = $('gpio-feedback-status');
  if (!target) return;
  target.textContent = $('gpio-feedback').checked
    ? '开启后通过按键触发动作会滴滴滴响 3 声并让复制键 LED 闪 3 次，脚本运行结束时也会重复一次声光提示。'
    : '已关闭：按键触发动作不再蜂鸣，也不再闪灯。';
}

// 即时保存单个按键开关：只提交「服务端已确认的配置 + 本次改动的一个字段」，
// 绝不夹带页面上其他未保存的草稿（否则用户只切了提示音，却把刚改过的 enabled
// 等状态一并写了进去）。失败分两种：服务端明确拒绝时回填已确认状态；网络结果
// 不确定时回读服务端确认，读不到就明确标注「状态未知」，不能继续宣称已关闭。
function gpioFlagConfirmed(field) {
  const gpio = currentStatus?.config?.gpio || {};
  return gpio[field] !== false; // 缺省开启，与 fillGPIOInputs 的回显口径一致
}

function syncGPIOFlagInput(field) {
  if (field === 'feedback') {
    $('gpio-feedback').checked = gpioFlagConfirmed('feedback');
    updateGPIOFeedbackStatus();
    return;
  }
  $('gpio-power-chime').checked = gpioFlagConfirmed('power_chime');
  updatePowerChimeStatus();
}

async function saveGPIOFlag(field, enabled, successText) {
  const confirmed = currentStatus?.config?.gpio;
  if (!confirmed) {
    syncGPIOFlagInput(field);
    showMessage('尚未加载按键配置，请刷新页面后重试。', true, 'message-gpio');
    return;
  }
  // 从已确认配置克隆，页面上其他输入框的未保存草稿不会被顺带提交。
  const payload = JSON.parse(JSON.stringify(confirmed));
  payload[field] = enabled;
  setBusy(true);
  try {
    render(await request('api/config/gpio', { method: 'POST', body: JSON.stringify(payload) }), true);
    showMessage(successText, false, 'message-gpio');
  } catch (error) {
    if (error?.name === 'TypeError') {
      // 连接中断/超时：服务端可能已经保存，不能自行宣称成功或失败，回读确认。
      try {
        render(await request('api/status'), true);
        syncGPIOFlagInput(field);
        showMessage('请求结果不确定，已按服务端状态刷新。', true, 'message-gpio');
      } catch (refreshError) {
        const target = $(field === 'feedback' ? 'gpio-feedback-status' : 'gpio-power-chime-status');
        if (target) {
          target.textContent = '状态未知：保存请求结果不确定，请刷新页面确认实际状态。';
        }
        showMessage(`保存结果未知：${error.message}`, true, 'message-gpio');
      }
    } else {
      // 服务端明确拒绝：把开关拨回已确认的状态，别让界面停留在未生效的草稿上。
      syncGPIOFlagInput(field);
      showMessage(`保存失败：${error.message}`, true, 'message-gpio');
    }
  } finally {
    setBusy(false);
  }
}

$('gpio-feedback').addEventListener('change', async () => {
  const enabled = $('gpio-feedback').checked;
  updateGPIOFeedbackStatus();
  await saveGPIOFlag('feedback', enabled, enabled ? '按键声光反馈已开启。' : '按键声光反馈已关闭。');
});

// 开机/关机提示音开关：与按键反馈一样切换即落盘，不必再点「保存按钮控制」。
const POWER_CHIME_HELP = '开启后开机进入系统、网页可访问时滴一声，触发软关机时再滴一声。';

function updatePowerChimeStatus() {
  const target = $('gpio-power-chime-status');
  if (!target) return;
  target.textContent = $('gpio-power-chime').checked
    ? POWER_CHIME_HELP
    : '已关闭：开机就绪与软关机时都不再蜂鸣。';
}

$('gpio-power-chime').addEventListener('change', async () => {
  const enabled = $('gpio-power-chime').checked;
  updatePowerChimeStatus();
  await saveGPIOFlag('power_chime', enabled, enabled ? '开机与关机提示音已开启。' : '开机与关机提示音已关闭。');
});

// 关停档报警临时静音：切换即调用专用接口（不落盘，退出关停自动解除）。
$('fan-alarm-mute').addEventListener('change', async () => {
  const muted = $('fan-alarm-mute').checked;
  setBusy(true);
  try {
    render(await request('api/fan/alarm-mute', { method: 'POST', body: JSON.stringify({ muted }) }), true);
    showMessage(muted
      ? '已临时静音关停档的故障/超温蜂鸣报警；界面报警与日志仍保留。'
      : '已恢复关停档的故障/超温蜂鸣报警。', false, 'message-fan');
  } catch (error) {
    showMessage(`静音失败：${error.message}`, true, 'message-fan');
    syncFanAlarmMute(currentStatus?.fan_control || {}); // 失败时把开关拨回真实状态
  } finally {
    setBusy(false);
  }
});

$('apply-now').addEventListener('click', async () => {
  setBusy(true);
  try {
    render(await request('api/apply', { method: 'POST', body: '{}' }), true);
    showMessage('已重新应用当前功耗与风扇配置。');
  } catch (error) {
    showMessage(`重应用失败：${error.message}`, true);
  } finally {
    setBusy(false);
  }
});

$('restore').addEventListener('click', async () => {
  const ok = await showAppConfirm({
    title: '关闭自动控制',
    message: '确定关闭自动控制，并恢复首次捕获的功耗限制与 BIOS 风扇模式吗？',
    confirmText: '关闭并恢复',
    danger: true,
  });
  if (!ok) return;
  setBusy(true);
  try {
    render(await request('api/restore', { method: 'POST', body: '{}' }));
    showMessage('已关闭自动控制，并恢复原始功耗与风扇配置。');
  } catch (error) {
    showMessage(`恢复失败：${error.message}`, true);
  } finally {
    setBusy(false);
  }
});

function setupCurveEditor(kind) {
  const editor = curveEditors[kind];
  const curveChart = $(editor.chartID);
  curveChart.addEventListener('pointerdown', (event) => {
    if (uiBusy) return;
    const node = event.target.closest?.('.curve-node');
    if (!node) return;
    editor.selectedIndex = Number(node.dataset.index);
    editor.draggedIndex = editor.selectedIndex;
    curveChart.classList.add('dragging');
    curveChart.setPointerCapture(event.pointerId);
    event.preventDefault();
    updateCurveFromPointer(kind, event);
  });
  curveChart.addEventListener('pointermove', (event) => {
    if (editor.draggedIndex < 0) return;
    event.preventDefault();
    updateCurveFromPointer(kind, event);
  });
  const finishCurveDrag = (event, update = true) => {
    if (editor.draggedIndex < 0) return;
    if (update) updateCurveFromPointer(kind, event);
    editor.draggedIndex = -1;
    curveChart.classList.remove('dragging');
    if (curveChart.hasPointerCapture(event.pointerId)) curveChart.releasePointerCapture(event.pointerId);
  };
  curveChart.addEventListener('pointerup', finishCurveDrag);
  curveChart.addEventListener('pointercancel', (event) => finishCurveDrag(event, false));
  curveChart.addEventListener('lostpointercapture', () => {
    editor.draggedIndex = -1;
    curveChart.classList.remove('dragging');
  });
  curveChart.addEventListener('keydown', (event) => {
    const node = event.target.closest?.('.curve-node');
    if (node) editor.selectedIndex = Number(node.dataset.index);
    const point = editor.curve[editor.selectedIndex];
    if (!point) return;
    let temperature = point.temp_c;
    let pwm = point.pwm_percent;
    if (event.key === 'ArrowLeft') temperature -= 1;
    else if (event.key === 'ArrowRight') temperature += 1;
    else if (event.key === 'ArrowDown') pwm -= 1;
    else if (event.key === 'ArrowUp') pwm += 1;
    else if (event.key === 'Delete' || event.key === 'Backspace') {
      event.preventDefault();
      removeSelectedCurvePoint(kind);
      return;
    } else return;
    event.preventDefault();
    setCurvePoint(kind, editor.selectedIndex, temperature, pwm);
    renderFanChart(kind);
    requestAnimationFrame(() => curveChart.querySelector(`.curve-node-control[data-index="${editor.selectedIndex}"]`)?.focus());
  });
  $(editor.addID).addEventListener('click', () => addCurvePoint(kind));
  $(editor.removeID).addEventListener('click', () => removeSelectedCurvePoint(kind));
}
CURVE_KINDS.forEach(setupCurveEditor);

// ---- 曲线编辑器标签页：三张卡一次只显示一张，收纵向空间 ----
// 切换后必须重绘：curveChartScale 按 clientWidth 测量，隐藏期间为 0，
// 缩放与图内文字会失真；显示后按真实尺寸重算。上次停留的标签记 localStorage
// （页面内布局记忆，无需进服务端 ui_prefs）。
let activeCurveKind = loadActiveCurveKind();

function loadActiveCurveKind() {
  try {
    const saved = window.localStorage.getItem('tad-curve-tab');
    return CURVE_KINDS.includes(saved) ? saved : CURVE_KINDS[0];
  } catch (error) { return CURVE_KINDS[0]; }
}

function applyCurveTab(kind) {
  activeCurveKind = CURVE_KINDS.includes(kind) ? kind : CURVE_KINDS[0];
  CURVE_KINDS.forEach((candidate) => {
    const panel = document.querySelector(`.curve-editor[data-curve-kind="${candidate}"]`);
    if (panel) panel.hidden = candidate !== activeCurveKind;
    const tab = $(`curve-tab-${candidate}`);
    if (tab) {
      tab.classList.toggle('active', candidate === activeCurveKind);
      tab.setAttribute('aria-selected', String(candidate === activeCurveKind));
    }
  });
  try { window.localStorage.setItem('tad-curve-tab', activeCurveKind); } catch (error) { /* 隐私模式等场景仅本次生效 */ }
  renderFanChart(activeCurveKind);
}

function setupCurveTabs() {
  CURVE_KINDS.forEach((kind) => {
    const tab = $(`curve-tab-${kind}`);
    if (tab) tab.addEventListener('click', () => applyCurveTab(kind));
  });
  applyCurveTab(activeCurveKind);
}
setupCurveTabs();
setupCurvePresets();
function setupCurveChartScaling() {
  const charts = [
    ...CURVE_KINDS.map((kind) => $(curveEditors[kind].chartID)),
    $('history-chart'),
  ].filter(Boolean);
  const update = () => {
    CURVE_KINDS.forEach((kind) => renderFanChart(kind));
    renderHistoryChart();
  };
  if (typeof ResizeObserver === 'function') {
    const observer = new ResizeObserver(update);
    charts.forEach((chart) => observer.observe(chart));
  } else {
    window.addEventListener('resize', update, { passive: true });
  }
  update();
}
setupCurveChartScaling();
$('fan-min').addEventListener('input', () => {
  if ($('fan-min').value !== '') CURVE_KINDS.forEach(normalizeCurveToControls);
});
$('fan-emergency').addEventListener('input', () => {
  if ($('fan-emergency').value !== '') CURVE_KINDS.forEach(normalizeCurveToControls);
});
$('config-form').addEventListener('invalid', (event) => {
  const panel = event.target.closest?.('.tab-panel');
  const tabID = panel?.getAttribute('aria-labelledby');
  if (tabID) activateTab(tabID);
}, true);
setupTabs();
// 固件升级 / 压力测试
$('firmware-fpt-upload').addEventListener('click', uploadFPT);
$('firmware-bios-flash').addEventListener('click', flashBIOS);
$('firmware-bios-manual-toggle').addEventListener('change', () => {
  setManualBIOSVisible($('firmware-bios-manual-toggle').checked);
});
$('firmware-nic-check').addEventListener('click', checkNIC);
$('firmware-nic-flash').addEventListener('click', () => flashNIC(false));
$('firmware-nic-flash-force').addEventListener('click', () => flashNIC(true));
$('firmware-nic-mft-install').addEventListener('click', installMFTTool);
$('bios-online-check').addEventListener('click', checkBIOSOnline);
$('bios-online-flash').addEventListener('click', flashBIOSOnline);
// 开关机计划：工具栏 + 编辑器
$('schedule-add').addEventListener('click', () => openScheduleEditor(-1));
$('schedule-edit').addEventListener('click', () => openScheduleEditor(powerScheduleSelected));
$('schedule-delete').addEventListener('click', deletePowerSchedule);
$('schedule-enable').addEventListener('click', () => setPowerScheduleEnabled(true));
$('schedule-disable').addEventListener('click', () => setPowerScheduleEnabled(false));
$('schedule-apply').addEventListener('click', applyPowerSchedule);
$('schedule-test').addEventListener('click', testPowerSchedule);
$('schedule-editor-ok').addEventListener('click', submitScheduleEditor);
$('schedule-editor-cancel').addEventListener('click', closeScheduleEditor);
document.querySelectorAll('[data-schedule-close]').forEach((node) => {
  node.addEventListener('click', closeScheduleEditor);
});
$('stress-start').addEventListener('click', startStressRun);
$('stress-stop').addEventListener('click', stopStressRun);
$('stress-memory-auto').addEventListener('change', updateStressMemoryAuto);
$('stress-off-restore').addEventListener('click', () => { restoreFanOffState(); });
$('stress-permanent').addEventListener('change', () => {
  const duration = $('stress-duration');
  if (!duration) return;
  duration.disabled = $('stress-permanent').checked || $('stress-start').disabled;
});
setupHistoryPanel();
setupHealthTooltip();
setupAppModal();
setupFanSlotSelectors();
setupGPIOActions();
document.querySelectorAll('input[name="power-mode"]').forEach((input) => {
  input.addEventListener('change', () => updatePowerMode(input.value, true));
});
$('check-updates').addEventListener('click', checkForUpdates);
$('debug-generate-report').addEventListener('click', generateDebugReport);
$('debug-copy-report').addEventListener('click', copyDebugReport);
$('debug-download-report').addEventListener('click', downloadDebugReport);
$('debug-open-issue').addEventListener('click', openDebugIssue);
setupHistoryExport();
setupSensorNames();
setupSerialSensor();
$('gpio-enabled').addEventListener('change', updateGPIOEnabledState);
$('gpio-refresh-sdcard').addEventListener('click', () => refreshSDCardManually());
$('storage-refresh-sdcard').addEventListener('click', () => refreshSDCardManually($('storage-refresh-sdcard'), $('storage-mobile-status')));
$('gpio-script-add').addEventListener('click', () => openGPIOScriptEditor());
$('gpio-script-cancel').addEventListener('click', closeGPIOScriptEditor);
$('gpio-script-commit').addEventListener('click', commitGPIOScriptEditor);
$('gpio-script-body').addEventListener('input', renderBashHighlight);
$('gpio-script-body').addEventListener('scroll', syncBashEditorScroll);
$('gpio-script-body').addEventListener('keydown', (event) => {
  if (event.key !== 'Tab') return;
  event.preventDefault();
  const textarea = event.currentTarget;
  const start = textarea.selectionStart;
  textarea.setRangeText('  ', start, textarea.selectionEnd, 'end');
  renderBashHighlight();
});
document.querySelectorAll('.gpio-action').forEach((select) => {
  select.addEventListener('change', () => { renderGPIOScriptList(); renderBackupJobList(); renderAutoBackupList(); });
});

// 备份任务：新增/编辑/删除、路径与卷选择、立即运行。
$('backup-job-add').addEventListener('click', () => openBackupJobEditor());
$('backup-job-cancel').addEventListener('click', closeBackupJobEditor);
$('backup-job-commit').addEventListener('click', commitBackupJobEditor);
$('backup-job-test').addEventListener('click', () => {
  const job = backupJobFromEditor();
  if (!validateBackupJobDraft(job)) return;
  runBackupJob(job.id, true, job);
});
$('backup-job-marker-write').addEventListener('change', onBackupMarkerToggleChange);
['source', 'target'].forEach((role) => {
  $(`backup-job-${role}-pick`).addEventListener('click', () => openPathPicker(role));
  $(`backup-job-${role}-volume`).addEventListener('click', () => openVolumePicker(role));
  $(`backup-job-${role}-clear`).addEventListener('click', () => setBackupEditorLocation(role, null));
});
document.querySelectorAll('input[name="backup-mode"]').forEach((radio) => {
  radio.addEventListener('change', updateBackupModeControls);
});
// 自动备份：打标管理、规则编辑器、试跑与立即运行。
$('auto-backup-add').addEventListener('click', () => openAutoBackupEditor());
$('auto-backup-cancel').addEventListener('click', closeAutoBackupEditor);
$('auto-backup-commit').addEventListener('click', commitAutoBackupEditor);
$('auto-backup-test').addEventListener('click', () => {
  const rule = autoRuleFromEditor();
  if (!validateAutoRuleDraft(rule)) return;
  runAutoBackup(rule.id, true, rule);
});
$('auto-backup-fixed-pick').addEventListener('click', () => openPathPicker('fixed'));
$('auto-backup-fixed-clear').addEventListener('click', () => setAutoBackupFixedLocation(null));
document.querySelectorAll('input[name="auto-backup-direction"]').forEach((radio) => {
  radio.addEventListener('change', updateAutoBackupModeControls);
});
document.querySelectorAll('input[name="auto-backup-mode"]').forEach((radio) => {
  radio.addEventListener('change', updateAutoBackupModeControls);
});
$('auto-backup-sub').addEventListener('input', updateAutoBackupModeControls);
// 勾上确认项就撤掉告警高亮（抖动只负责把人引过来，勾没勾以勾选框为准）。
$('auto-backup-allow-move').addEventListener('change', () => {
  if ($('auto-backup-allow-move').checked) setAutoMoveConfirmAlert(false);
});
$('auto-backup-marker-refresh').addEventListener('click', refreshAutoBackupDevices);
$('auto-backup-marker-pick').addEventListener('click', () => {
  // 从外接存储的位置进入，避免用户在系统目录里乱选；打标认定这块盘。
  openPathPicker('marker');
});
$('auto-backup-sub-pick').addEventListener('click', () => {
  // 子目录打标：选中的目录成为这块盘的操作范围（可选多个）。
  openPathPicker('submark');
});
$('path-picker-close').addEventListener('click', closePathPicker);
$('path-picker-up').addEventListener('click', (event) => {
  const parent = event.currentTarget.dataset.parent;
  // parent 为空表示已经到位置根目录，再上一级就是位置总览。
  if (parent) browsePathTo(parent, pathPicker.scope);
  else browsePathTo('', 'places');
});
$('path-picker-mkdir').addEventListener('click', promptCreateDirectory);
$('path-picker-advanced').addEventListener('click', togglePathPickerAdvanced);
$('path-picker-select').addEventListener('click', confirmPathPicker);
document.querySelectorAll('[data-picker-close]').forEach((node) => {
  node.addEventListener('click', closePathPicker);
});
$('volume-picker-close').addEventListener('click', closeVolumePicker);
$('volume-picker-refresh').addEventListener('click', loadVolumeList);
$('volume-picker-confirm').addEventListener('click', confirmVolumePicker);
document.querySelectorAll('[data-volume-close]').forEach((node) => {
  node.addEventListener('click', closeVolumePicker);
});
document.addEventListener('keydown', (event) => {
  if (event.key !== 'Escape') return;
  if (!$('path-picker').hidden) closePathPicker();
  else if (!$('volume-picker').hidden) closeVolumePicker();
});

const THEME_STORAGE_KEY = 'tad-theme';
const THEME_ORDER = ['auto', 'light', 'dark'];
const THEME_META = {
  auto: { label: '主题：跟随 fnOS/系统', icon: '🌗' },
  light: { label: '主题：浅色', icon: '☀️' },
  dark: { label: '主题：深色', icon: '🌙' },
};

function currentThemeMode() {
  try {
    const saved = window.localStorage.getItem(THEME_STORAGE_KEY);
    return THEME_ORDER.includes(saved) ? saved : 'auto';
  } catch (error) {
    return 'auto';
  }
}

// fnOS 没有公开的插件主题接口；插件页与 fnOS 桌面同源内嵌时，读取父窗口
// 明暗实现自动跟随。fnOS 实测（1.2.0505）：桌面主题挂在 <body theme-mode=
// "dark|light"> 属性上（Semi Design 的暗色选择器即 body[theme-mode=dark]），
// <html> 的 class="light" 是引导脚本默认值、从不随主题变；body 无该属性时
// 按根元素背景亮度兜底。跨域或无父窗口时返回 null，退回 prefers-color-scheme。
function detectParentTheme() {
  try {
    if (window.parent === window || !window.parent.document) return null;
    const parent = window.parent;
    const root = parent.document.documentElement;
    const body = parent.document.body;
    const bodyMode = (body.getAttribute('theme-mode') || '').trim().toLowerCase();
    if (bodyMode === 'dark') return 'dark';
    if (bodyMode === 'light') return 'light';
    const themeAttr = `${root.getAttribute('data-theme') || ''} ${root.getAttribute('data-bs-theme') || ''}`;
    const className = `${root.className || ''} ${body.className || ''}`;
    const tokens = `${themeAttr} ${className}`;
    if (/(^|\s)(dark|night)(\s|$)/i.test(tokens)) return 'dark';
    if (/(^|\s)light(\s|$)/i.test(tokens)) return 'light';
    const background = parent.getComputedStyle(root).backgroundColor;
    const rgb = background.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)(?:,\s*([\d.]+))?\)/);
    if (rgb && (rgb[4] === undefined || Number(rgb[4]) > 0.5)) {
      const luminance = 0.2126 * Number(rgb[1]) + 0.7152 * Number(rgb[2]) + 0.0722 * Number(rgb[3]);
      if (luminance < 90) return 'dark';
      if (luminance > 150) return 'light';
    }
  } catch (error) {
    return null; // 父窗口跨域不可读
  }
  return null;
}

function resolveAutoTheme() {
  return detectParentTheme() || (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
}

// theme-color 与 styles.css 两套 --bg 同值（#f3f4f6 / #141519）。fnOS 桌面
// 的窗口标题栏由宿主绘制，插件无法直接改色；meta 供手机浏览器地址栏等
// 外壳取色，切主题时同步，避免深色页面顶着浅色外壳条。
function syncThemeColorMeta(theme) {
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', theme === 'dark' ? '#141519' : '#f3f4f6');
}

function applyTheme(mode) {
  if (typeof window === 'undefined' || !window.matchMedia) return;
  const theme = mode === 'auto' ? resolveAutoTheme() : mode;
  document.documentElement.dataset.theme = theme;
  syncThemeColorMeta(theme);
  const meta = THEME_META[mode];
  const button = $('theme-toggle');
  if (button) {
    button.textContent = meta.icon;
    button.title = meta.label;
    button.setAttribute('aria-label', meta.label);
  }
}

// 仅跟随档需要监听 fnOS 桌面主题变化；观察器注册在父窗口节点上，不随
// 本 iframe 销毁回收，固定浅/深档或页面卸载时必须显式断开，否则会在
// fnOS 桌面上累积观察器并在父窗口属性变动时对已销毁文档反复取主题。
let parentThemeObserver = null;

function syncParentThemeObserver() {
  if (parentThemeObserver) {
    parentThemeObserver.disconnect();
    parentThemeObserver = null;
  }
  if (currentThemeMode() !== 'auto') return;
  try {
    if (window.parent === window || !window.parent.MutationObserver) return;
    const observer = new window.parent.MutationObserver(() => applyTheme(currentThemeMode()));
    observer.observe(window.parent.document.documentElement, {
      attributes: true, attributeFilter: ['class', 'data-theme', 'data-bs-theme', 'style'],
    });
    observer.observe(window.parent.document.body, {
      attributes: true, attributeFilter: ['class', 'theme-mode', 'style'],
    });
    parentThemeObserver = observer;
  } catch (error) { /* 父窗口不可读时仅跟随浏览器偏好 */ }
}

applyTheme(currentThemeMode());
syncParentThemeObserver();
window.addEventListener('pagehide', () => {
  if (parentThemeObserver) {
    parentThemeObserver.disconnect();
    parentThemeObserver = null;
  }
});
// bfcache 恢复不会重新执行脚本，而 pagehide 已断开观察器：pageshow 时重挂，
// 并重取一次主题（恢复期间 fnOS 桌面可能已切换明暗）。
window.addEventListener('pageshow', () => {
  applyTheme(currentThemeMode());
  syncParentThemeObserver();
});
const themeToggle = $('theme-toggle');
if (themeToggle) {
  themeToggle.addEventListener('click', () => {
    const next = THEME_ORDER[(THEME_ORDER.indexOf(currentThemeMode()) + 1) % THEME_ORDER.length];
    try { window.localStorage.setItem(THEME_STORAGE_KEY, next); } catch (error) { /* 隐私模式等场景下仅本次生效 */ }
    applyTheme(next);
    syncParentThemeObserver();
  });
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => applyTheme(currentThemeMode()));
}

CURVE_KINDS.forEach(renderFanChart);
refresh();
setInterval(() => refresh(true), 5000);

// 动态隐藏滚动条：悬停淡入由 CSS 负责，这里补「正在滚动」这一路——
// 滚轮/触屏滚动时滑块短暂露出，停下 0.7s 后淡出。滚动事件不冒泡，
// 挂捕获阶段，弹窗里的存储位置／目录列表也能收到。
let scrollbarTimer = null;
window.addEventListener('scroll', () => {
  const root = document.documentElement;
  if (!root || !root.classList) return;
  root.classList.add('scr-on');
  if (scrollbarTimer) clearTimeout(scrollbarTimer);
  scrollbarTimer = setTimeout(() => root.classList.remove('scr-on'), 700);
}, { passive: true, capture: true });
