'use strict';

// 回归测试（node:test，无第三方依赖）：通过 vm 在最小 DOM/定时器桩中加载
// 真实的 app.js，对纯逻辑函数做断言。运行：node --test app/ui/static/

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function stubElement() {
  const store = { checked: false, value: '', textContent: '' };
  return new Proxy(function stub() {}, {
    get(_target, prop) {
      if (prop === Symbol.toPrimitive || prop === 'toString') return () => '';
      if (prop in store) return store[prop];
      return stubElement();
    },
    set(_target, prop, value) {
      store[prop] = value;
      return true;
    },
    apply() {
      return stubElement();
    },
  });
}

// 可记录事件监听的元素桩：value/checked 按元素持久保存，供交互用例读取与触发
function listeningStub() {
  const store = { checked: false, value: '', textContent: '' };
  const listeners = new Map();
  const stub = new Proxy(function stub() {}, {
    get(_target, prop) {
      if (prop === Symbol.toPrimitive || prop === 'toString') return () => '';
      if (prop === 'addEventListener') return (type, handler) => { listeners.set(type, handler); };
      if (prop in store) return store[prop];
      return stubElement();
    },
    set(_target, prop, value) {
      store[prop] = value;
      return true;
    },
    apply() {
      return stubElement();
    },
  });
  stub.listeners = listeners;
  return stub;
}

function loadAppContext(overrides = {}) {
  const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const matchMedia = () => ({ matches: false, addEventListener() {} });
  // getElementById 按 id 复用同一元素桩（与真实 DOM 一致），交互用例才能读回输入值
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, listeningStub());
    return elements.get(id);
  };
  const sandbox = {
    console,
    URL,
    URLSearchParams,
    setInterval: () => 0,
    clearInterval: () => {},
    setTimeout: () => 0,
    clearTimeout: () => {},
    requestAnimationFrame: overrides.requestAnimationFrame || (() => 0),
    fetch: overrides.fetch || (() => new Promise(() => {})),
    Image: function Image() {},
    ResizeObserver: class ResizeObserver {
      observe() {}
      disconnect() {}
      unobserve() {}
    },
    navigator: { userAgent: 'node-test' },
    location: { reload() {}, href: '', pathname: '/', origin: 'http://localhost' },
    document: {
      readyState: 'complete',
      getElementById: (id) => element(id),
      createElement: () => stubElement(),
      createElementNS: () => stubElement(),
      createTextNode: () => stubElement(),
      querySelector: () => stubElement(),
      querySelectorAll: () => [],
      addEventListener() {},
      body: stubElement(),
      documentElement: stubElement(),
    },
  };
  sandbox.window = {
    matchMedia,
    addEventListener() {},
    innerWidth: 1280,
    innerHeight: 800,
    confirm: overrides.confirm || (() => true),
    location: sandbox.location,
  };
  sandbox.globalThis = sandbox;
  const context = vm.createContext(sandbox);
  vm.runInContext(source, context, { filename: 'app.js' });
  return {
    resolve: (name) => vm.runInContext(name, context),
    element,
    click: (id, type = 'click') => {
      const handler = element(id).listeners.get(type);
      return handler ? handler({ preventDefault() {}, stopPropagation() {} }) : undefined;
    },
  };
}

// 记录型 fetch 桩：按 URL 返回可配置响应体，供交互用例断言实际发出的请求
function recordingFetch(responseFor) {
  const requests = [];
  const fetch = async (url, init = {}) => {
    const href = String(url);
    requests.push({ url: href, init });
    const payload = responseFor(href, init);
    // 返回体里带布尔 ok 字段时按它模拟 HTTP 失败（request 会 throw）
    const ok = payload && typeof payload === 'object' && typeof payload.ok === 'boolean' ? payload.ok : true;
    return { ok, status: ok ? 200 : 500, json: async () => payload };
  };
  return { requests, fetch };
}

const { resolve } = loadAppContext();

test('盘位覆盖层标签：empty/present/used/warning/unknown 各状态文案统一', () => {
  const storageSlotStatusLabel = resolve('storageSlotStatusLabel');
  assert.equal(storageSlotStatusLabel({ state: 'empty' }, 'empty'), '空置');
  assert.equal(storageSlotStatusLabel({ state: 'present' }), '未使用');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'idle' }), '空闲');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'light' }), '轻载');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'medium' }), '中载');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'heavy' }), '高载');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'busy' }), '繁忙');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'full' }), '满载');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'sleeping' }), '休眠');
  assert.equal(storageSlotStatusLabel({ state: 'warning' }), '告警');
  assert.equal(storageSlotStatusLabel({ state: 'unknown' }), '未知');
});

test('盘位覆盖层标签：used 无活动读数显示"已使用"，不得伪装成 SMART 健康', () => {
  const storageSlotStatusLabel = resolve('storageSlotStatusLabel');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'unknown' }), '未知');
  assert.equal(storageSlotStatusLabel({ state: 'used' }), '已使用');
});

test('盘位覆盖层标签：高温优先于活动状态，前置与 M.2 阈值各自生效', () => {
  const storageSlotStatusLabel = resolve('storageSlotStatusLabel');
  const isStorageHot = resolve('isStorageHot');
  assert.equal(storageSlotStatusLabel({ state: 'used', kind: 'front', temperature_c: 56, activity: 'idle' }), '高温');
  assert.equal(storageSlotStatusLabel({ state: 'used', kind: 'm2', temperature_c: 71, activity: 'idle' }), '高温');
  assert.equal(isStorageHot({ kind: 'front', temperature_c: 55 }), true);
  assert.equal(isStorageHot({ kind: 'm2', temperature_c: 70 }), true);
  assert.equal(isStorageHot({ kind: 'front', temperature_c: 54.9 }), false);
  assert.equal(isStorageHot({ kind: 'm2', temperature_c: 69.9 }), false);
});

test('风扇 1200→0→1200：选择器签名不变，未保存的勾选不触发重建', () => {
  const fanListSignature = resolve('fanListSignature');
  const fans = (rpm) => [
    { id: 'fan0', channel: 0, rpm },
    { id: 'fan1', channel: 1, rpm },
  ];
  const baseline = fanListSignature(fans(1200));
  assert.equal(fanListSignature(fans(0)), baseline);
  assert.equal(fanListSignature(fans(1200)), baseline);
  assert.equal(baseline, 'fan0|fan1');
  assert.notEqual(fanListSignature([{ id: 'fan0' }, { id: 'fan2' }]), baseline);
});

