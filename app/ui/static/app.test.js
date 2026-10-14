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
  // 默认给一个内存版 localStorage：曲线槽位等本地偏好在真实浏览器里要靠它落地，
  // 用例可以传自己的实现来断言写入内容
  const storage = new Map();
  const localStorage = overrides.localStorage || {
    getItem: (key) => (storage.has(key) ? storage.get(key) : null),
    setItem: (key, value) => { storage.set(key, String(value)); },
    removeItem: (key) => { storage.delete(key); },
  };
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
    // 浏览器里 TextEncoder 一直存在（app.js 用它算脚本字节数），但默认不往沙箱里加：
    // 既有用例是在「初始化流程没跑到那一步」的前提下写的，加上它会让一些后台 promise
    // 继续执行、反过来改掉用例的前置状态。要用的用例自己传 overrides.TextEncoder。
    ...(overrides.TextEncoder ? { TextEncoder: overrides.TextEncoder, TextDecoder: overrides.TextDecoder || TextDecoder } : {}),
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
      querySelector: overrides.querySelector || (() => stubElement()),
      querySelectorAll: overrides.querySelectorAll || (() => []),
      addEventListener() {},
      body: stubElement(),
      documentElement: stubElement(),
    },
  };
  sandbox.window = {
    matchMedia,
    addEventListener() {},
    localStorage,
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

// ---- 风扇曲线：顺序 / 默认值 / 预设 / 自定义槽位 / 导出 ----

test('曲线顺序：HDD/SATA → NVMe → CPU（标签栏、编辑器、CURVE_KINDS 三处一致）', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const order = ['hdd', 'nvme', 'cpu'];

  const tabs = order.map((kind) => html.indexOf(`id="curve-tab-${kind}"`));
  assert.ok(tabs.every((index) => index > 0), '三个标签按钮都要在');
  assert.deepEqual([...tabs].sort((a, b) => a - b), tabs, '标签栏顺序应为 HDD/SATA → NVMe → CPU');

  const editors = order.map((kind) => html.indexOf(`data-curve-kind="${kind}"`));
  assert.ok(editors.every((index) => index > 0), '三张编辑器都要在');
  assert.deepEqual([...editors].sort((a, b) => a - b), editors, '编辑器 DOM 顺序要跟标签栏一致');

  assert.deepEqual(Array.from(resolve('CURVE_KINDS')), order, 'CURVE_KINDS 决定默认停在第一张');

  // 默认展示第一张（HDD/SATA），另外两张收起；第一张对应标签 aria-selected
  assert.ok(!/data-curve-kind="hdd"[^>]*hidden/.test(html), 'HDD/SATA 编辑器默认要展示');
  assert.match(html, /data-curve-kind="nvme"[^>]*hidden/, 'NVMe 编辑器默认收起');
  assert.match(html, /data-curve-kind="cpu"[^>]*hidden/, 'CPU 编辑器默认收起');
  assert.match(html, /id="curve-tab-hdd"[^>]*aria-selected="true"/, '默认选中的标签是第一张');

  // 面板标题与结论文案跟着新顺序走
  assert.match(html, /HDD\/SATA、NVMe 与 CPU 三温控曲线/);
  assert.match(html, /最终转速 = HDD\/SATA、NVMe 与 CPU 三条曲线目标的最高值/);
});

test('默认最低转速 40：前端兜底与后端默认值一致，默认曲线第一档也是 40', () => {
  const app = loadAppContext();
  app.resolve('fillFanInputs')({}); // 后端没给 min_pwm_percent 时的兜底
  assert.equal(app.element('fan-min').value, 40, '最低转速兜底应为 40');
  assert.equal(app.resolve('DEFAULT_MIN_PWM_PERCENT'), 40);
  assert.equal(app.resolve('DEFAULT_CPU_CURVE')[0].pwm_percent, 40);
  assert.equal(app.resolve('DEFAULT_STORAGE_CURVE')[0].pwm_percent, 40);
  // 空输入时 fanMinimumPWM 也要给 40（曲线地板的兜底值）
  app.element('fan-min').value = '';
  assert.equal(app.resolve('fanMinimumPWM')(), 40);
  // 后端新装机默认（DefaultFanConfig）保持一致，且不带旧默认的 60
  const go = fs.readFileSync(path.join(__dirname, '..', '..', '..', 'internal', 'powerguard', 'fan.go'), 'utf8');
  assert.match(go, /defaultMinPWMPercent\s*=\s*40/, '后端默认最低转速应为 40');
  assert.match(go, /MinPWMPercent:\s*defaultMinPWMPercent/);
});

test('预设曲线模式：静音/平衡/性能覆盖三条曲线，且满足保存前的全部校验', () => {
  const app = loadAppContext({ confirm: () => true });
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';
  const curveEditors = app.resolve('curveEditors');
  const kinds = Array.from(app.resolve('CURVE_KINDS'));
  const applyCurvePreset = app.resolve('applyCurvePreset');

  const snapshot = () => kinds.reduce((acc, kind) => {
    acc[kind] = curveEditors[kind].curve.map((point) => `${point.temp_c}/${point.pwm_percent}`).join(',');
    return acc;
  }, {});

  for (const mode of ['quiet', 'balanced', 'performance']) {
    applyCurvePreset(mode);
    for (const kind of kinds) {
      const curve = curveEditors[kind].curve;
      assert.ok(curve.length >= 2 && curve.length <= 8, `${mode}/${kind} 节点数应在 2–8`);
      curve.forEach((point, index) => {
        assert.ok(point.temp_c >= 20, `${mode}/${kind} 温度不得低于 20°C`);
        assert.ok(point.pwm_percent >= 40, `${mode}/${kind} 转速不得低于最低转速 40%`);
        assert.ok(point.pwm_percent <= 100, `${mode}/${kind} 转速不得高于 100%`);
        if (index > 0) {
          assert.ok(point.temp_c > curve[index - 1].temp_c, `${mode}/${kind} 温度必须严格递增`);
          assert.ok(point.pwm_percent >= curve[index - 1].pwm_percent, `${mode}/${kind} 转速不能随温度下降`);
        }
      });
      assert.ok(curve[curve.length - 1].temp_c <= 85, `${mode}/${kind} 末节点不得越过紧急满速温度`);
    }
    assert.deepEqual(snapshot(), snapshot(), `${mode} 套用两次结果要一致`);
  }

  // 三档要有实质区别：性能档起步高于静音档，静音档起点是最低转速
  applyCurvePreset('quiet');
  const quiet = snapshot();
  applyCurvePreset('performance');
  const performance = snapshot();
  assert.notEqual(quiet.cpu, performance.cpu, '静音与性能不能是同一套曲线');
  assert.match(quiet.cpu, /^48\/40,/, '静音档从最低转速起步');
  assert.match(performance.cpu, /^34\/55,/, '性能档一上来就给更高转速');
});