test('已勾选风扇 1200→0→1200 仍可见；未勾选的 0 RPM / 负值通道隐藏', () => {
  const connectedFans = resolve('connectedFans');
  const fans = [
    { id: 'fan0', channel: 0, rpm: 1200 },
    { id: 'fan1', channel: 1, rpm: 1200, selected: true },
    { id: 'fan2', channel: 2, rpm: 0 },
    { id: 'fan3', channel: 3, rpm: -1 },
  ];
  assert.deepEqual(connectedFans({ fans }).map((fan) => fan.id), ['fan0', 'fan1']);
  fans[1].rpm = 0;
  assert.deepEqual(connectedFans({ fans }).map((fan) => fan.id), ['fan0', 'fan1']);
  fans[1].rpm = 1200;
  assert.deepEqual(connectedFans({ fans }).map((fan) => fan.id), ['fan0', 'fan1']);
  assert.equal(connectedFans({ fans: [{ id: 'fan1', rpm: 0 }] }).length, 0);
  assert.equal(connectedFans({ fans: [{ id: 'fan1', rpm: 0, selected: true }] }).length, 1);
});

// ---- 历史温度纯函数 ----

test('历史范围过滤：只保留 now-range 之后的采样', () => {
  const historyFilterRange = resolve('historyFilterRange');
  const nowTs = 1700003600;
  const samples = [
    { ts: nowTs - 90000 },
    { ts: nowTs - 3600 },
    { ts: nowTs - 60 },
    { ts: nowTs },
  ];
  const ranged = historyFilterRange(samples, 1, nowTs); // 1 小时
  assert.deepEqual([...ranged.map((s) => s.ts)], [nowTs - 3600, nowTs - 60, nowTs]);
});

test('历史抽稀：超出上限按步长取样且保留最后一个点', () => {
  const historyThinOut = resolve('historyThinOut');
  const samples = Array.from({ length: 1000 }, (_, i) => ({ ts: i }));
  const thinned = historyThinOut(samples, 100);
  assert.ok(thinned.length <= 101);
  assert.equal(thinned[0].ts, 0);
  assert.equal(thinned[thinned.length - 1].ts, 999);
  assert.ok(historyThinOut(samples, 2000).length === 1000, '不超上限时原样返回');
});

test('历史断口：相邻点间隔超过 2.5 倍采样周期时分段', () => {
  const historySplitSegments = resolve('historySplitSegments');
  const samples = [
    { ts: 0 }, { ts: 60 }, { ts: 120 },
    { ts: 1200 }, // 缺口 1080s > 60*2.5
    { ts: 1260 },
  ];
  const segments = historySplitSegments(samples, 60);
  assert.deepEqual([...segments.map((segment) => segment.length)], [3, 2]);
  assert.equal(historySplitSegments([{ ts: 0 }, { ts: 150 }], 60).length, 1, '2.5 倍以内不断开');
});

test('历史数值刻度：步长取 1/2/5 档，覆盖数据范围', () => {
  const historyNiceTicks = resolve('historyNiceTicks');
  assert.deepEqual([...historyNiceTicks(20, 90, 5)], [20, 40, 60, 80]);
  assert.deepEqual([...historyNiceTicks(0, 2000, 4)], [0, 500, 1000, 1500, 2000]);
  assert.equal(historyNiceTicks(5, 5, 4).length, 0, '范围为空时无刻度');
});

test('历史温度轴范围：下界放 5 度并按 5 度取整（最低 35 → 从 30 起）', () => {
  const historyYBounds = resolve('historyYBounds');
  const fallback = historyYBounds([], 20, 90);
  assert.equal(fallback.lo, 20);
  assert.equal(fallback.hi, 90);
  const bounds = historyYBounds([35, 48, 62], 20, 90);
  assert.equal(bounds.lo, 30, '最低 35 度时下界应为 30');
  assert.ok(bounds.hi >= 64, `上界应留余量: ${bounds.hi}`);
  const zeroIgnored = historyYBounds([0, 0, 55], 20, 90);
  assert.ok(zeroIgnored.lo <= 50, '0 值（不可用）不参与范围计算');
  const cold = historyYBounds([38], 20, 90);
  assert.equal(cold.lo, 30);
});

test('历史时间刻度：各档位固定步长（30 分钟→1 分钟 … 30 天→1 天）', () => {
  const historyTimeTicks = resolve('historyTimeTicks');
  const start = 1700000000;
  const halfHour = historyTimeTicks(start, start + 1800, 0.5);
  assert.ok(halfHour.length >= 29 && halfHour.length <= 31, `30min ticks ${halfHour.length}`);
  assert.ok(halfHour.every((ts) => ts % 60 === 0), '30 分钟档刻度应对齐分钟');
  const dayTicks = historyTimeTicks(start, start + 86400, 24);
  assert.ok(dayTicks.length >= 11 && dayTicks.length <= 13, `24h ticks ${dayTicks.length}`);
  assert.ok(dayTicks.every((ts) => ts % 7200 === 0), '24h 范围刻度应为 2 小时步长');
  const weekEnd = start + 7 * 86400;
  const weekTicks = historyTimeTicks(start, weekEnd, 168);
  assert.ok(weekTicks.length >= 27 && weekTicks.length <= 29, `7d ticks ${weekTicks.length}`);
  assert.ok(weekTicks.every((ts) => ts % 21600 === 0), '7 天范围刻度应为 6 小时步长');
  const monthEnd = start + 30 * 86400;
  const monthTicks = historyTimeTicks(start, monthEnd, 720);
  assert.ok(monthTicks.length >= 29 && monthTicks.length <= 31, `month ticks ${monthTicks.length}`);
  assert.ok(monthTicks.every((ts) => ts % 86400 === 0), '30 天范围刻度应为 1 天步长');
});

test('历史范围过滤与断口分段配合：先按原始间隔切段，再抽稀', () => {
  const historySplitSegments = resolve('historySplitSegments');
  const historyThinOut = resolve('historyThinOut');
  // 30 个连续点 + 10 分钟停机断口 + 30 个连续点
  const samples = [];
  for (let i = 0; i < 30; i++) samples.push({ ts: i * 60, cpu_c: 50 });
  for (let i = 0; i < 30; i++) samples.push({ ts: 3000 + i * 60, cpu_c: 55 });
  const segments = historySplitSegments(samples, 60);
  assert.deepEqual([...segments.map((segment) => segment.length)], [30, 30]);
  const thinned = segments.map((segment) => historyThinOut(segment, 10));
  assert.ok(thinned.every((segment) => segment.length <= 11), '每段抽稀后不超上限且保留末点');
});

test('历史时间格式：本地 HH:MM 两位补零', () => {
  const historyFormatClock = resolve('historyFormatClock');
  const ts = new Date(2026, 8, 15, 9, 5).getTime() / 1000;
  assert.equal(historyFormatClock(ts), '09:05');
});

test('历史范围标签：分钟 / 小时+分钟 / 整小时 / 天数文案', () => {
  const historyRangeLabel = resolve('historyRangeLabel');
  assert.equal(historyRangeLabel(0.5), '30 分钟');
  assert.equal(historyRangeLabel(1), '1 小时');
  assert.equal(historyRangeLabel(1.5), '1 小时 30 分');
  assert.equal(historyRangeLabel(1.75), '1 小时 45 分');
  assert.equal(historyRangeLabel(2), '2 小时');
  assert.equal(historyRangeLabel(6), '6 小时');
  assert.equal(historyRangeLabel(24), '24 小时');
  assert.equal(historyRangeLabel(168), '7 天');
  assert.equal(historyRangeLabel(720), '30 天');
});

test('历史范围钳制：无极区 1 分钟粒度，>2h 吸附最近挡位，7/30 天保留，非法值回默认', () => {
  const normalizeHistoryRangeHours = resolve('normalizeHistoryRangeHours');
  // 无极区：1 分钟粒度吸附
  assert.equal(normalizeHistoryRangeHours(0.51), 0.5 + 1 / 60, '1 分钟内吸附到整分钟');
  assert.equal(normalizeHistoryRangeHours(1.5), 1.5, '90 分钟正好整分');
  assert.equal(normalizeHistoryRangeHours(0.1), 0.5);
  assert.equal(normalizeHistoryRangeHours(2.05), 6, '略超 2h 即属挡位区,吸附最近挡 6h');
  // >2h 吸附最近挡位（全套挡位，与显隐无关）
  assert.equal(normalizeHistoryRangeHours(2.5), 6, '2–6h 之间吸附到 6h 挡');
  assert.equal(normalizeHistoryRangeHours(4), 6);
  assert.equal(normalizeHistoryRangeHours(30), 24, '24–168h 之间吸附到 24h 挡');
  assert.equal(normalizeHistoryRangeHours(200), 168, '7–30 天之间吸附到 7 天挡');
  assert.equal(normalizeHistoryRangeHours(-5), 0.5);
  assert.equal(normalizeHistoryRangeHours(168), 168, '7 天挡原样保留');
  assert.equal(normalizeHistoryRangeHours(720), 720, '30 天挡原样保留');
  assert.equal(normalizeHistoryRangeHours(NaN), 0.5, '非法值回落默认 30 分钟');
  assert.equal(normalizeHistoryRangeHours('abc'), 0.5);
});

test('滑杆双段换算：无极区 1 分钟粒度往返一致；挡位段吸附与停靠位', () => {
  const historyPosToHours = resolve('historyPosToHours');
  const historyHoursToPos = resolve('historyHoursToPos');
  // 无极区端点与中间整分钟往返
  for (const minutes of [30, 45, 61, 89, 90, 120]) {
    const hours = minutes / 60;
    assert.equal(historyPosToHours(historyHoursToPos(hours)), hours, `${minutes} 分钟往返一致`);
  }
  // 位置→小时:无极区按 1 分钟粒度
  assert.equal(historyPosToHours(0), 0.5);
  assert.equal(historyPosToHours(400), 2, '无极区末端是 2h');
  // 挡位段:落入即吸附
  assert.equal(historyPosToHours(401), 6, '无极区之后立刻是 6h 挡');
  assert.equal(historyPosToHours(460), 6, '6h 停靠位');
  assert.equal(historyPosToHours(580), 12, '12h 停靠位');
  assert.equal(historyPosToHours(940), 720, '30 天停靠位(=滑杆 max,可填满轨道)');
  assert.equal(historyPosToHours(9999), 720, '越界钳到尾挡');
  // 挡位小时→位置:停靠位
  assert.equal(historyHoursToPos(6), 460);
  assert.equal(historyHoursToPos(12), 580);
  assert.equal(historyHoursToPos(24), 700);
  assert.equal(historyHoursToPos(168), 820);
  assert.equal(historyHoursToPos(720), 940);
});

test('渲染抽稀：≤2h 全精度，>2h 5 抽 1，>6h 15 抽 1；保留末点且不断口', () => {
  const historyDecimationStride = resolve('historyDecimationStride');
  const historyThinByStride = resolve('historyThinByStride');
  assert.equal(historyDecimationStride(0.5), 1);
  assert.equal(historyDecimationStride(2), 1, '2h 无极区端点仍全精度');
  assert.equal(historyDecimationStride(2.5), 5, '>2h 即 5 抽 1');
  assert.equal(historyDecimationStride(6), 5, '6h 恰好不大于 6,仍 5 抽 1');
  assert.equal(historyDecimationStride(6.5), 15, '>6h 为 15 抽 1');
  assert.equal(historyDecimationStride(12), 15);
  assert.equal(historyDecimationStride(24), 15);
  assert.equal(historyDecimationStride(720), 15);
  const samples = Array.from({ length: 100 }, (_, i) => ({ ts: i, v: i }));
  const five = historyThinByStride(samples, 5);
  assert.equal(five.length, 21, '100 点 5 抽 1 → 20 点 + 末点');
  assert.equal(five[0].ts, 0, '保留首点');
  assert.deepEqual(five[five.length - 1], { ts: 99, v: 99 }, '保留最后一个点');
  assert.equal(historyThinByStride(samples, 1).length, 100, 'stride 1 原样返回');
});

test('历史时间刻度：2 小时与 12 小时档固定步长 1800/10800（4–5 条网格线）', () => {
  const historyTimeTicks = resolve('historyTimeTicks');
  const start = 1700000000;
  const twoHour = historyTimeTicks(start, start + 7200, 2);
  assert.ok(twoHour.length >= 4, '2h 范围至少 4 条刻度（30 分钟步长）');
  assert.ok(twoHour.every((ts) => ts % 1800 === 0), '2h 范围刻度应为 30 分钟步长');
  const twelveHour = historyTimeTicks(start, start + 43200, 12);
  assert.ok(twelveHour.length >= 4, '12h 范围至少 4 条刻度（3 小时步长）');
  assert.ok(twelveHour.every((ts) => ts % 10800 === 0), '12h 范围刻度应为 3 小时步长');
});