test('导出曲线参数：JSON 载荷带三条曲线与三个全局参数', () => {
  const app = loadAppContext();
  app.element('fan-min').value = '45';
  app.element('fan-emergency').value = '90';
  app.element('fan-poll').value = '3';
  const payload = app.resolve('fanCurveExportPayload')();

  assert.equal(payload.app, 'tad-module');
  assert.equal(payload.kind, 'fan-curves');
  assert.equal(payload.version, 1);
  assert.match(payload.exported_at, /^\d{4}-\d{2}-\d{2}T/);
  assert.equal(payload.min_pwm_percent, 45);
  assert.equal(payload.emergency_temp_c, 90);
  assert.equal(payload.poll_seconds, 3);
  assert.equal(payload.curves.hdd.length, 3);
  assert.equal(payload.curves.nvme.length, 3);
  assert.equal(payload.curves.cpu.length, 4);
  assert.equal(payload.curves.cpu[0].pwm_percent, 40);

  // 也可以直接导出某个槽位的曲线（同样的载荷形状）
  const slotPayload = app.resolve('fanCurveExportPayload')({ cpu: [{ temp_c: 30, pwm_percent: 50 }], hdd: [], nvme: [] });
  assert.equal(slotPayload.curves.cpu[0].pwm_percent, 50);
});