test('风扇组配色：黑灰阶梯取色，不混入温度传感器色系', () => {
  const historyChildColor = resolve('historyChildColor');
  const HISTORY_GROUPS = resolve('HISTORY_GROUPS');
  const HISTORY_FAN_SHADES = resolve('HISTORY_FAN_SHADES');
  const fanGroup = HISTORY_GROUPS.find((group) => group.key === 'fan');
  assert.equal(fanGroup.color, '#6e7780', '风扇组基础色应为中性灰');
  const fanIDs = ['fan1', 'fan2', 'fan3', 'fan4'];
  const colors = fanIDs.map((id) => historyChildColor('fan', id, fanIDs));
  // vm 上下文里的数组原型与宿主不同，先展开成普通数组再比较
  assert.deepEqual([...colors], [...HISTORY_FAN_SHADES.slice(0, 4)], '按子类序号取灰色阶梯');
  assert.equal(new Set(colors).size, 4, '4 个风扇颜色彼此可区分');
  // 温度组仍走 HSL 派生（同色相明度阶梯），与风扇灰阶不冲突
  const sataIDs = ['front-1', 'front-2'];
  const sataColors = sataIDs.map((id) => historyChildColor('sata', id, sataIDs));
  assert.ok(sataColors.every((color) => color.startsWith('hsl(')));
  assert.equal(new Set(sataColors).size, 2);
});

// ---- 历史设置与保存天数 ----

test('fillHistoryInputs：保存天数缺省或旧后端无字段时按 30 天兜底', () => {
  const { resolve, element } = loadAppContext();
  resolve('fillHistoryInputs')({ enabled: true, max_size_mb: 128, retention_days: 45 });
  assert.equal(element('history-retention-days').value, 45);
  assert.equal(element('history-max-size').value, 128);
  assert.equal(element('history-enabled').checked, true);
  resolve('fillHistoryInputs')({ retention_days: 0 }); // 0 等非法值同样兜底
  assert.equal(element('history-retention-days').value, 30);
  resolve('fillHistoryInputs')({}); // 旧后端无 retention_days 字段
  assert.equal(element('history-retention-days').value, 30);
  assert.equal(element('history-max-size').value, 64);
});

test('历史设置校验：上限 8–1024 MB、保存天数至少 1 天（不设上限），越界返回对应文案', () => {
  const historySettingsError = resolve('historySettingsError');
  assert.equal(historySettingsError(64, 30), null);
  assert.equal(historySettingsError(8, 1), null, '下限边界应通过');
  assert.equal(historySettingsError(1024, 90), null, '上限边界应通过');
  assert.equal(historySettingsError(64, 365), null, '保存天数不设产品上限');
  assert.equal(historySettingsError(7, 30), '数据库大小上限需在 8–1024 MB 之间。');
  assert.equal(historySettingsError(1025, 30), '数据库大小上限需在 8–1024 MB 之间。');
  assert.equal(historySettingsError(64, 0), '保存天数需至少为 1 天。');
  assert.equal(historySettingsError(64, NaN), '保存天数需至少为 1 天。');
});

test('保存历史设置：请求体携带 retention_days 与长期记录字段，成功后按启停状态提示', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const { element, click } = loadAppContext({ fetch });
  element('history-max-size').value = '96';
  element('history-retention-days').value = '45';
  element('history-enabled').checked = true;
  element('history-archive-enabled').checked = true;
  element('history-archive-dir').value = ' /vol1/1000/长期记录 ';
  await click('save-history');
  const saves = requests.filter((req) => req.url.includes('api/config/history')); // 过滤掉加载期的 api/status 等
  assert.equal(saves.length, 1);
  assert.equal(saves[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(saves[0].init.body), {
    enabled: true, max_size_mb: 96, retention_days: 45,
    archive_enabled: true, archive_dir: '/vol1/1000/长期记录', // 前端 trim，后端负责绝对路径校验
  });
  assert.equal(element('message-history').textContent, '历史设置已保存；后台每分钟继续写入采样。');
});

test('保存历史设置：开启长期记录但未填位置时不发请求', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const { element, click } = loadAppContext({ fetch });
  element('history-max-size').value = '64';
  element('history-retention-days').value = '30';
  element('history-archive-enabled').checked = true;
  element('history-archive-dir').value = '   ';
  await click('save-history');
  assert.equal(requests.filter((req) => req.url.includes('api/config/history')).length, 0, '缺位置应拦截');
  assert.equal(element('message-history').textContent, '开启长期记录需先填写保存位置。');
});

test('保存历史设置：保存天数越界时不发请求，仅在 message-history 提示', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const { element, click } = loadAppContext({ fetch });
  element('history-max-size').value = '64';
  element('history-retention-days').value = '0';
  await click('save-history');
  assert.equal(requests.filter((req) => req.url.includes('api/config/history')).length, 0, '越界时应拦截，不发 POST');
  assert.equal(element('message-history').textContent, '保存天数需至少为 1 天。');
});

test('风扇调试参数四舍五入为整数——手输 1.5 这类小数不得原样发到后端', async () => {
  const { requests, fetch } = recordingFetch(() => ({ fans: [] }));
  const app = loadAppContext({ fetch });
  // 自动测试:步进/间隔取整
  await app.resolve('applyFanDebugAuto')('it8:fan1', true, 1.5, 2.7, 'percent');
  const auto = JSON.parse(requests.find((req) => req.url.includes('api/fans/debug/auto')).init.body);
  assert.equal(auto.fans[0].step, 2);
  assert.equal(auto.fans[0].interval, 3);
  // 调试转速取整
  await app.resolve('applyFanDebugPWM')('it8:fan1', 45.6, 'percent');
  const pwm = JSON.parse(requests.find((req) => req.url.includes('api/fans/debug/pwm')).init.body);
  assert.equal(pwm.value, 46);
});

test('清空数据库：点击即补冲刷（取消也发）；确认后 POST api/history/clear 并强制刷新缓存', async () => {
  const cancelled = recordingFetch(() => ({ ok: true, flushed: false }));
  const cancelledApp = loadAppContext({ fetch: cancelled.fetch, confirm: () => false });
  await cancelledApp.click('history-clear');
  assert.ok(
    cancelled.requests.some((req) => req.url.includes('api/history/archive')),
    '点击瞬间就应补冲刷（取消确认也发）',
  );
  assert.equal(cancelled.requests.filter((req) => req.url.includes('api/history/clear')).length, 0, '取消确认时不应发清空请求');

  const { requests, fetch } = recordingFetch((href) => (href.includes('api/history/archive') ? { ok: true, flushed: true } : href.includes('api/history/clear') ? { ok: true } : { samples: [] }));
  const app = loadAppContext({ fetch });
  await app.click('history-clear');
  const archiveIndex = requests.findIndex((req) => req.url.includes('api/history/archive'));
  const clearIndex = requests.findIndex((req) => req.url.includes('api/history/clear'));
  assert.ok(archiveIndex >= 0, '确认流程也应先补冲刷');
  assert.ok(clearIndex > archiveIndex, '清空请求应在补冲刷之后');
  assert.equal(requests[clearIndex].init.method, 'POST');
  assert.ok(
    requests.some((req, index) => index > clearIndex && req.url.includes('api/history?range=')),
    '清空后应强制刷新历史缓存',
  );
  assert.equal(app.element('message-history').textContent, '历史数据库已清空。');
});

test('清空数据库：补冲刷失败时弹窗明示后果，用户确认仍可清空', async () => {
  const confirms = [];
  const { requests, fetch } = recordingFetch((href) => (href.includes('api/history/archive') ? { ok: false, error: 'HDD 不可写' } : href.includes('api/history/clear') ? { ok: true } : { samples: [] }));
  const app = loadAppContext({
    fetch,
    confirm: (message) => { confirms.push(message); return true; },
  });
  await app.click('history-clear');
  assert.equal(requests.filter((req) => req.url.includes('api/history/clear')).length, 1, '用户确认后仍应清空');
  assert.match(confirms[0], /冲刷失败/, '弹窗应明示冲刷失败与数据丢失后果');
});

test('档位记忆挂后端：切档 POST api/config/ui-prefs；首帧采纳后端档位，手动切过后不被覆盖', async () => {
  // 切档双写：localStorage + 后端配置
  const wrote = recordingFetch(() => ({ samples: [] }));
  const writeApp = loadAppContext({ fetch: wrote.fetch });
  writeApp.resolve('setHistoryRange')(6);
  const prefPosts = wrote.requests.filter((req) => req.url.includes('api/config/ui-prefs'));
  assert.equal(prefPosts.length, 1, '切档应 POST 一次 ui-prefs');
  assert.equal(prefPosts[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(prefPosts[0].init.body), { history_range_hours: 6, fan_debug_visible: false, history_series: {}, history_children: {} }, 'ui_prefs 整段替换,必须带上当前全部偏好字段');

  // 独立上下文（未手动切档）：后端下发 2 小时 → 采纳并按新档位取数
  const adopt = recordingFetch(() => ({ samples: [] }));
  const adoptApp = loadAppContext({ fetch: adopt.fetch });
  adoptApp.resolve('applyBackendHistoryRange')({ history_range_hours: 2 });
  assert.equal(adoptApp.resolve('historyRangeHours'), 2, '应采纳后端档位');
  assert.ok(adopt.requests.some((req) => req.url.includes('api/history?range=2')), '采纳后应按新档位取数');
  // 已采纳过后再下发不同档位：不覆盖
  adoptApp.resolve('applyBackendHistoryRange')({ history_range_hours: 12 });
  assert.equal(adoptApp.resolve('historyRangeHours'), 2, '二次下发不应覆盖');
});

test('历史分组子类：空通道过滤、组别归类、聚合项与勾选可见性', () => {
  const historyGroupChildIDs = resolve('historyGroupChildIDs');
  const historyGroupSeries = resolve('historyGroupSeries');
  const historyChildValue = resolve('historyChildValue');
  const historyFanLabel = resolve('historyFanLabel');
  const historySlotLabel = resolve('historySlotLabel');
  const samples = [
    { ts: 0, cpu_c: 55, hdd_c: 41, nvme_c: 48,
      sensors: [{ group: 'cpu', key: 'Core 0', c: 55 }, { group: 'cpu', key: 'Package id 0', c: 52 }, { group: 'nic', key: 'igc:PHY', c: 62 }, { group: 'other', key: 'acpi:temp1', c: 30 }],
      fans: [{ id: 'it8613:fan2', rpm: 0, pwm_percent: 0 }, { id: 'it8613:fan3', rpm: 1200, pwm_percent: 45 }],
      disks: [{ id: 'front-2', c: 41.2 }, { id: 'm2-1', c: 48 }] },
    { ts: 60, cpu_c: 57, hdd_c: 42, nvme_c: 49,
      sensors: [{ group: 'cpu', key: 'Core 0', c: 57 }, { group: 'cpu', key: 'Package id 0', c: 53 }, { group: 'nic', key: 'igc:PHY', c: 63 }, { group: 'other', key: 'acpi:temp1', c: 31 }],
      fans: [{ id: 'it8613:fan2', rpm: 0, pwm_percent: 0 }, { id: 'it8613:fan3', rpm: 1300, pwm_percent: 55 }],
      disks: [{ id: 'front-2', c: 42 }, { id: 'm2-1', c: 49 }] },
  ];
  // CPU 组：聚合项 + 两个传感器；SATA/NVMe 组：聚合项 + 各自盘位（不串组）
  assert.deepEqual([...historyGroupChildIDs('cpu', samples)], ['__agg__', 'Core 0', 'Package id 0']);
  assert.deepEqual([...historyGroupChildIDs('sata', samples)], ['__agg__', 'front-2']);
  assert.deepEqual([...historyGroupChildIDs('nvme', samples)], ['__agg__', 'm2-1']);
  // 网卡组：只有网卡传感器；其它组：非网卡非 CPU 传感器
  assert.deepEqual([...historyGroupChildIDs('nic', samples)], ['igc:PHY']);
  assert.deepEqual([...historyGroupChildIDs('other', samples)], ['acpi:temp1']);
  // 风扇组：全 0 空通道过滤
  assert.deepEqual([...historyGroupChildIDs('fan', samples)], ['it8613:fan3']);
  assert.equal(historyFanLabel('it8613:fan3'), 'fan3');
  assert.equal(historySlotLabel('front-2'), '前置2');

  // 子类取值：聚合与单盘
  assert.equal(historyChildValue('sata', '__agg__', samples[0]), 41);
  assert.equal(historyChildValue('sata', 'front-2', samples[1]), 42);
  assert.equal(historyChildValue('cpu', 'Core 0', samples[1]), 57);
  assert.equal(historyChildValue('nic', 'igc:PHY', samples[0]), 62);

  // 组曲线可见性：未记录走默认（温度组只勾聚合）→ 一条线；显式 null=全部
  const defaultSeries = historyGroupSeries('sata', samples, 60);
  assert.equal(defaultSeries.length, 1);
  assert.equal(defaultSeries[0].id, 'sata:__agg__', '默认只画聚合线');
  const setHistoryChildSelection = resolve('setHistoryChildSelection');
  setHistoryChildSelection('sata', null);
  const allSeries = historyGroupSeries('sata', samples, 60);
  assert.equal(allSeries.length, 2);
  setHistoryChildSelection('sata', new Set(['__agg__']));
  const aggOnly = historyGroupSeries('sata', samples, 60);
  setHistoryChildSelection('sata', null);
  assert.equal(aggOnly.length, 1, '只勾聚合项时应只有一条线');
  assert.equal(aggOnly[0].id, 'sata:__agg__');
  assert.equal(aggOnly[0].color, '#18a779');
});

test('父类取消勾选连带清空子类勾选，重新勾上后子类为空', () => {
  const setHistoryChildSelection = resolve('setHistoryChildSelection');
  const historyChildSelectionFor = resolve('historyChildSelectionFor');
  const setHistoryGroupEnabled = resolve('setHistoryGroupEnabled');
  const seriesEnabled = (key) => resolve('historySeriesEnabledFor')(key);
  setHistoryChildSelection('sata', new Set(['__agg__', 'front-1']));
  setHistoryGroupEnabled('sata', false); // 取消父类
  assert.equal(seriesEnabled('sata'), false, '父类应记录为关闭');
  const cleared = historyChildSelectionFor('sata');
  assert.ok(cleared && cleared.size === 0, `取消父类应清空子类勾选集合，实际 ${cleared}`);
  // 重新勾上父类:子类仍为空（残留会违背"取消=清空"的直觉）
  setHistoryGroupEnabled('sata', true);
  assert.equal(seriesEnabled('sata'), true);
  assert.ok(historyChildSelectionFor('sata').size === 0);
});

test('取消父类不影响其它组的子类勾选', () => {
  const setHistoryChildSelection = resolve('setHistoryChildSelection');
  const historyChildSelectionFor = resolve('historyChildSelectionFor');
  const setHistoryGroupEnabled = resolve('setHistoryGroupEnabled');
  setHistoryChildSelection('sata', new Set(['front-1']));
  setHistoryChildSelection('fan', new Set(['fan3']));
  setHistoryGroupEnabled('sata', false);
  assert.equal(historyChildSelectionFor('fan').has('fan3'), true, '其它组勾选应原样保留');
});

test('index.html 面板结构：相邻 tab-panel 之间 section 开闭配对，防止面板被嵌套', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const markers = [...html.matchAll(/id="panel-[a-z]+"/g)].map((match) => match.index);
  assert.ok(markers.length >= 6, '应存在至少 6 个面板');
  for (let i = 0; i < markers.length - 1; i++) {
    const segment = html.slice(markers[i], markers[i + 1]);
    const opens = (segment.match(/<section\b/g) || []).length;
    const closes = (segment.match(/<\/section>/g) || []).length;
    assert.equal(opens, closes, `panel 区间 ${i}（${html.slice(markers[i], markers[i] + 60)}…）section 未配对，会把后续面板嵌套进去`);
  }
});