test('自定义曲线槽位：空槽位不给载，存入后可载回，坏数据整槽作废', () => {
  const store = new Map();
  const localStorage = {
    getItem: (key) => (store.has(key) ? store.get(key) : null),
    setItem: (key, value) => { store.set(key, String(value)); },
    removeItem: (key) => { store.delete(key); },
  };
  const app = loadAppContext({ localStorage, confirm: () => true });
  app.element('fan-min').value = '40';
  const curveEditors = app.resolve('curveEditors');
  const stored = () => JSON.parse(store.get('tad-fan-curve-slots') || '{}');

  // 空槽位：点名字只提示，不写存储、不动曲线
  const before = curveEditors.cpu.curve.length;
  app.click('curve-custom-1');
  assert.equal(store.has('tad-fan-curve-slots'), false, '空槽位不该写本地存储');
  assert.equal(curveEditors.cpu.curve.length, before, '空槽位不该改动当前曲线');

  // 存：当前三条曲线进槽位 1，槽位按钮标记为「已保存」
  app.element('fan-min').value = '40';
  app.resolve('applyCurvePreset')('balanced');
  app.click('curve-custom-1-save');
  const saved = stored()['1'];
  assert.ok(saved, '槽位 1 应被写入');
  assert.equal(saved.cpu.length, 4);
  assert.equal(saved.hdd.length, 3);
  assert.equal(saved.nvme.length, 3);
  assert.match(app.element('curve-custom-1').title, /载入自定义 1/);

  // 改坏当前曲线后载回：应恢复槽位里的节点
  curveEditors.cpu.curve = [{ temp_c: 20, pwm_percent: 30 }, { temp_c: 95, pwm_percent: 100 }];
  app.click('curve-custom-1');
  assert.equal(curveEditors.cpu.curve.length, 4, '载入应把槽位里的 4 个节点放回来');
  assert.equal(curveEditors.cpu.curve[0].pwm_percent, saved.cpu[0].pwm_percent);
  assert.equal(curveEditors.cpu.curve[0].temp_c, saved.cpu[0].temp_c);

  // 槽位之间互不干扰
  assert.match(app.element('curve-custom-2').title, /还是空的/);

  // 坏数据（节点数不足）整槽作废，不污染曲线编辑器
  const broken = loadAppContext({
    localStorage: {
      getItem: () => JSON.stringify({ 1: { cpu: [{ temp_c: 40, pwm_percent: 40 }], hdd: [], nvme: [] } }),
      setItem() {}, removeItem() {},
    },
    confirm: () => true,
  });
  assert.equal(Object.keys(broken.resolve('curveSlots')).length, 0, '缺节点的槽位应被丢弃');
  assert.match(broken.element('curve-custom-1').title, /还是空的/);
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

test('历史范围读数固定在滑杆正中：不跟随拇指左右移动', () => {
  const css = fs.readFileSync(path.join(__dirname, 'styles.css'), 'utf8');
  assert.match(css, /\.history-range-value \{[\s\S]*?align-self: center/, '读数要在滑杆上居中（按内容宽度）');
  assert.ok(!/\.history-range-value \{[\s\S]*?var\(--thumb-pct/.test(css), '读数不该再按拇指位置偏移');
  assert.ok(!css.includes('--thumb-pct'), '跟随拇指的 CSS 变量应已删除');
  const js = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  assert.ok(!js.includes('syncRunLogThumbPct'), '跟随拇指的 JS 逻辑应已删除');
  assert.ok(!js.includes('--thumb-pct'), 'JS 不该再写跟随拇指的变量');
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
  assert.ok(adopt.requests.some((req) => req.url.includes('range=16&max_points=2000')), '采纳后应按新档位取数（窗口式拉 8 倍跨度 + 大点数上限）');
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

test('窗口式回看：拉取范围、窗口吸附/夹取/贴最新与数据不足全覆盖', () => {
  const historyFetchHoursFor = resolve('historyFetchHoursFor');
  const historyIsWindowed = resolve('historyIsWindowed');
  const historyScrubWindow = resolve('historyScrubWindow');
  // 拉取范围：30 分钟档拉 7.5 小时；≤24 小时档位拉 8 倍跨度；7 天/30 天全览原样
  assert.equal(historyIsWindowed(0.5), true);
  assert.equal(historyIsWindowed(2), true);
  assert.equal(historyIsWindowed(24), true);
  assert.equal(historyIsWindowed(168), false);
  assert.equal(historyFetchHoursFor(0.5), 7.5);
  assert.equal(historyFetchHoursFor(1), 8);
  assert.equal(historyFetchHoursFor(6), 48);
  assert.equal(historyFetchHoursFor(24), 192);
  assert.equal(historyFetchHoursFor(168), 168);
  assert.equal(historyFetchHoursFor(720), 720);

  // 45 个分钟点、30 分钟窗口：默认贴最新 → 窗口=最后 30 分钟，末端=最新点
  const samples = Array.from({ length: 45 }, (_, i) => ({ ts: 1000 + i * 60 }));
  const latest = 1000 + 44 * 60;
  assertWindow(historyScrubWindow(samples, 60, latest, 30 * 60), latest - 30 * 60, latest, true, '默认贴最新');
  // 拖到中间：末端吸附网格（ts=latest-10*60），atLive=false
  assertWindow(historyScrubWindow(samples, 60, latest - 10 * 60, 30 * 60), latest - 10 * 60 - 30 * 60, latest - 10 * 60, false, '拖到中间');
  // 末端不在网格上时吸附到最近采样槽（latest+7 → latest）
  const snapped = historyScrubWindow(samples, 60, latest + 7, 30 * 60);
  assert.equal(snapped.end, latest, '末端吸附采样网格');
  assert.equal(snapped.atLive, true, '吸附回最新点视为实时');
  // 越过最旧边界：末端夹在 最旧+span，窗口保持完整跨度
  assertWindow(historyScrubWindow(samples, 60, 1000, 30 * 60), 1000, 1000 + 30 * 60, false, '越过最旧边界被夹取');
  // 数据不足一个窗口：窗口覆盖全部并视为实时
  assertWindow(historyScrubWindow(samples.slice(0, 10), 60, 1000 + 9 * 60, 30 * 60), 1000, 1000 + 9 * 60, true, '数据不足全覆盖');
  // 空数据兜底
  assertWindow(historyScrubWindow([], 60, 0, 30 * 60), 0, 0, true, '空数据兜底');

  // 24 小时窗口在 8 天数据上（6 分钟采样）：贴最新与拖到中间各覆盖 24 小时跨度
  const daySamples = Array.from({ length: 8 * 240 }, (_, i) => ({ ts: 5000 + i * 360 }));
  const dayLatest = 5000 + (8 * 240 - 1) * 360;
  assertWindow(historyScrubWindow(daySamples, 360, dayLatest, 24 * 3600), dayLatest - 24 * 3600, dayLatest, true, '24 小时窗口贴最新');
  assertWindow(
    historyScrubWindow(daySamples, 360, dayLatest - 48 * 3600, 24 * 3600),
    dayLatest - 48 * 3600 - 24 * 3600,
    dayLatest - 48 * 3600,
    false,
    '24 小时窗口拖到两天前',
  );
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

test('浏览对话框：新建文件夹走 /api/backup/mkdir 并直接进入新目录', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/backup/mkdir')) return { path: '/vol1/1000/备份', name: '备份' };
    if (href.includes('api/backup/browse')) {
      return { mode: 'browse', scope: 'places', path: '/vol1/1000/备份', places: [], entries: [], volumes: [] };
    }
    return {};
  });
  const app = loadAppContext({ fetch });
  const pathPicker = app.resolve('pathPicker');
  pathPicker.path = '/vol1/1000';
  pathPicker.scope = 'places';

  const pending = app.resolve('promptCreateDirectory')();
  // 名称首尾空格由前端先去掉，避免发出明显会被后端拒绝的名字
  app.element('confirm-input').value = '  备份  ';
  await app.click('confirm-ok');
  await pending;

  const mkdir = requests.filter((item) => item.url.includes('api/backup/mkdir')).pop();
  assert.ok(mkdir, '应发出新建文件夹请求');
  assert.equal(mkdir.init.method, 'POST');
  assert.deepEqual(JSON.parse(mkdir.init.body), { path: '/vol1/1000', name: '备份', scope: 'places' });
  assert.equal(pathPicker.path, '/vol1/1000/备份', '建完应自动进入新目录');
  assert.match(app.element('path-picker-message').textContent, /已新建文件夹「备份」/);
});

test('浏览对话框：还没进入目录时不发新建请求，只提示', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  app.resolve('pathPicker').path = '';

  await app.resolve('promptCreateDirectory')();

  assert.equal(requests.filter((item) => item.url.includes('api/backup/mkdir')).length, 0);
  const message = app.element('path-picker-message');
  assert.match(message.textContent, /先进入一个目录/);
  assert.match(message.className, /error/);
});

test('浏览对话框：后端拒绝时保留原目录并显示原因', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/backup/mkdir')) return { ok: false, error: '同名文件或文件夹已存在：备份' };
    return {};
  });
  const app = loadAppContext({ fetch });
  const pathPicker = app.resolve('pathPicker');
  pathPicker.path = '/vol1/1000';
  pathPicker.scope = 'places';

  const pending = app.resolve('promptCreateDirectory')();
  app.element('confirm-input').value = '备份';
  await app.click('confirm-ok');
  await pending;

  assert.equal(requests.filter((item) => item.url.includes('api/backup/browse?path=%2Fvol1%2F1000%2F%E5%A4%87%E4%BB%BD')).length, 0, '失败时不应跳进不存在的目录');
  assert.equal(pathPicker.path, '/vol1/1000', '失败后仍停在原目录');
  assert.match(app.element('path-picker-message').textContent, /已存在/);
});


test('复制方式：两种备份卡片选项与顺序一致（转移归档排第二）', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const modesOf = (name) => {
    const re = new RegExp(`name="${name}" value="([a-z_]+)"`, 'g');
    const found = [];
    let match;
    while ((match = re.exec(html)) !== null) found.push(match[1]);
    return found;
  };
  const expected = ['archive', 'move_archive', 'sync', 'copy', 'move_overwrite'];
  assert.deepEqual(modesOf('backup-mode'), expected, '备份任务的复制方式顺序不对');
  assert.deepEqual(modesOf('auto-backup-mode'), expected, '自动备份的复制方式应与备份任务一致');
});


test('打标设备列表：接口回执驱动列表，别名优先于卷标显示', async () => {
  const { fetch } = recordingFetch((href) => {
    if (href.includes('api/backup/auto')) {
      return {
        marker_dir: '.tad-backup/mark.json',
        devices: [
          { identity: '1234-ABCD', alias: '相机SD卡', label: 'NO NAME', marked: true, online: true, removable: true, mount_point: '/media/usb0', subs: ['DCIM'] },
          { identity: 'vol1', label: '', marked: false, online: true, removable: false, mount_point: '/vol1', subs: [] },
          { identity: '5678-EFGH', marked: true, online: false, removable: true, pending_clear: true, mount_point: '/media/usb1', subs: [] },
        ],
      };
    }
    return {};
  });
  const app = loadAppContext({ fetch });
  await app.resolve('refreshAutoBackupDevices')();

  const devices = app.resolve('autoDevices');
  assert.equal(devices.length, 3, '应把三台设备都收进列表');
  assert.equal(app.resolve('autoMarkerDirFile'), '.tad-backup/mark.json', '打标文件位置由后端下发');

  const autoDeviceName = app.resolve('autoDeviceName');
  assert.equal(autoDeviceName(devices[0]), '相机SD卡', '别名优先');
  assert.equal(autoDeviceName({ label: 'NO NAME' }), 'NO NAME', '没有别名时退回卷标');
  assert.equal(autoDeviceName({ device: '/dev/sdc1' }), '/dev/sdc1', '都没有时退回设备名');
  assert.equal(autoDeviceName({}), '未知设备');
});

test('打标列表加载失败：显示原因而不是空白', async () => {
  const { fetch } = recordingFetch(() => ({ ok: false, error: '仅管理员可以查看自动备份状态' }));
  const app = loadAppContext({ fetch });
  await app.resolve('refreshAutoBackupDevices')();
  assert.equal(app.resolve('autoDevices').length, 0, '失败时不该留下上一次的列表');
});

test('打标动作：七个动作各自发出对应的 action 与参数体', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/backup/auto/marker')) {
      return { action: 'mark', message: '已给 相机卡 整块卷打标', devices: [{ identity: 'x', marked: true, online: true, removable: true, mount_point: '/media/usb0' }] };
    }
    return {};
  });
  const app = loadAppContext({ fetch });

  const cases = [
    ['mark', { path: '/media/usb0', alias: '相机卡' }],
    ['sub_add', { path: '/media/usb0/DCIM' }],
    ['sub_remove', { path: '/media/usb0/DCIM' }],
    ['unmark', { path: '/media/usb0' }],
    ['rename', { path: '/media/usb0', alias: '备用盘' }],
    ['offline_unmark', { identity: '5678-EFGH' }],
    ['offline_keep', { identity: '5678-EFGH' }],
  ];
  for (const [action, payload] of cases) {
    await app.resolve('autoBackupMarkerAction')(action, payload);
    const sent = requests.filter((item) => item.url.includes('api/backup/auto/marker')).pop();
    assert.equal(sent.init.method, 'POST', `${action} 应当用 POST`);
    assert.deepEqual(JSON.parse(sent.init.body), { action, ...payload }, `${action} 的请求体不对`);
  }

  // 回执里的设备列表直接用来重绘，并把后端的提示语展示出来
  assert.equal(app.resolve('autoDevices')[0].identity, 'x');
  assert.match(app.element('auto-backup-status').textContent, /整块卷打标/);
});