test('传感器父类下拉选项与后端 sensorGroupValues 契约一致（gpu|nic|other，空值=默认）', () => {
  const options = resolve('SENSOR_GROUP_OPTIONS');
  assert.equal(options.map((option) => option.value).sort().join(','), 'gpu,nic,other');
  assert.ok(options.every((option) => option.label && option.label !== option.value), '每项都要有中文标签');
});

test('切到调试页：拉取历史后渲染传感器显示名列表（PR#7 重构曾丢失该钩子）', async () => {
  const { requests, fetch } = recordingFetch((href) => (
    href.includes('api/history')
      ? { version: 1, interval_seconds: 60, samples: [{ ts: 1700000000, sensors: [{ group: 'cpu', key: 'Core 0', c: 50 }] }] }
      : {}
  ));
  // rAF 立即执行回调，activateTab 的进页钩子才能在本用例内跑完
  const ctx = loadAppContext({ fetch, requestAnimationFrame: (fn) => fn() });
  // 包一层计数 spy：钩子是否存在决定渲染函数是否被调用
  ctx.resolve('renderSensorNamesList = (function (orig) { return function () { globalThis.__sensorNamesRenders = (globalThis.__sensorNamesRenders || 0) + 1; return orig.apply(this, arguments); }; })(renderSensorNamesList)');
  ctx.resolve("activateTab('tab-debug')");
  await new Promise((done) => setImmediate(done));
  assert.ok(requests.some((req) => req.url.includes('api/history')), '切调试页应拉取历史数据');
  assert.ok(ctx.resolve('__sensorNamesRenders') >= 1, '历史返回后应渲染传感器显示名列表');
});

test('运行日志：保存校验并 POST /api/config/log；导出发起下载；清空经确认后 POST /api/log/clear', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const { element, click } = loadAppContext({ fetch });
  // 回填:fillHistoryInputs 应顺带填入 runlog-max-size(status.config.log)
  element('history-max-size').value = '64';
  element('history-retention-days').value = '30';
  element('runlog-max-size').value = '16';
  // 越界拦截
  element('runlog-max-size').value = '999';
  await click('save-runlog');
  assert.equal(requests.filter((req) => req.url.includes('api/config/log')).length, 0, '越界应拦截');
  assert.equal(element('runlog-status').textContent, '日志大小上限需在 1–256 MB 之间。');
  // 合法保存
  element('runlog-max-size').value = '32';
  await click('save-runlog');
  const saves = requests.filter((req) => req.url.includes('api/config/log'));
  assert.equal(saves.length, 1);
  assert.deepEqual(JSON.parse(saves[0].init.body), { max_size_mb: 32 });
  assert.match(element('runlog-status').textContent, /32 MB/);
  // 清空:取消不发
  const cancelled = loadAppContext({ fetch: recordingFetch(() => ({})).fetch, confirm: () => false });
  await cancelled.click('runlog-clear');
  assert.equal(requests.filter((req) => req.url.includes('api/log/clear')).length, 0, '取消确认不应清空');
  // 确认后清空
  await click('runlog-clear');
  const clears = requests.filter((req) => req.url.includes('api/log/clear'));
  assert.equal(clears.length, 1);
  assert.equal(clears[0].init.method, 'POST');
  assert.equal(element('runlog-status').textContent, '运行日志已清空。');
});