test('打标失败：把后端的原话（只读 / 权限）显示给用户', async () => {
  const { fetch } = recordingFetch((href) => {
    if (href.includes('api/backup/auto/marker')) return { ok: false, error: '该卷以只读方式挂载（read-only）' };
    if (href.includes('api/backup/auto')) return { devices: [] };
    return {};
  });
  const app = loadAppContext({ fetch });

  await app.resolve('autoBackupMarkerAction')('mark', { path: '/media/ro' });

  const status = app.element('auto-backup-status');
  assert.match(status.textContent, /只读方式挂载/, '失败原因要原样带给用户');
  assert.match(status.className, /error/);
});

test('规则编辑器：子目录输入框只在「NAS → 打标卷」方向可见', () => {
  // 按方向返回不同的选中的单选：to_volume 时需要填卷内目标子目录，from_volume 时不用
  const radio = (value) => {
    const stub = listeningStub();
    stub.value = value;
    stub.checked = true;
    return stub;
  };
  const editor = (direction) => loadAppContext({
    fetch: recordingFetch(() => ({})).fetch,
    querySelector: (selector) => {
      if (selector.includes('auto-backup-direction')) return radio(direction);
      if (selector.includes('auto-backup-mode')) return radio('archive');
      return listeningStub();
    },
  });

  const toVolume = editor('to_volume');
  toVolume.resolve('updateAutoBackupModeControls')();
  assert.equal(toVolume.element('auto-backup-sub-field').hidden, false, 'NAS → 打标卷要填卷内目标子目录');
  assert.equal(toVolume.element('auto-backup-sub-hint').hidden, true);
  assert.equal(toVolume.element('auto-backup-fixed-label').textContent, 'NAS 侧源（要备份的目录）');

  const fromVolume = editor('from_volume');
  fromVolume.resolve('updateAutoBackupModeControls')();
  assert.equal(fromVolume.element('auto-backup-sub-field').hidden, true, '打标卷 → NAS 的卷内范围由打标决定');
  assert.equal(fromVolume.element('auto-backup-sub-hint').hidden, false, '应改显示「由打标决定」的提示');
  assert.equal(fromVolume.element('auto-backup-fixed-label').textContent, 'NAS 侧目标（导入到哪里）');
  // 归档是默认档：不覆盖同名文件由归档语义定，保留份数可改
  assert.equal(fromVolume.element('auto-backup-overwrite').disabled, true, '归档模式不该允许改「覆盖同名文件」');
  assert.equal(fromVolume.element('auto-backup-keep').disabled, false, '归档模式应允许改保留份数');
});

test('复制方式：五档标题与说明就是用户指定的那套措辞', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const expected = [
    ['archive', '按日期增量归档（硬链接增量）', '每次生成一个带时间戳的快照'],
    ['move_archive', '按日期转移归档（复制后清空源）', '把源文件复制进去之后删除源里的文件'],
    ['sync', '同步文件', '目标里多出来的文件会被删除。源文件不动'],
    ['copy', '直接复制覆盖', '同名文件覆盖。目标中多出来的文件原样保留'],
    ['move_overwrite', '移动覆盖（复制后删源）', '复制成功后删除源里的文件'],
  ];
  for (const name of ['backup-mode', 'auto-backup-mode']) {
    for (const [value, title, detail] of expected) {
      const block = new RegExp(`name="${name}" value="${value}">\\s*<span><strong>([^<]+)</strong><small>([^<]+)</small>`);
      const match = html.match(block);
      assert.ok(match, `${name}=${value} 的选项块没找到`);
      assert.equal(match[1], title, `${name}=${value} 的标题不对`);
      assert.ok(match[2].includes(detail), `${name}=${value} 的说明不对：${match[2]}`);
    }
  }
});

test('按键面板：默认只绑网络键短按，保存按钮紧跟按键表', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const status = html.match(/<p id="gpio-status"[^>]*>([^<]+)<\/p>/);
  assert.ok(status, '缺少 gpio-status 说明');
  assert.match(status[1], /默认只有网络键的短按绑定「刷新 SD 卡」/);
  assert.match(status[1], /其余按键与档位默认无动作/);

  // 保存按钮控制放到按键表格（最后一行是后置重置键）下面一行，且在刷新 SD 卡之前
  const rearReset = html.indexOf('data-button="rear_reset"');
  const save = html.indexOf('id="save-gpio"');
  const message = html.indexOf('id="message-gpio"');
  const refresh = html.indexOf('id="gpio-refresh-sdcard"');
  assert.ok(rearReset > 0 && save > rearReset, '保存按钮应排在按键表格之后');
  assert.ok(message > save, '保存结果提示应紧跟保存按钮');
  assert.ok(refresh > message, '刷新 SD 卡区域应保持在其后');
  assert.match(html.slice(rearReset, save), /<div class="actions app-actions gpio-save-row">/, '保存按钮应带独立行样式');
});