test('fanDebugStatusInfo：紧急覆盖优先，其次列出递增中/已完成风扇，无事返回 null', () => {
  const info = resolve('fanDebugStatusInfo');
  const fans = (overrides = []) => [
    { id: 'it8613:hwmon3:fan1', name: 'fan1' },
    { id: 'it8613:hwmon3:fan2', name: 'fan2' },
    ...[],
  ].map((fan) => {
    const patch = overrides.find((item) => item.id === fan.id) || {};
    return { ...fan, ...patch };
  });
  // 紧急温度覆盖最优先
  const emergency = info({ emergency: true, fans: fans([{ id: 'it8613:hwmon3:fan1', auto_running: true }]) });
  assert.equal(emergency.error, true);
  assert.match(emergency.text, /紧急温度/);
  // 递增中:列出风扇名
  const running = info({ fans: fans([
    { id: 'it8613:hwmon3:fan1', auto_running: true },
    { id: 'it8613:hwmon3:fan2', auto_running: true },
  ]) });
  assert.equal(running.error, undefined);
  assert.match(running.text, /fan1、fan2/);
  assert.match(running.text, /独立推进/);
  // 全部停了但有人跑完:完成提示
  const done = info({ fans: fans([{ id: 'it8613:hwmon3:fan1', auto_done: true }]) });
  assert.match(done.text, /已完成自动递增：fan1/);
  // 什么都没有:null(清空状态行)
  assert.equal(info({ fans: fans() }), null);
  assert.equal(info({}), null);
});

test('applyFanDebugAuto：开=按风扇 POST fans 数组；关=POST auto/stop 单 id', async () => {
  const { requests, fetch } = recordingFetch(() => ({ fans: [] }));
  const { resolve: fn } = loadAppContext({ fetch });
  await fn('applyFanDebugAuto')('it8613:hwmon3:fan1', true, 10, 5, 'pwm');
  const start = requests.filter((req) => req.url.includes('api/fans/debug/auto'));
  assert.equal(start.length, 1);
  assert.equal(start[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(start[0].init.body), { fans: [{ id: 'it8613:hwmon3:fan1', step: 10, interval: 5, unit: 'pwm' }] });
  await fn('applyFanDebugAuto')('it8613:hwmon3:fan1', false, 0, 0, '');
  const stops = requests.filter((req) => req.url.includes('api/fans/debug/auto/stop'));
  assert.equal(stops.length, 1);
  assert.deepEqual(JSON.parse(stops[0].init.body), { id: 'it8613:hwmon3:fan1' });
});

test('风扇调试单位表:三种单位(RPM/%/PWM)、后缀与线性换算契约', () => {
  const units = resolve('FAN_DEBUG_UNITS');
  assert.equal(units.map((unit) => unit.value).join(','), 'rpm,percent,pwm');
  assert.equal(units.map((unit) => unit.suffix).join('|'), 'RPM|%|', 'PWM 不带符号');
  assert.equal(units.map((unit) => unit.max).join(','), '2000,100,255');
  const convert = resolve('convertDebugValue');
  assert.equal(convert(50, 'percent', 'pwm'), 128);
  assert.equal(convert(128, 'pwm', 'percent'), 50);
  assert.equal(convert(50, 'percent', 'rpm'), 1000);
  assert.equal(convert(1000, 'rpm', 'percent'), 50);
  assert.equal(convert(2000, 'rpm', 'pwm'), 255);
  // 满转基准(后端全速实测标定):rpm 换算用基准,未传退回名义 2000
  assert.equal(convert(2250, 'rpm', 'percent', 4500), 50);
  assert.equal(convert(50, 'percent', 'rpm', 4500), 2250);
  assert.equal(convert(100, 'percent', 'rpm', 4500), 4500, '100% 即满转基准');
  assert.equal(convert(4500, 'rpm', 'pwm', 4500), 255);
  assert.equal(convert(2000, 'rpm', 'pwm'), 255, '未标定时按名义 2000');
});

// 回看窗口对象来自 vm 沙箱，原型与宿主不同，deepEqual 会误报——逐字段断言
function assertWindow(actual, start, end, atLive, message) {
  assert.equal(actual.start, start, message);
  assert.equal(actual.end, end, message);
  assert.equal(actual.atLive, atLive, message);
}

test('30 分钟档回看：拉取范围、窗口吸附/夹取/贴最新与数据不足全覆盖', () => {
  const historyFetchHoursFor = resolve('historyFetchHoursFor');
  const historyScrubWindow = resolve('historyScrubWindow');
  // 拉取范围解耦：仅 30 分钟档改拉 7.5 小时（480 点聚合上限内），其余档位原样
  assert.equal(historyFetchHoursFor(0.5), 7.5);
  assert.equal(historyFetchHoursFor(1), 1);
  assert.equal(historyFetchHoursFor(720), 720);

  // 45 个分钟点：默认贴最新 → 窗口=最后 30 点，末端=最新点
  const samples = Array.from({ length: 45 }, (_, i) => ({ ts: 1000 + i * 60 }));
  const latest = 1000 + 44 * 60;
  assertWindow(historyScrubWindow(samples, 60, latest), latest - 29 * 60, latest, true, '默认贴最新');
  // 拖到中间：末端吸附网格（ts=latest-10*60），atLive=false
  assertWindow(historyScrubWindow(samples, 60, latest - 10 * 60), latest - 10 * 60 - 29 * 60, latest - 10 * 60, false, '拖到中间');
  // 末端不在网格上时吸附到最近采样槽（latest+7 → latest）
  const snapped = historyScrubWindow(samples, 60, latest + 7);
  assert.equal(snapped.end, latest, '末端吸附采样网格');
  assert.equal(snapped.atLive, true, '吸附回最新点视为实时');
  // 越过最旧边界：末端夹在 最旧+span，窗口保持 30 点
  assertWindow(historyScrubWindow(samples, 60, 1000), 1000, 1000 + 29 * 60, false, '越过最旧边界被夹取');
  // 数据不足 30 点：窗口覆盖全部并视为实时
  assertWindow(historyScrubWindow(samples.slice(0, 10), 60, 1000 + 9 * 60), 1000, 1000 + 9 * 60, true, '数据不足全覆盖');
  // 空数据兜底
  assertWindow(historyScrubWindow([], 60, 0), 0, 0, true, '空数据兜底');
});

test('串口传感器状态行（多设备）：未保存/未启用/等待/已连接/失败与读数年龄', () => {
  const serialSensorStatusText = resolve('serialSensorStatusText');
  const now = Date.parse('2026-10-09T12:00:00Z');
  assert.equal(serialSensorStatusText(null, now), '尚未保存。');
  assert.equal(serialSensorStatusText({ enabled: false, path: '/dev/ttyUSB0' }, now), '未启用（保留配置，不再读取）。');
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0' }, now),
    '等待连接…。',
  );
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0', open: true, key: 'usb:tty:ttyUSB0', last_celsius: 25.6, last_at: '2026-10-09T11:59:40Z' }, now),
    '已连接；最近读数 25.6 °C（20 秒前）；曲线键 usb:tty:ttyUSB0。',
  );
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0', last_error: 'permission denied' }, now),
    '连接失败：permission denied。',
  );
  // 有错误但读数还新鲜（≤60 秒）：按瞬断重连表述，不吓唬用户
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0', last_error: 'usb:tty:ttyUSB0: read: hangup: EOF', last_celsius: 29.3, last_at: '2026-10-09T11:59:50Z' }, now),
    '读数中断，正在自动重连（usb:tty:ttyUSB0: read: hangup: EOF）；最近读数 29.3 °C（10 秒前）。',
  );
  // 错误持续超过 60 秒无新读数：升级为连接失败
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0', last_error: 'open /dev/ttyUSB0: no such device', last_celsius: 29.3, last_at: '2026-10-09T11:55:00Z' }, now),
    '连接失败：open /dev/ttyUSB0: no such device；最近读数 29.3 °C（300 秒前）。',
  );
  // 时钟倒挂（轮询间隙设备时间漂移）不出现负数年龄
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0', open: true, last_celsius: 26, last_at: '2026-10-09T12:00:10Z' }, now),
    '已连接；最近读数 26.0 °C（0 秒前）。',
  );
  // 旧后端的零值时间戳（0001-01-01）当无读数处理，不渲染出纪元差值"亿秒前"
  assert.equal(
    serialSensorStatusText({ enabled: true, path: '/dev/ttyUSB0', open: true, last_celsius: 0, last_at: '0001-01-01T00:00:00Z' }, now),
    '已连接。',
  );
});