test('样式：确认框的隐藏行不占位、面板标题按钮不换行、保存行有间距', () => {
  const css = fs.readFileSync(path.join(__dirname, 'styles.css'), 'utf8');
  // 折叠空白后再比对，免得被换行与缩进绊住
  const flat = css.split(new RegExp('\\s+')).join(' ');
  assert.ok(
    flat.includes('.confirm-input-row[hidden] { display: none !important; }'),
    '确认框隐藏行必须真正不占位（grid 会压过 hidden）',
  );
  assert.ok(
    flat.includes('.backup-keep-field[hidden] { display: none !important; }'),
    '同理：备份编辑器里隐藏的字段也不能占位',
  );
  const headingButton = flat.indexOf('.panel-heading > button {');
  assert.ok(headingButton >= 0, '面板标题按钮需要独立样式');
  assert.ok(
    flat.slice(headingButton, flat.indexOf('}', headingButton)).includes('white-space: nowrap;'),
    '「+ 新增…」按钮不该换行',
  );
  assert.ok(flat.includes('.gpio-save-row {'), '保存行需要独立间距');
  // 未确认就保存时确认项要抖一下：关键帧与动画都必须在样式里
  assert.ok(flat.includes('@keyframes backup-confirm-shake {'), '确认项的抖动关键帧不能丢');
  assert.ok(
    flat.includes('.backup-danger-toggle.needs-confirm.shake { animation: backup-confirm-shake .5s ease-in-out; }'),
    '告警态要挂上抖动动画',
  );
  assert.ok(
    flat.includes('.backup-danger-toggle.needs-confirm { border-radius: 6px; outline: 1px solid var(--danger);'),
    '告警态要有描边高亮（outline 不占位，抖动时不会挤动正文）',
  );
});

test('移动类模式：两个方向都能选，但「NAS → 打标卷」要出示红色警示与确认开关', () => {
  // 后端只在 to_volume + 移动类时额外要求显式确认（allow_move_to_volume），
  // 界面不能再把选项禁掉，但必须把风险讲清楚、并把确认开关摆出来。
  const radiosFor = () => ['archive', 'sync', 'move_archive', 'move_overwrite'].map((value) => {
    const radio = listeningStub(); // app 初始化会给这些 radio 挂 change 监听
    radio.value = value;
    radio.checked = value === 'archive';
    // 先显式置为可选：若有人又把「按方向禁用移动档」写回来，下面的断言就会失败
    radio.disabled = false;
    return radio;
  });
  const editor = (direction, mode = 'archive') => {
    const radios = radiosFor();
    for (const radio of radios) radio.checked = radio.value === mode;
    const app = loadAppContext({
      fetch: recordingFetch(() => ({})).fetch,
      querySelector: (selector) => {
        if (selector.includes('auto-backup-direction')) return { value: direction, checked: true };
        if (selector.includes('auto-backup-mode')) {
          if (selector.includes(':checked')) return radios.find((radio) => radio.checked) || null;
          const match = selector.match(/value="([^"]+)"/);
          return match ? radios.find((radio) => radio.value === match[1]) : null;
        }
        // 其它查询走通用元素桩：app 初始化里还有别的 querySelector（如健康提示的 summary）
        return stubElement();
      },
      querySelectorAll: (selector) => (selector.includes('auto-backup-mode') ? radios : []),
    });
    return { app, radios };
  };
  const moveDisabled = (radios) => radios.filter((radio) => radio.value.startsWith('move')).map((radio) => radio.disabled);

  // 打标卷 → NAS：移动档可用，不需要确认项（源在可移动介质上，删源正是「导入后清空卡」）
  const fromVolume = editor('from_volume', 'move_archive');
  fromVolume.app.resolve('updateAutoBackupModeControls')();
  assert.deepEqual(moveDisabled(fromVolume.radios), [false, false], '打标卷 → NAS 必须能用移动类模式');
  assert.equal(fromVolume.app.element('auto-backup-allow-move-field').hidden, true, '正向不该出现确认项');

  // NAS → 打标卷：仍然可选（不再禁用），但给出红色警示 + 确认开关
  const toVolume = editor('to_volume', 'move_archive');
  toVolume.app.resolve('updateAutoBackupModeControls')();
  assert.deepEqual(moveDisabled(toVolume.radios), [false, false], '反向也不该禁用，改成勾选确认');
  assert.equal(toVolume.app.element('auto-backup-allow-move-field').hidden, false, '反向必须出现确认项');
  const hint = toVolume.app.element('auto-backup-mode-hint');
  assert.match(hint.className, /danger/, '反向移动模式要标红');
  assert.match(hint.textContent, /会删掉源里的文件/);
  assert.match(hint.textContent, /多块盘同时插着时这条规则会被拒绝运行/);

  // 切回非移动档：确认项隐藏并复位，避免带着上一次的勾选保存别的规则
  const plain = editor('to_volume', 'sync');
  plain.app.element('auto-backup-allow-move').checked = true;
  plain.app.resolve('updateAutoBackupModeControls')();
  assert.equal(plain.app.element('auto-backup-allow-move-field').hidden, true);
  assert.equal(plain.app.element('auto-backup-allow-move').checked, false);
});

test('反向移动规则：没勾确认就保存会被拦住，勾上才放行', () => {
  const app = loadAppContext({ fetch: recordingFetch(() => ({})).fetch });
  const validate = app.resolve('validateAutoRuleDraft');
  const rule = {
    id: 'to-disk', name: '归档到移动盘', direction: 'to_volume',
    fixed: { kind: 'path', path: '/vol1/1000/照片' },
    volume_sub: '照片', mode: 'move_archive', overwrite: true, keep: 0,
  };

  assert.equal(validate(rule), false, '未确认的移动规则不能保存');
  assert.match(app.element('auto-backup-message').textContent, /勾选/, '要告诉用户去勾哪里');
  assert.equal(app.element('auto-backup-message').className, 'inline-status error');

  assert.equal(validate({ ...rule, allow_move_to_volume: true }), true, '勾选确认后可以保存');
  // 正向的移动规则从来不需要确认（老配置与老前端提交都不带这个字段）
  assert.equal(validate({ ...rule, direction: 'from_volume', volume_sub: '' }), true, '打标卷 → NAS 不受影响');
  // 反向的非移动规则也不该被误伤
  assert.equal(validate({ ...rule, mode: 'archive' }), true);
});

test('反向移动规则：没勾确认就保存时，确认项抖动告警，勾上或换档后复位', () => {
  const radios = ['archive', 'sync', 'move_archive', 'move_overwrite'].map((value) => {
    const radio = listeningStub();
    radio.value = value;
    radio.checked = false;
    return radio;
  });
  const setMode = (value) => {
    for (const radio of radios) radio.checked = radio.value === value;
  };
  const app = loadAppContext({
    fetch: recordingFetch(() => ({})).fetch,
    querySelector: (selector) => {
      if (selector.includes('auto-backup-direction')) return { value: 'to_volume', checked: true };
      if (selector.includes('auto-backup-mode')) {
        if (selector.includes(':checked')) return radios.find((radio) => radio.checked) || null;
        const match = selector.match(/value="([^"]+)"/);
        return match ? radios.find((radio) => radio.value === match[1]) : null;
      }
      return stubElement();
    },
    querySelectorAll: (selector) => (selector.includes('auto-backup-mode') ? radios : []),
  });
  const alert = () => app.resolve('autoMoveConfirmAlert');
  const draft = {
    id: 'draft', name: '搬走照片', direction: 'to_volume',
    fixed: { kind: 'path', path: '/vol1/1000/照片' },
    volume_sub: '', mode: 'move_overwrite', overwrite: true, keep: 0,
  };

  setMode('move_archive');
  app.resolve('updateAutoBackupModeControls')();
  assert.equal(alert(), false, '还没被拦过的时候不该告警');

  // 没勾确认就保存：抖动 + 高亮 + 红字提示，并且焦点落到确认项上
  assert.equal(app.resolve('validateAutoRuleDraft')(draft), false);
  assert.equal(alert(), true, '拦下未确认的规则后确认项要进入告警态');
  assert.match(app.element('auto-backup-message').textContent, /还没有确认风险/);
  assert.match(app.element('auto-backup-message').textContent, /勾选/);
  assert.equal(app.element('auto-backup-message').className, 'inline-status error');

  // 勾上确认项：告警撤掉
  app.element('auto-backup-allow-move').checked = true;
  app.click('auto-backup-allow-move', 'change');
  assert.equal(alert(), false, '勾上确认后撤掉高亮');

  // 换回不需要确认的档位：同样复位，并且不带着上一次的勾选
  assert.equal(app.resolve('validateAutoRuleDraft')(draft), false);
  assert.equal(alert(), true);
  setMode('archive');
  app.resolve('updateAutoBackupModeControls')();
  assert.equal(alert(), false, '换到非移动档位后复位');
  assert.equal(app.element('auto-backup-allow-move').checked, false, '非移动档位要清掉上一次的勾选');
});

test('反向移动规则：页面上运行前再确认一次，取消就不发请求、试跑不弹窗', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  const rule = {
    id: 'to-disk', name: '归档到移动盘', direction: 'to_volume',
    fixed: { kind: 'path', path: '/vol1/1000/照片' },
    volume_sub: '照片', mode: 'move_overwrite', overwrite: true, keep: 0, allow_move_to_volume: true,
  };

  // 取消：不发出运行请求
  const cancelled = app.resolve('runAutoBackup')('to-disk', false, rule);
  assert.equal(app.element('confirm-title').textContent, '确认删除 NAS 上的源文件');
  assert.match(app.element('confirm-message').textContent, /从 NAS 上删除/);
  assert.match(app.element('confirm-message').textContent, /\/vol1\/1000\/照片/);
  await app.click('confirm-cancel');
  await cancelled;
  assert.equal(requests.filter((item) => item.url.includes('api/backup/auto/run')).length, 0, '取消后不该发起运行');
  assert.match(app.element('auto-backup-status').textContent, /已取消运行/);

  // 确认：正常发起
  const confirmed = app.resolve('runAutoBackup')('to-disk', false, rule);
  await app.click('confirm-ok');
  await confirmed;
  const sent = requests.filter((item) => item.url.includes('api/backup/auto/run')).pop();
  assert.ok(sent, '确认后应发起运行');
  assert.deepEqual(JSON.parse(sent.init.body), { rule, dry_run: false });

  // 试跑不写不删任何东西，不需要确认
  const dry = app.resolve('runAutoBackup')('to-disk', true, rule);
  await dry;
  assert.equal(requests.filter((item) => item.url.includes('api/backup/auto/run')).length, 2, '试跑应直接发出');
});

test('反向移动规则：规则列表上明确标出会删除 NAS 源文件', () => {
  const app = loadAppContext({ fetch: recordingFetch(() => ({})).fetch });
  const risky = { id: 'r1', name: '搬走照片', direction: 'to_volume', fixed: { kind: 'path', path: '/vol1/1000/照片' }, mode: 'move_archive' };
  const safe = { id: 'r2', name: '导入相机卡', direction: 'from_volume', fixed: { kind: 'path', path: '/vol1/1000/导入' }, mode: 'move_archive' };
  assert.equal(app.resolve('autoRuleDeletesNasSource')(risky), true);
  assert.equal(app.resolve('autoRuleDeletesNasSource')(safe), false, '正向删的是卡上的源文件');
  assert.equal(app.resolve('autoRuleDeletesNasSource')({ ...risky, mode: 'archive' }), false);
  assert.equal(app.resolve('autoRuleDeletesNasSource')(undefined), false);
});