test('历史曲线默认显隐：温度组只开聚合线+风扇全开，GPU/网卡/其它默认关', () => {
  const defaultHistoryGroupEnabled = resolve('defaultHistoryGroupEnabled');
  const defaultHistoryChildSelection = resolve('defaultHistoryChildSelection');
  for (const key of ['cpu', 'sata', 'nvme', 'fan']) {
    assert.equal(defaultHistoryGroupEnabled(key), true, `${key} 默认应展示`);
  }
  for (const key of ['gpu', 'nic', 'other']) {
    assert.equal(defaultHistoryGroupEnabled(key), false, `${key} 默认应关闭`);
  }
  // 温度组默认只勾聚合项
  for (const key of ['cpu', 'sata', 'nvme']) {
    const selection = defaultHistoryChildSelection(key);
    assert.equal(selection.size, 1, `${key} 默认只勾聚合`);
    assert.equal(selection.has('__agg__'), true, `${key} 默认勾 __agg__`);
  }
  // 风扇等组默认全部显示
  for (const key of ['fan', 'gpu', 'nic', 'other']) {
    assert.equal(defaultHistoryChildSelection(key), null, `${key} 默认全部显示`);
  }
});

test('历史曲线显隐：未记录走默认，显式记录优先', () => {
  const historySeriesEnabledFor = resolve('historySeriesEnabledFor');
  const historyChildSelectionFor = resolve('historyChildSelectionFor');
  // 未记录：默认（温度组开+聚合、其它组关）；用 nvme 验证——共享 vm 状态
  // 里 sata 已被前序测试写过显式值
  assert.equal(historySeriesEnabledFor('nvme'), true);
  assert.equal(historySeriesEnabledFor('gpu'), false);
  const nvmeDefault = historyChildSelectionFor('nvme');
  assert.equal(nvmeDefault.has('__agg__'), true);
  // 显式记录：优先于默认
  const setHistoryGroupEnabled = resolve('setHistoryGroupEnabled');
  setHistoryGroupEnabled('gpu', true);
  assert.equal(historySeriesEnabledFor('gpu'), true);
  setHistoryGroupEnabled('nvme', false); // 关组连带清空子类勾选（原设计）
  assert.equal(historySeriesEnabledFor('nvme'), false);
  const cleared = historyChildSelectionFor('nvme');
  assert.equal(cleared.size, 0, '关组后子类勾选应为空集合');
});

test('历史曲线偏好：服务端 ui_prefs 采纳一次，轮询/旧值不冲掉本地修改', () => {
  const applyBackendHistorySeries = resolve('applyBackendHistorySeries');
  const historySeriesEnabledFor = resolve('historySeriesEnabledFor');
  const historyChildSelectionFor = resolve('historyChildSelectionFor');
  // 采纳服务端记录
  applyBackendHistorySeries({ history_series: { gpu: true }, history_children: { cpu: ['__agg__', 'Core 0'], nic: null } });
  assert.equal(historySeriesEnabledFor('gpu'), true, '服务端 gpu=true 应生效');
  const cpu = historyChildSelectionFor('cpu');
  assert.equal(cpu.has('__agg__'), true);
  assert.equal(cpu.has('Core 0'), true);
  assert.equal(historyChildSelectionFor('nic'), null, 'null=全部显示');
  // 本地再改后，第二次采纳（轮询回来的同值）不得覆盖
  const setHistoryGroupEnabled = resolve('setHistoryGroupEnabled');
  setHistoryGroupEnabled('gpu', false);
  applyBackendHistorySeries({ history_series: { gpu: true } });
  assert.equal(historySeriesEnabledFor('gpu'), false, '二次采纳不得覆盖本地修改');
});

test('ui_prefs 保存体：整段替换语义下四个字段全量带上，overrides 最后生效', () => {
  const uiPrefsBody = resolve('uiPrefsBody');
  const setHistoryChildSelection = resolve('setHistoryChildSelection');
  setHistoryChildSelection('cpu', new Set(['__agg__']));
  const body = uiPrefsBody({ fan_debug_visible: true });
  assert.equal(typeof body.history_range_hours, 'number');
  assert.equal(body.fan_debug_visible, true, 'overrides 应覆盖全局值');
  // history_series 只含显式改过的键（前序测试留下 gpu:false）；未记录键
  // 由前端按默认处理，不落盘。字段级断言：vm 跨 realm 对象的原型与外部
  // Object.prototype 不同，deepStrictEqual 会误报
  assert.equal(body.history_series.gpu, false);
  assert.equal(Object.keys(body.history_series).length, 1);
  assert.equal(body.history_children.cpu.length, 1);
  assert.equal(body.history_children.cpu[0], '__agg__');
});