test('两张备份卡片：保留份数跟着选中的归档档位走，标识输入框排在开关下方', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');

  for (const [name, keepID, rowID, prefix] of [
    ['backup-mode', 'backup-job-keep', 'backup-job-keep-row', 'backup-job'],
    ['auto-backup-mode', 'auto-backup-keep', 'auto-backup-keep-row', 'auto-backup'],
  ]) {
    const row = html.indexOf(`id="${rowID}"`);
    const sync = html.indexOf(`name="${name}" value="sync"`);
    assert.ok(row > 0 && sync > row, `${rowID} 默认待在这两个「归档」档位之后`);
    // 默认收起：只有选中归档档位时，由 app.js 摆到那个档位正下方
    assert.match(html.slice(row, row + 200), /^[^>]*hidden>/, `${rowID} 默认应收起`);
    assert.match(html.slice(row, row + 220), /class="text-input" type="number"/, `${keepID} 要和其它文本框同款`);
    // 两个归档档位各自带 id，给 app.js 当锚点用
    assert.ok(html.includes(`id="${prefix}-mode-archive"`), `${prefix} 的增量归档档位缺少锚点 id`);
    assert.ok(html.includes(`id="${prefix}-mode-move-archive"`), `${prefix} 的转移归档档位缺少锚点 id`);
  }

  // 说明文字与输入框排在同一行，输入框收窄
  const css = fs.readFileSync(path.join(__dirname, 'styles.css'), 'utf8');
  assert.match(css, /\.backup-mode-keep \{[\s\S]*?display: flex;/, '保留份数要排成一行');
  assert.match(css, /\.backup-mode-keep \.text-input \{[^}]*width: 80px/, '保留份数的输入框要收窄');

  // 「在源中创建标识文件」不再是按钮，而是与「覆盖同名文件」同一种 toggle 开关
  assert.ok(!html.includes('<button id="backup-job-marker-write"'), '标识不该还是按钮');
  assert.ok(
    html.includes('<label class="toggle backup-marker-toggle"><input id="backup-job-marker-write" type="checkbox"><span></span>在源中创建标识文件</label>'),
    '标识开关要用覆盖开关那一种风格',
  );
  // 「源介质标识」这个字段整组排在开关下方：先开开关，再看到标识内容
  const toggle = html.indexOf('id="backup-job-marker-write"');
  const label = html.indexOf('源介质标识（可选，用来确认没认错盘）');
  const marker = html.indexOf('id="backup-job-marker"');
  assert.ok(toggle > 0 && label > 0 && marker > 0, '标识相关的结构不完整');
  assert.ok(toggle < label, '「源介质标识」标签要排在开关下方');
  assert.ok(label < marker, '「源介质标识」输入框要跟在它的标签下面');
  // 说明文案与真实位置一致（标识文件在统一元数据目录里）
  assert.match(html, /名为 <code>\.tad-backup\/source<\/code> 的小文件/);
  assert.match(html, /如果您的数据需要严谨对待那么请创建标识文件，否则：留空＝不校验。/);
});

test('保留份数的落点：只有两个归档档位有锚点，其它档位落空', () => {
  const app = loadAppContext();
  const backupKeepAnchorID = app.resolve('backupKeepAnchorID');

  assert.equal(backupKeepAnchorID('archive', 'backup-job'), 'backup-job-mode-archive');
  assert.equal(backupKeepAnchorID('move_archive', 'backup-job'), 'backup-job-mode-move-archive');
  assert.equal(backupKeepAnchorID('archive', 'auto-backup'), 'auto-backup-mode-archive');
  assert.equal(backupKeepAnchorID('move_archive', 'auto-backup'), 'auto-backup-mode-move-archive');
  for (const mode of ['sync', 'copy', 'move_overwrite', '']) {
    assert.equal(backupKeepAnchorID(mode, 'backup-job'), '', `${mode} 用不到保留份数`);
  }
});

test('保留份数那一行：有锚点才显示，没锚点整行收起', () => {
  const app = loadAppContext();
  const syncBackupKeepRow = app.resolve('syncBackupKeepRow');
  const row = app.element('backup-job-keep-row');

  syncBackupKeepRow('backup-job-keep-row', '');
  assert.equal(row.hidden, true, '非归档档位要把整行收起');

  syncBackupKeepRow('backup-job-keep-row', 'backup-job-mode-archive');
  assert.equal(row.hidden, false, '选中增量归档要显示出来');

  syncBackupKeepRow('backup-job-keep-row', 'backup-job-mode-move-archive');
  assert.equal(row.hidden, false, '换成转移归档也要显示');
});

test('源介质标识：开关打开才写文件，关掉＝不校验', async () => {
  const { requests, fetch } = recordingFetch((url) =>
    url.includes('api/backup/marker') ? { marker: 'tad-abc123', path: '/usb/.tad-backup/source' } : {});
  const app = loadAppContext({ fetch });
  const toggle = app.element('backup-job-marker-write');
  const markerInput = app.element('backup-job-marker');
  const markerRequests = () => requests.filter((item) => item.url.includes('api/backup/marker'));

  // 没选源就打开：不发请求、开关弹回去、红字提示
  toggle.checked = true;
  await app.click('backup-job-marker-write', 'change');
  assert.equal(markerRequests().length, 0, '没选源不该发请求');
  assert.equal(toggle.checked, false, '没选源时开关要弹回去');
  assert.match(app.element('backup-job-message').textContent, /请先选择源/);

  // 选好源之后再打开：立刻写入，并把后端生成的标识回填到输入框
  app.resolve('backupEditorLocations').source = { kind: 'path', path: '/usb' };
  toggle.checked = true;
  await app.click('backup-job-marker-write', 'change');
  const sent = markerRequests().pop();
  assert.ok(sent, '打开开关应把标识写进源介质');
  assert.deepEqual(JSON.parse(sent.init.body), { job: { source: { kind: 'path', path: '/usb' }, marker: '' } });
  assert.equal(markerInput.value, 'tad-abc123', '生成的标识要回填到输入框');
  assert.equal(toggle.checked, true);
  assert.match(app.element('backup-job-message').textContent, /\.tad-backup\/source/);

  // 关掉＝不校验：清空标识，且不再动源里的文件
  markerInput.value = 'tad-abc123';
  toggle.checked = false;
  await app.click('backup-job-marker-write', 'change');
  assert.equal(markerInput.value, '', '关掉后留空＝不校验');
  assert.equal(markerRequests().length, 1, '关掉不该再发写文件的请求');

  // 开关与保存结果一致：关掉时保存出来不带标识，打开时带上
  assert.equal(app.resolve('backupJobFromEditor')().marker, '');
  toggle.checked = true;
  markerInput.value = 'tad-abc123';
  assert.equal(app.resolve('backupJobFromEditor')().marker, 'tad-abc123');

  // 关掉再打开：沿用原来那一份，不重新生成（否则同一块盘上别的任务会突然对不上）
  toggle.checked = false;
  markerInput.value = 'tad-keepme';
  await app.click('backup-job-marker-write', 'change');
  toggle.checked = true;
  await app.click('backup-job-marker-write', 'change');
  assert.equal(JSON.parse(markerRequests().pop().init.body).job.marker, 'tad-keepme', '关掉再打开要沿用原标识');

  // 换源介质：原标识作废（不能把 A 盘的标识写进 B 盘）
  app.resolve('setBackupEditorLocation')('source', { kind: 'path', path: '/usb2' });
  assert.equal(markerInput.value, '');
  assert.equal(toggle.checked, false, '换源后开关要关掉');
  assert.match(app.element('backup-job-message').textContent, /原标识作废/);

  // 开着开关却拿不到标识内容（写入没成功，比如换源之后又手动勾上）时不许保存
  markerInput.value = '';
  toggle.checked = true;
  const draft = { id: 'j1', name: '任务', source: { kind: 'path', path: '/usb' }, target: { kind: 'path', path: '/vol1/1000/bak' }, mode: 'copy', marker: '' };
  assert.equal(app.resolve('validateBackupJobDraft')(draft), false, '开关开着但标识为空要拦住');
  assert.match(app.element('backup-job-message').textContent, /标识还没写进源介质/);
  assert.equal(app.resolve('validateBackupJobDraft')({ ...draft, marker: 'tad-abc123' }), true);
});

test('内置存储：列表里只留一句「内置存储 · 不支持打标」，多块合并成一行', () => {
  const builtinStorageLabel = resolve('builtinStorageLabel');
  const autoDeviceName = resolve('autoDeviceName');

  assert.equal(builtinStorageLabel([]), '', '没有内置卷时不出这一行');
  assert.equal(builtinStorageLabel([{ removable: true }]), '', '只有可移动卷时不出这一行');
  assert.equal(builtinStorageLabel([{ removable: false }]), '内置存储 · 不支持打标');
  assert.equal(
    builtinStorageLabel([{ removable: false }, { removable: false }, { removable: true }]),
    '内置存储（2 块）· 不支持打标',
    '多块内置卷合并成同一行'
  );

  // 内置卷整条被简化掉，不再参与逐台命名；可移动卷的兜底顺序不变
  assert.equal(autoDeviceName({ alias: '相机卡', label: 'NO NAME' }), '相机卡');
  assert.equal(autoDeviceName({ label: 'NO NAME' }), 'NO NAME');
  assert.equal(autoDeviceName({ device: '/dev/sdc1' }), '/dev/sdc1');
  assert.equal(autoDeviceName({}), '未知设备');
});

test('自动备份：「我的文件」根目录只挡 NAS 侧目标，不挡 NAS 侧源', () => {
  const app = loadAppContext({ fetch: recordingFetch(() => ({})).fetch });
  const validate = app.resolve('validateAutoRuleDraft');
  const base = {
    id: 'rule-x', name: '搬照片', volume_sub: 'DCIM', mode: 'sync', overwrite: true, keep: 0,
  };

  // 打标卷 → NAS：fixed 是 NAS 侧目标，指向整个「我的文件」根目录必须拦下
  const dangerous = { ...base, direction: 'from_volume', fixed: { kind: 'path', path: '/vol1/1000' } };
  assert.equal(validate(dangerous), false, '打标卷 → NAS 的目标是整个「我的文件」时要拦住');
  assert.match(app.element('auto-backup-message').textContent, /我的文件/);

  // 同一个位置在「NAS → 打标卷」下是 NAS 侧的源，把整份「我的文件」备份进打标盘是合法用法
  assert.equal(
    validate({ ...dangerous, direction: 'to_volume', mode: 'archive' }), true,
    'NAS → 打标卷时该位置是源，不该被误拒'
  );
  assert.equal(
    validate({ ...dangerous, fixed: { kind: 'path', path: '/vol1/1000/备份' } }), true,
    '指向子目录时两个方向都放行'
  );
});

test('自动备份：页面重载后移动确认项要原样带回，否则按键配置再也存不上', () => {
  const app = loadAppContext({ fetch: recordingFetch(() => ({})).fetch, TextEncoder });
  app.resolve('fillGPIOInputs')({
    enabled: true,
    migrations: [],
    buttons: [],
    autos: [{
      id: 'to-disk', name: '归档到移动盘', direction: 'to_volume',
      fixed: { kind: 'path', path: '/vol1/1000/照片' },
      volume_sub: '照片', mode: 'move_archive', overwrite: true, keep: 0,
      allow_move_to_volume: true,
    }],
  });
  const rules = app.resolve('gpioAutos');
  assert.equal(rules.length, 1);
  assert.equal(rules[0].allow_move_to_volume, true, '确认项必须保留，丢了就会提交不带它的 autos 被后端拒绝');

  // 老规则没有这个字段时回填成明确的 false，而不是 undefined
  app.resolve('fillGPIOInputs')({
    autos: [{
      id: 'old', name: '老规则', direction: 'from_volume',
      fixed: { kind: 'path', path: '/vol1/1000/导入' },
      volume_sub: '', mode: 'archive', overwrite: true, keep: 0,
    }],
  });
  assert.equal(app.resolve('gpioAutos')[0].allow_move_to_volume, false);
});

test('「保存按钮控制」先把自动备份编辑器里的草稿收进来，再一并提交', async () => {
  const radios = ['archive', 'sync', 'move_archive', 'move_overwrite'].map((value) => {
    const radio = listeningStub();
    radio.value = value;
    radio.checked = value === 'archive';
    return radio;
  });
  const { requests, fetch } = recordingFetch(() => ({ fan: {}, gpio: {} }));
  const app = loadAppContext({
    fetch,
    querySelector: (selector) => {
      if (selector.includes('auto-backup-direction')) return { value: 'to_volume', checked: true };
      if (selector.includes('auto-backup-mode')) {
        if (selector.includes(':checked')) return radios.find((radio) => radio.checked) || null;
        const match = selector.match(/value="([^"]+)"/);
        return match ? radios.find((radio) => radio.value === match[1]) : null;
      }
      return stubElement();
    },
    querySelectorAll: (selector) => (selector.includes('auto-backup-mode') ? radios : []),
  });
  const saves = () => requests.filter((item) => item.url.includes('api/config/gpio'));

  // 编辑器开着但名称还空着：保存要被拦住，不能把这条草稿悄悄丢掉
  app.element('auto-backup-editor').hidden = false;
  app.element('auto-backup-name').value = '';
  await app.click('save-gpio');
  assert.equal(saves().length, 0, '自动备份草稿不合法时不该提交按键控制');

  // 名称补齐、NAS 侧位置选好之后，同样的点击走完整保存，草稿进入 autos
  app.element('auto-backup-name').value = '归档到移动盘';
  app.resolve('autoEditorLocations').fixed = { kind: 'path', path: '/vol1/1000/照片' };
  await app.click('save-gpio');
  assert.equal(saves().length, 1, '补齐后应当保存按键控制');
  const payload = JSON.parse(saves()[0].init.body);
  assert.ok(
    Array.isArray(payload.autos) && payload.autos.some((rule) => rule.name === '归档到移动盘'),
    '编辑器草稿要落进提交的 autos'
  );
});
