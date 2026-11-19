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

// 可记录属性/子节点/事件的元素桩：渲染路径选择器列表的用例专用，
// 能断言 tabIndex、role、aria-selected、classList，并模拟 click / keydown 触发
function recordingElement(tag = 'li') {
  const store = { checked: false, value: '', textContent: '', children: [], attrs: new Map() };
  const classes = new Set();
  const listeners = new Map();
  const classList = {
    add: (...names) => names.forEach((name) => classes.add(name)),
    remove: (...names) => names.forEach((name) => classes.delete(name)),
    toggle: (name, force) => {
      const on = force === undefined ? !classes.has(name) : Boolean(force);
      if (on) classes.add(name);
      else classes.delete(name);
      return on;
    },
    contains: (name) => classes.has(name),
  };
  const dispatch = (type, event = {}) => {
    const handler = listeners.get(type);
    if (handler) handler({ preventDefault() {}, stopPropagation() {}, ...event });
  };
  return new Proxy(function stub() {}, {
    get(_target, prop) {
      if (prop === Symbol.toPrimitive || prop === 'toString') return () => store.textContent || tag;
      if (prop === 'classList') return classList;
      if (prop === 'children') return store.children;
      if (prop === 'append') return (...kids) => { store.children.push(...kids); };
      if (prop === 'replaceChildren') return () => { store.children.length = 0; };
      if (prop === 'setAttribute') return (name, val) => { store.attrs.set(String(name), String(val)); };
      if (prop === 'getAttribute') return (name) => (store.attrs.has(String(name)) ? store.attrs.get(String(name)) : null);
      if (prop === 'addEventListener') return (type, handler) => { listeners.set(String(type), handler); };
      if (prop === 'click') return () => dispatch('click');
      if (prop === 'keydown') return (event) => dispatch('keydown', event);
      if (prop === 'listeners') return listeners;
      if (prop in store) return store[prop];
      return stubElement();
    },
    set(_target, prop, value) {
      if (prop === 'className') {
        classes.clear();
        String(value).split(/\s+/).filter(Boolean).forEach((name) => classes.add(name));
      }
      store[prop] = value;
      return true;
    },
  });
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
    ...(overrides.FormData ? { FormData: overrides.FormData } : {}),
    setInterval: overrides.setInterval || (() => 0),
    clearInterval: overrides.clearInterval || (() => {}),
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
      getElementById: (id) => (overrides.getElementById ? overrides.getElementById(id, element) : element(id)),
      createElement: overrides.createElement || (() => stubElement()),
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

// request/postForm 助手契约：响应体损坏（截断 JSON、网关改写、空响应）时结果不确定，
// 必须抛错，不得静默当成成功空对象（评审：不确定时不得当成功）。
test('request 助手：2xx 但响应体不是合法 JSON 时抛错，不得当成成功', async () => {
  const fetchImpl = async (url) => {
    if (String(url).includes('api/probe-bad-json')) {
      return { ok: true, status: 200, json: () => Promise.reject(new SyntaxError('Unexpected end of JSON input')) };
    }
    return new Promise(() => {}); // 其余初始请求挂起，不干扰断言
  };
  const ctx = loadAppContext({ fetch: fetchImpl });
  const requestFn = ctx.resolve('request');
  await assert.rejects(() => requestFn('api/probe-bad-json'), /响应体解析失败.*200/);
});

test('request 助手：HTTP 失败透传 body.error，响应体损坏回退状态码', async () => {
  const fetchImpl = async (url) => {
    const href = String(url);
    if (href.includes('api/probe-err-bad')) {
      return { ok: false, status: 502, json: () => Promise.reject(new SyntaxError('bad gateway')) };
    }
    if (href.includes('api/probe-err')) {
      return { ok: false, status: 500, json: async () => ({ error: '磁盘不可用' }) };
    }
    return new Promise(() => {});
  };
  const ctx = loadAppContext({ fetch: fetchImpl });
  const requestFn = ctx.resolve('request');
  await assert.rejects(() => requestFn('api/probe-err'), /磁盘不可用/);
  await assert.rejects(() => requestFn('api/probe-err-bad'), /HTTP 502/);
});

test('postForm 助手：2xx 但响应体损坏时抛错（刷写类入口不得误判成功）', async () => {
  const fetchImpl = async (url) => {
    if (String(url).includes('api/probe-form')) {
      return { ok: true, status: 200, json: () => Promise.reject(new SyntaxError('truncated')) };
    }
    return new Promise(() => {});
  };
  const ctx = loadAppContext({ fetch: fetchImpl });
  const postFormFn = ctx.resolve('postForm');
  await assert.rejects(() => postFormFn('api/probe-form', {}), /响应体解析失败.*200/);
});

test('盘位覆盖层标签：告警/未知透传真实活动，读数缺失如实报未读取', () => {
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
  // 告警且繁忙/休眠是可达组合（后端先记录活动再叠加告警），不得伪装成「未使用」
  assert.equal(storageSlotStatusLabel({ state: 'warning', activity: 'busy' }), '繁忙');
  assert.equal(storageSlotStatusLabel({ state: 'warning', activity: 'sleeping' }), '休眠');
  // 告警/未知但没有活动读数：不能推导成未使用，如实显示未读取
  assert.equal(storageSlotStatusLabel({ state: 'warning' }), '未读取');
  assert.equal(storageSlotStatusLabel({ state: 'unknown' }), '未读取');
  assert.equal(storageSlotStatusLabel({ state: 'used', kind: 'front', temperature_c: 56, activity: 'idle' }), '空闲');
});

test('盘位覆盖层标签：used 无活动读数不伪装健康词', () => {
  const storageSlotStatusLabel = resolve('storageSlotStatusLabel');
  assert.equal(storageSlotStatusLabel({ state: 'used', activity: 'unknown' }), '未知');
  assert.equal(storageSlotStatusLabel({ state: 'used' }), '');
});

test('盘位高温阈值：前置 55°C、M.2 70°C 各自生效', () => {
  const isStorageHot = resolve('isStorageHot');
  assert.equal(isStorageHot({ kind: 'front', temperature_c: 55 }), true);
  assert.equal(isStorageHot({ kind: 'm2', temperature_c: 70 }), true);
  assert.equal(isStorageHot({ kind: 'front', temperature_c: 54.9 }), false);
  assert.equal(isStorageHot({ kind: 'm2', temperature_c: 69.9 }), false);
});

test('盘位健康行：缺失 health 如实报未读取，高温 > 告警 > health 优先级正确', () => {
  const storageHealthLabel = resolve('storageHealthLabel');
  // 后端没给健康结果（smartctl 缺失等）：不得推断成正常
  assert.equal(storageHealthLabel({ state: 'used' }), '未读取');
  assert.equal(storageHealthLabel({ state: 'used', health: '' }), '未读取');
  // 明确正常 / 明确告警文本透传
  assert.equal(storageHealthLabel({ state: 'used', health: '正常' }), '正常');
  assert.equal(storageHealthLabel({ state: 'used', health: '坏道告警' }), '坏道告警');
  // warning / state=warning 优先于 health 字段
  assert.equal(storageHealthLabel({ state: 'warning', health: '正常' }), '告警');
  assert.equal(storageHealthLabel({ state: 'used', warning: 'SMART 告警', health: '正常' }), '告警');
  // 高温最高优先：压过告警与 health
  assert.equal(storageHealthLabel({ kind: 'front', temperature_c: 55, warning: 'x', health: '正常' }), '高温');
  assert.equal(storageHealthLabel({ kind: 'm2', temperature_c: 70, health: '正常' }), '高温');
  // 未达阈值不误报高温，回落到 health 字段
  assert.equal(storageHealthLabel({ kind: 'front', temperature_c: 54.9, health: '正常' }), '正常');
  assert.equal(storageHealthLabel({ kind: 'm2', temperature_c: 69.9, health: '正常' }), '正常');
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

test('预设曲线模式：静音/平衡/性能三套系统集成参数都合法且互不相同', async () => {
  const app = loadAppContext();
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';
  const curveEditors = app.resolve('curveEditors');
  const kinds = Array.from(app.resolve('CURVE_KINDS'));
  const applyCurvePreset = app.resolve('applyCurvePreset');

  const snapshot = () => kinds.reduce((acc, kind) => {
    acc[kind] = curveEditors[kind].curve.map((point) => `${point.temp_c}/${point.pwm_percent}`).join(',');
    return acc;
  }, {});

  // 套预设要过网页二次确认：点「确定」再等 promise
  async function apply(mode) {
    const pending = applyCurvePreset(mode);
    await app.click('confirm-ok');
    await pending;
  }

  for (const mode of ['quiet', 'balanced', 'performance']) {
    await apply(mode);
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

  // 三档必须是三套不同参数：静音起步 40%，性能一上来就是 47%
  await apply('quiet');
  const quiet = snapshot();
  await apply('performance');
  const performance = snapshot();
  assert.notEqual(quiet.cpu, performance.cpu, '静音与性能不能是同一套曲线');
  assert.match(quiet.cpu, /^20\/40,/, '静音档从最低转速起步');
  assert.match(performance.cpu, /^20\/47,/, '性能档一上来就给更高转速');
  // 节点数与用户给的系统参数一致：HDD 8 点、NVMe 4 点
  await apply('balanced');
  assert.equal(curveEditors.hdd.curve.length, 8, 'HDD 平衡档应为 8 个节点');
  assert.equal(curveEditors.nvme.curve.length, 4, 'NVMe 平衡档应为 4 个节点');
  assert.equal(curveEditors.cpu.curve.length, 8, 'CPU 平衡档应为 8 个节点');
});

test('曲线导出：文件头带全局参数与版本信息，导出全部只收已保存的按键', async () => {
  const store = new Map();
  const localStorage = {
    getItem: (key) => (store.has(key) ? store.get(key) : null),
    setItem: (key, value) => { store.set(key, String(value)); },
    removeItem: (key) => { store.delete(key); },
  };
  const app = loadAppContext({ localStorage });
  app.element('fan-min').value = '45';
  app.element('fan-emergency').value = '90';
  app.element('fan-poll').value = '3';

  const header = app.resolve('curvePayloadHeader')();
  assert.equal(header.app, 'tad-module');
  assert.equal(header.kind, 'fan-curve-slots');
  assert.equal(header.version, 2);
  assert.match(header.exported_at, /^\d{4}-\d{2}-\d{2}T/);
  assert.equal(header.min_pwm_percent, 45);
  assert.equal(header.emergency_temp_c, 90);
  assert.equal(header.poll_seconds, 3);

  // 一开始三个键都空：导出全部收集不到东西（跨 realm 数组只比长度）
  assert.equal(app.resolve('savedCurveSlots')().length, 0);

  // 存进按键 2 后，只收按键 2（不带空按键、不带当前编辑器曲线）
  app.resolve('openCurveSlotDialog')(2);
  const pending = app.click('curve-slot-save');
  await app.click('confirm-ok');
  await pending;
  const slots = app.resolve('savedCurveSlots')();
  assert.equal(slots.length, 1);
  assert.equal(slots[0].slot, 2);
  assert.equal(slots[0].name, '自定义 2');
  assert.ok(slots[0].curves.hdd.length >= 2, '导出的曲线要带完整节点');
});

test('自定义曲线按键：点开弹窗不写存储，保存/应用/改名各键独立', async () => {
  const store = new Map();
  const localStorage = {
    getItem: (key) => (store.has(key) ? store.get(key) : null),
    setItem: (key, value) => { store.set(key, String(value)); },
    removeItem: (key) => { store.delete(key); },
  };
  const app = loadAppContext({ localStorage });
  app.element('fan-min').value = '40';
  const curveEditors = app.resolve('curveEditors');
  const stored = () => JSON.parse(store.get('tad-fan-curve-slots') || '{}');

  // 空按键：点一下只开弹窗，不写存储、不动曲线
  const before = curveEditors.cpu.curve.length;
  app.click('curve-custom-1');
  assert.equal(store.has('tad-fan-curve-slots'), false, '空按键不该写本地存储');
  assert.equal(curveEditors.cpu.curve.length, before, '空按键不该改动当前曲线');
  assert.equal(app.element('curve-slot-title').textContent, '自定义 1');

  // 先套「平衡」再存，确保存进去的是有内容的 8 点曲线
  const pendingPreset = app.resolve('applyCurvePreset')('balanced');
  await app.click('confirm-ok');
  await pendingPreset;

  // 保存：当前三条曲线进按键 1（要过二次确认）
  const pendingSave = app.click('curve-slot-save');
  await app.click('confirm-ok');
  await pendingSave;
  const saved = stored()['1'];
  assert.ok(saved, '按键 1 应被写入');
  assert.equal(saved.name, '自定义 1');
  assert.equal(saved.curves.cpu.length, 8);
  assert.equal(saved.curves.hdd.length, 8);
  assert.equal(saved.curves.nvme.length, 4);
  assert.match(app.element('curve-custom-1').title, /打开「自定义 1」/);

  // 应用：把按键里的节点填回编辑器（弹窗关掉后要重新点开按键）
  curveEditors.cpu.curve = [{ temp_c: 20, pwm_percent: 30 }, { temp_c: 95, pwm_percent: 100 }];
  app.click('curve-custom-1');
  const pendingApply = app.click('curve-slot-apply');
  await app.click('confirm-ok');
  await pendingApply;
  assert.equal(curveEditors.cpu.curve.length, 8, '应用应把按键里的 8 个节点放回来');
  assert.equal(curveEditors.cpu.curve[0].pwm_percent, saved.curves.cpu[0].pwm_percent);
  assert.equal(curveEditors.cpu.curve[0].temp_c, saved.curves.cpu[0].temp_c);

  // 按键之间互不干扰
  assert.match(app.element('curve-custom-2').title, /还是空的/);

  // 改名：只改按键 1 的名字，曲线内容不动
  app.click('curve-custom-1');
  const pendingRename = app.click('curve-slot-rename');
  app.element('confirm-input').value = '夏天用';
  await app.click('confirm-ok');
  await pendingRename;
  assert.equal(stored()['1'].name, '夏天用');
  assert.equal(stored()['1'].curves.cpu.length, 8);
  assert.equal(app.element('curve-custom-1').textContent, '夏天用');
  assert.equal(store.has('tad-fan-curve-slots'), true);
  assert.equal(Object.keys(stored()).length, 1, '改名不该凭空造出别的按键');

  // 坏数据（节点数不足）整键作废，不污染曲线编辑器
  const broken = loadAppContext({
    localStorage: {
      getItem: () => JSON.stringify({ 1: { cpu: [{ temp_c: 40, pwm_percent: 40 }], hdd: [], nvme: [] } }),
      setItem() {}, removeItem() {},
    },
  });
  assert.equal(Object.keys(broken.resolve('curveSlots')).length, 0, '缺节点的按键应被丢弃');
  assert.match(broken.element('curve-custom-1').title, /还是空的/);
});

test('自定义曲线导入：认得新格式与旧单曲线文件，坏文件给出原因', () => {
  const app = loadAppContext();
  const parse = app.resolve('parseCurveSlotFile');
  const curves = {
    hdd: [{ temp_c: 20, pwm_percent: 40 }, { temp_c: 50, pwm_percent: 100 }],
    nvme: [{ temp_c: 20, pwm_percent: 40 }, { temp_c: 55, pwm_percent: 60 }],
    cpu: [{ temp_c: 20, pwm_percent: 40 }, { temp_c: 80, pwm_percent: 100 }],
  };

  const fresh = parse(JSON.stringify({ kind: 'fan-curve-slots', version: 2, slots: [{ slot: 1, name: '夜猫子', curves }] }));
  assert.equal(fresh.name, '夜猫子');
  assert.equal(fresh.count, 1);
  assert.equal(fresh.curves.hdd.length, 2);

  // 旧版单曲线文件没有 slots，也能导入（名字留空＝按键名字不变）
  const legacy = parse(JSON.stringify({ kind: 'fan-curves', version: 1, curves }));
  assert.equal(legacy.name, '');
  assert.equal(legacy.curves.cpu[1].pwm_percent, 100);

  assert.match(parse('{ not json').error, /JSON/);
  assert.match(parse(JSON.stringify({ kind: 'fan-curves', curves: { hdd: [], nvme: [], cpu: [] } })).error, /曲线/);
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
  const cancelledApp = loadAppContext({ fetch: cancelled.fetch });
  const pendingCancel = cancelledApp.click('history-clear');
  // 弹窗要等补冲刷的请求回来才打开：先让出事件循环，再点取消
  await new Promise((resolve) => setImmediate(resolve));
  await cancelledApp.click('confirm-cancel');
  await pendingCancel;
  assert.ok(
    cancelled.requests.some((req) => req.url.includes('api/history/archive')),
    '点击瞬间就应补冲刷（取消确认也发）',
  );
  assert.equal(cancelled.requests.filter((req) => req.url.includes('api/history/clear')).length, 0, '取消确认时不应发清空请求');

  const { requests, fetch } = recordingFetch((href) => (href.includes('api/history/archive') ? { ok: true, flushed: true } : href.includes('api/history/clear') ? { ok: true } : { samples: [] }));
  const app = loadAppContext({ fetch });
  const pending = app.click('history-clear');
  await new Promise((resolve) => setImmediate(resolve));
  await app.click('confirm-ok');
  await pending;
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
  const { requests, fetch } = recordingFetch((href) => (href.includes('api/history/archive') ? { ok: false, error: 'HDD 不可写' } : href.includes('api/history/clear') ? { ok: true } : { samples: [] }));
  const app = loadAppContext({ fetch });
  const pending = app.click('history-clear');
  await new Promise((resolve) => setImmediate(resolve));
  // 网页弹窗（不是原生 confirm）：文案写在 confirm-message 里
  assert.match(app.element('confirm-message').textContent, /冲刷失败/, '弹窗应明示冲刷失败与数据丢失后果');
  await app.click('confirm-ok');
  await pending;
  assert.equal(requests.filter((req) => req.url.includes('api/history/clear')).length, 1, '用户确认后仍应清空');
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

test('风扇曲线：转速 0% 的点照样画（关停档不能整段消失）', () => {
  const historyGroupSeries = resolve('historyGroupSeries');
  const historySeriesPoints = resolve('historySeriesPoints');
  const pick = (sample) => sample.fans[0].pwm_percent;
  const samples = [
    { ts: 0, fans: [{ id: 'it8613:fan1', rpm: 1200, pwm_percent: 45 }] },
    { ts: 60, fans: [{ id: 'it8613:fan1', rpm: 0, pwm_percent: 0 }] },
    { ts: 120, fans: [{ id: 'it8613:fan1', rpm: 0, pwm_percent: 0 }] },
  ];
  // 温度类仍然把 0 当缺失，风扇组把 0 当正常读数
  assert.equal(historySeriesPoints(samples, () => 0).length, 0);
  assert.equal(historySeriesPoints(samples, pick, true).length, 3);

  const series = historyGroupSeries('fan', samples, 60);
  assert.equal(series.length, 1, '风扇组要有线');
  const values = series[0].segments.flat().map((point) => point.value);
  assert.equal(values[0], 45);
  assert.equal(values[values.length - 1], 0, '停转之后的 0% 不能被丢掉');

  // 整段都是 0（一直停在关停档）：通道也不能被当成空通道过滤掉
  assert.equal(historyGroupSeries('fan', samples.slice(1), 60).length, 1);
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
  // 清空:取消不发（网页弹窗，点「取消」）
  const cancelled = loadAppContext({ fetch: recordingFetch(() => ({})).fetch });
  const pendingCancel = cancelled.click('runlog-clear');
  await cancelled.click('confirm-cancel');
  await pendingCancel;
  assert.equal(requests.filter((req) => req.url.includes('api/log/clear')).length, 0, '取消确认不应清空');
  // 确认后清空
  const pendingClear = click('runlog-clear');
  await click('confirm-ok');
  await pendingClear;
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

test('历史曲线默认显隐：七组全开，CPU/SATA/NVMe 只画「最高」聚合线', () => {
  const defaultHistoryGroupEnabled = resolve('defaultHistoryGroupEnabled');
  const defaultHistoryChildSelection = resolve('defaultHistoryChildSelection');
  for (const key of ['cpu', 'sata', 'nvme', 'gpu', 'nic', 'other', 'fan']) {
    assert.equal(defaultHistoryGroupEnabled(key), true, `${key} 默认应展示`);
  }
  // 温度组默认只勾「最高」聚合项
  for (const key of ['cpu', 'sata', 'nvme']) {
    const selection = defaultHistoryChildSelection(key);
    assert.equal(selection.size, 1, `${key} 默认只勾最高`);
    assert.equal(selection.has('__agg__'), true, `${key} 默认勾 __agg__`);
  }
  // GPU 与风扇默认全部显示
  assert.equal(defaultHistoryChildSelection('gpu'), null, 'GPU 默认全部显示');
  assert.equal(defaultHistoryChildSelection('fan'), null, '风扇默认全部显示');
  // 网卡 / 其它默认只勾最关心的两个传感器
  assert.equal([...defaultHistoryChildSelection('nic')].join(','), 'mlx5:sensor0,mlx5:Module1');
  assert.equal([...defaultHistoryChildSelection('other')].join(','), 'spd5118:temp1,it8613:temp2');
  // 聚合项文案：CPU最高 / SATA最高 / NVMe最高
  const labels = Array.from(resolve('HISTORY_GROUPS')).slice(0, 3).map((group) => group.aggregateLabel);
  assert.equal(labels.join('|'), 'CPU最高|SATA最高|NVMe最高');
});

test('历史曲线显隐：未记录走默认，显式记录优先', () => {
  const historySeriesEnabledFor = resolve('historySeriesEnabledFor');
  const historyChildSelectionFor = resolve('historyChildSelectionFor');
  // 未记录：默认（七组全开、温度组只勾聚合）；用 nvme 验证——共享 vm 状态
  // 里 sata 已被前序测试写过显式值
  assert.equal(historySeriesEnabledFor('nvme'), true);
  assert.equal(historySeriesEnabledFor('gpu'), true, 'GPU 现在默认打开');
  const nvmeDefault = historyChildSelectionFor('nvme');
  assert.equal(nvmeDefault.has('__agg__'), true);
  // 网卡默认只勾两条
  assert.equal(historyChildSelectionFor('nic').size, 2);
  // 显式记录：优先于默认
  const setHistoryGroupEnabled = resolve('setHistoryGroupEnabled');
  setHistoryGroupEnabled('gpu', true);
  assert.equal(historySeriesEnabledFor('gpu'), true);
  setHistoryGroupEnabled('nvme', false); // 关组连带清空子类勾选（原设计）
  assert.equal(historySeriesEnabledFor('nvme'), false);
  const cleared = historyChildSelectionFor('nvme');
  assert.equal(cleared.size, 0, '关组后子类勾选应为空集合');
  // 默认勾的传感器一个都不在这台机器上时退回「全部显示」，别让整组空着
  const historyEffectiveSelection = resolve('historyEffectiveSelection');
  assert.equal(historyEffectiveSelection('nic', ['mlx5:sensor0', 'mlx5:Module1', 'r8169:temp1']).size, 2);
  assert.equal(historyEffectiveSelection('nic', ['r8169:temp1']), null);
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

test('浏览对话框位置列表：剩余容量属于第一行（右对齐），第二行只留身份信息', () => {
  const app = loadAppContext();
  const meta = app.resolve('placeMetaText');
  const place = { subtitle: '可移动介质', total_bytes: 31124881408, free_bytes: 31017926656 };
  // 容量不再混进第二行小字：它要在名称那一行右对齐，参考硬盘测试工具的选目录列表
  assert.equal(meta(place), '可移动介质', '第二行不应包含「剩余 …」容量');
  assert.equal(meta({ ...place, read_only: true }), '可移动介质 · 只读');
  assert.equal(meta({ subtitle: '远程挂载', read_only: true, total_bytes: 1 }), '远程挂载 · 只读');
  assert.equal(meta({}), '', '什么都没有时就空着');
});

test('浏览对话框位置列表：位置行可聚焦，Enter/空格 与点击等效', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/backup/browse')) {
      return { mode: 'places', scope: 'places', path: '/vol1/1000', places: [], entries: [], volumes: [] };
    }
    return {};
  });
  const placesList = recordingElement('ul');
  const app = loadAppContext({
    fetch,
    createElement: () => recordingElement(),
    getElementById: (id, fallback) => (id === 'path-picker-places' ? placesList : fallback(id)),
  });
  app.resolve('renderPathPlaces')([{ id: 'v1', name: '卷1', path: '/vol1', home: '/vol1/1000', kind: 'volume' }], '');

  const li = placesList.children[0];
  assert.equal(li.tabIndex, 0, '位置行应可键盘聚焦');
  li.keydown({ key: 'Tab', target: li });
  assert.equal(requests.filter((item) => item.url.includes('api/backup/browse')).length, 0, '其他按键不应触发进入');
  li.keydown({ key: 'Enter', target: li });
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(
    requests.some((item) => item.url.includes(`api/backup/browse?path=${encodeURIComponent('/vol1/1000')}&scope=places`)),
    'Enter 应等效于点击，进入卷内目录',
  );
});

test('浏览对话框目录列表：行是 option 可聚焦，Enter 选中文件并同步 aria-selected', () => {
  const { fetch } = recordingFetch(() => ({}));
  const entriesList = recordingElement('ul');
  const app = loadAppContext({
    fetch,
    createElement: () => recordingElement(),
    getElementById: (id, fallback) => (id === 'path-picker-entries' ? entriesList : fallback(id)),
    // selectPathFile 靠这个选择器清旧高亮，桩里按 classList 现算
    querySelectorAll: (selector) => (
      selector === '.path-entry.selected' ? entriesList.children.filter((row) => row.classList.contains('selected')) : []
    ),
  });
  app.resolve('renderPathEntries')({
    mode: 'browse',
    path: '/vol1/1000',
    entries: [
      { name: 'a.txt', path: '/vol1/1000/a.txt', is_dir: false, size: 12 },
      { name: 'b.txt', path: '/vol1/1000/b.txt', is_dir: false, size: 34 },
    ],
  }, false);

  const [rowA, rowB] = entriesList.children;
  assert.equal(rowA.tabIndex, 0, '目录行应可键盘聚焦');
  assert.equal(rowA.getAttribute('role'), 'option');
  assert.equal(rowA.getAttribute('aria-selected'), 'false', '初始未选中');

  rowA.keydown({ key: 'Enter' });
  assert.equal(app.resolve('pathPicker').selectedFile, '/vol1/1000/a.txt', 'Enter 应选中文件');
  assert.equal(rowA.getAttribute('aria-selected'), 'true');
  assert.ok(rowA.classList.contains('selected'));

  rowB.keydown({ key: ' ' });
  assert.equal(app.resolve('pathPicker').selectedFile, '/vol1/1000/b.txt', '空格应等效于点击选中');
  assert.equal(rowA.getAttribute('aria-selected'), 'false', '旧选中行要同步清掉 aria-selected');
  assert.ok(!rowA.classList.contains('selected'), '旧选中行要清掉高亮 class');
  assert.equal(rowB.getAttribute('aria-selected'), 'true');
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

test('自动备份：卷内子目录填 . / ./ / ././ 等同于整块盘，保存会被拦住', () => {
  const app = loadAppContext({ fetch: recordingFetch(() => ({})).fetch });
  const validate = app.resolve('validateAutoRuleDraft');
  const base = {
    id: 'to-disk', name: '同步到移动盘', direction: 'to_volume',
    fixed: { kind: 'path', path: '/vol1/1000/照片' },
    volume_sub: '.', mode: 'sync', overwrite: true, keep: 0,
  };

  for (const sub of ['.', './', '././', ' . ']) {
    assert.equal(validate({ ...base, volume_sub: sub }), false, `同步模式子目录 ${JSON.stringify(sub)} 必须被拦住`);
    assert.match(app.element('auto-backup-message').textContent, /整块盘/, '要说明会清空整块盘');
  }
  assert.equal(validate({ ...base, volume_sub: '' }), false, '同步模式留空同样会清空整块盘');
  // 非同步模式：点目录等于把规则作用到整卷，与「填了子目录」的预期不符
  for (const mode of ['archive', 'copy']) {
    assert.equal(validate({ ...base, mode, volume_sub: './' }), false, `${mode} 模式也要拦住 ./`);
    assert.match(app.element('auto-backup-message').textContent, /根目录/);
  }
  // 正常子目录、以及非同步模式的留空（整卷参与）放行
  assert.equal(validate({ ...base, volume_sub: '照片/2026' }), true);
  assert.equal(validate({ ...base, mode: 'archive', volume_sub: '' }), true);
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

// ---- 曲线模式：静音/平衡/性能/临时/全速/关停/手动 ---------------------------
const nextTick = () => new Promise((resolve) => setImmediate(resolve));

function curvePercents(app) {
  const editors = app.resolve('curveEditors');
  return Array.from(app.resolve('CURVE_KINDS'))
    .map((kind) => editors[kind].curve.map((point) => point.pwm_percent));
}

test('曲线模式：全速一次确认拉满 100%，关停必须两次确认才归零', async () => {
  const { fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';

  const full = app.resolve('applyFullSpeed')();
  await nextTick();
  await app.click('confirm-ok');
  await full;
  curvePercents(app).forEach((list) => list.forEach((pwm) => assert.equal(pwm, 100, '全速档所有节点都该是 100%')));
  assert.equal(app.resolve('activeCurveMode'), 'full');

  const off = app.resolve('applyShutdown')();
  await nextTick();
  await app.click('confirm-ok');
  await nextTick();
  assert.equal(app.resolve('fanOffMode'), false, '第一次确认不得直接关停');
  assert.ok(curvePercents(app).every((list) => list.every((pwm) => pwm === 100)), '第二次确认前曲线不能被改动');

  // 第二次确认只剩一个红色动作「关停并保存风扇控制」：主按钮被藏起来
  assert.equal(app.element('confirm-ok').hidden, true, '关停弹窗不该再有「确认关停」主按钮');
  await app.click('confirm-secondary');
  await off;
  curvePercents(app).forEach((list) => list.forEach((pwm) => assert.equal(pwm, 0, '关停档所有节点都该是 0%')));
  assert.equal(app.resolve('activeCurveMode'), 'off');
  assert.equal(app.resolve('fanOffMode'), true, '关停档要允许 0% 通过曲线校验');
  // 关停的优先级高于最低转速：下限降到 0
  assert.equal(app.resolve('fanMinimumPWM')(), 0);
});

test('关停二次确认：写明 5 分钟上限后自动恢复原曲线，超温仍只蜂鸣报警', async () => {
  const app = loadAppContext();
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';

  const off = app.resolve('applyShutdown')();
  await nextTick();
  await app.click('confirm-ok');
  await nextTick();
  const text = app.element('confirm-message').textContent;
  assert.match(text, /5 分钟/, '关停有时长上限，必须在弹窗里写明');
  assert.match(text, /自动恢复原选定曲线/, '到期要自动恢复用户原曲线，不能让人以为会永久停转');
  assert.match(text, /蜂鸣器/, '超温只蜂鸣报警，必须写明');
  assert.doesNotMatch(text, /自动满速|满速兜底/, '不能再承诺超温自动拉满');
  await app.click('confirm-cancel');
  await off;
  assert.equal(app.resolve('fanOffMode'), false, '取消后不得进入关停档');
});

test('关停档文案一致：页面说明、0 转提示都指向「停转 + 蜂鸣报警」', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  assert.match(html, /「关停」档除外：维修期间一律停转，超过紧急温度只蜂鸣报警/, '页面说明要与后端维修安全策略一致');
  const app = loadAppContext();
  const notice = app.resolve('FAN_OFF_RPM_NOTICE');
  assert.match(notice, /不会自动起转/, '0 转提示不能让用户以为超温会自动起转');
  assert.match(notice, /蜂鸣/, '0 转提示要说明改用蜂鸣报警');
});

test('曲线模式：关停在第一次确认点取消，曲线与模式都不变', async () => {
  const app = loadAppContext();
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';
  const before = JSON.stringify(curvePercents(app));

  const off = app.resolve('applyShutdown')();
  await nextTick();
  await app.click('confirm-cancel');
  await off;
  assert.equal(JSON.stringify(curvePercents(app)), before, '取消后曲线必须原样');
  assert.equal(app.resolve('fanOffMode'), false);
});

test('曲线模式：手动填百分比生成同值平线，越界输入被拦下', async () => {
  const app = loadAppContext();
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';

  const rejected = app.resolve('applyManualSpeed')();
  await nextTick();
  app.element('confirm-input').value = '20'; // 低于 30% 下限
  await app.click('confirm-ok');
  await rejected;
  assert.equal(app.resolve('activeCurveMode'), 'temp', '越界不该切换模式');

  const applied = app.resolve('applyManualSpeed')();
  await nextTick();
  app.element('confirm-input').value = '55';
  await app.click('confirm-ok');
  await applied;
  curvePercents(app).forEach((list) => list.forEach((pwm) => assert.equal(pwm, 55, '三条曲线应全部为 55%')));
  assert.equal(app.resolve('activeCurveMode'), 'manual');
});

test('曲线模式：「临时」回到进入全速前的曲线', async () => {
  const app = loadAppContext();
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';
  const before = JSON.stringify(curvePercents(app));

  const full = app.resolve('applyFullSpeed')();
  await nextTick();
  await app.click('confirm-ok');
  await full;
  assert.notEqual(JSON.stringify(curvePercents(app)), before);

  const temp = app.resolve('applyTempMode')();
  await nextTick();
  await app.click('confirm-ok');
  await temp;
  assert.equal(JSON.stringify(curvePercents(app)), before, '临时档要还原进入全速前的曲线');
  assert.equal(app.resolve('activeCurveMode'), 'temp');
});

test('应用并保存风扇控制：副按钮直接下发 config/fan，关停带 off_mode', async () => {
  const { requests, fetch } = recordingFetch((href) => (href.includes('api/config/fan') ? {} : {}));
  const app = loadAppContext({ fetch });
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';

  const pending = app.resolve('applyShutdown')();
  await nextTick();
  await app.click('confirm-ok');
  await nextTick();
  await app.click('confirm-secondary');
  await pending;

  const post = requests.find((req) => req.url.includes('api/config/fan'));
  assert.ok(post, '点「关停并保存风扇控制」应直接提交风扇配置');
  const body = JSON.parse(post.init.body);
  assert.equal(body.off_mode, true, '关停档要带 off_mode 让后端绕过最低转速');
  assert.ok(body.curve.every((point) => point.pwm_percent === 0));
  assert.ok(body.hdd_curve.every((point) => point.pwm_percent === 0));
  assert.ok(body.nvme_curve.every((point) => point.pwm_percent === 0));
});

test('应用并保存风扇控制：预设弹窗的副按钮同样直接下发，且不带 off_mode', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';

  const pending = app.resolve('applyCurvePreset')('performance');
  await nextTick();
  await app.click('confirm-secondary');
  await pending;

  const post = requests.find((req) => req.url.includes('api/config/fan'));
  assert.ok(post, '「应用并保存风扇控制」要提交配置');
  const body = JSON.parse(post.init.body);
  assert.equal(body.off_mode, false);
  assert.equal(body.curve[0].pwm_percent, 47, '性能档 CPU 第一档是 47%');
  assert.equal(app.resolve('activeCurveMode'), 'performance');
});

test('关停档：0 转提示改成「因为设置曲线模式为关停」，不再往接线上带', () => {
  const app = loadAppContext();
  const notice = app.resolve('FAN_OFF_RPM_NOTICE');
  const stopped = { driver_detected: true, active: true, off_mode: true, fans: [{ id: 'it8613:fan1', rpm: 0, pwm_percent: 0 }] };
  const issues = app.resolve('healthIssues')({ supported: true }, stopped, {}, {}).join('\n');
  assert.match(issues, /因为设置曲线模式为关停，风扇转速反馈为 0 RPM/);
  assert.doesNotMatch(issues, /请检查风扇接线/);

  app.resolve('renderFanHardwareWarning')(stopped);
  assert.equal(app.element('fan-warning-description').textContent, notice);
  assert.equal(app.element('fan-warning-title').textContent, '风扇已按「关停」档停转');

  // 没开关停档时还是接线/驱动提示
  const normal = { driver_detected: true, active: true, fans: [{ id: 'it8613:fan1', rpm: 0, pwm_percent: 0 }] };
  const normalIssues = app.resolve('healthIssues')({ supported: true }, normal, {}, {}).join('\n');
  assert.match(normalIssues, /请检查风扇接线或驱动状态/);
  app.resolve('renderFanHardwareWarning')(normal);
  assert.equal(app.element('fan-warning-description').textContent, '已发现风扇通道，请检查单风扇接线、BIOS 模式或驱动状态。');
});

test('自定义按键：应用并保存风扇控制一点即下发（不用再回主界面点保存）', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  // 首屏那次 refresh() 会比用例先落地：等它跑完再动手，否则它会把档位重置成「临时」
  await nextTick();
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';

  // 先把当前曲线存进按键 1
  app.click('curve-custom-1');
  const pendingSave = app.click('curve-slot-save');
  await app.click('confirm-ok');
  await pendingSave;

  // 弹窗里新增的绿底按钮：应用 + 直接下发，不用回主界面再点保存
  app.click('curve-custom-1');
  const pendingApply = app.click('curve-slot-apply-save');
  await pendingApply;
  const post = requests.find((req) => req.url.includes('api/config/fan'));
  assert.ok(post, '「应用并保存风扇控制」要直接提交风扇配置');
  const body = JSON.parse(post.init.body);
  assert.ok(body.curve.length >= 2, '下发的 CPU 曲线要带完整节点');
  assert.equal(body.off_mode, false);
  assert.equal(app.resolve('activeCurveMode'), 'slot-1');
});

test('改名：留空不报错，自动填回「自定义 1/2/3」', async () => {
  const store = new Map();
  const localStorage = {
    getItem: (key) => (store.has(key) ? store.get(key) : null),
    setItem: (key, value) => { store.set(key, String(value)); },
    removeItem: (key) => { store.delete(key); },
  };
  const app = loadAppContext({ localStorage });
  app.click('curve-custom-2');

  // 先改成一个真名字
  let pending = app.click('curve-slot-rename');
  await nextTick();
  app.element('confirm-input').value = '静音老曲线';
  await app.click('confirm-ok');
  await pending;
  assert.equal(app.element('curve-slot-title').textContent, '静音老曲线');

  // 再留空改名：自动回落默认名，不报错
  pending = app.click('curve-slot-rename');
  await nextTick();
  app.element('confirm-input').value = '   ';
  await app.click('confirm-ok');
  await pending;
  assert.equal(app.element('curve-slot-title').textContent, '自定义 2');
  assert.equal(JSON.parse(store.get('tad-fan-curve-slots'))['2'].name, '自定义 2', '留空要落成默认名');
});

test('曲线模式按键：页面里十个按键的先后顺序固定为 静音/平衡/性能/临时/全速/关停/手动/自定义 1-3', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const start = html.indexOf('class="curve-modes"');
  const end = html.indexOf('class="curve-editors"');
  assert.ok(start > 0 && end > start, 'index.html 里应有 curve-modes 区块');
  const block = html.slice(start, end);
  const ids = [...block.matchAll(/<button id="([^"]+)"/g)].map((match) => match[1]);
  assert.deepEqual(ids, [
    'curve-mode-quiet',
    'curve-mode-balanced',
    'curve-mode-performance',
    'curve-mode-temp',
    'curve-mode-full',
    'curve-mode-off',
    'curve-mode-manual',
    'curve-custom-1',
    'curve-custom-2',
    'curve-custom-3',
  ], '模式按键顺序与分组要一次固定下来，导出按钮已移除，按键组里只剩十个模式键');
});

test('页面：导出所有的按键已删除，自定义弹窗里改叫「应用到曲线」并新增绿底「应用并保存风扇控制」', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  assert.equal(html.includes('curve-export'), false, '整页不该再有「导出所有自定义曲线参数」按键');
  assert.match(html, /id="curve-slot-apply"[^>]*>应用到曲线</);
  assert.match(html, /id="curve-slot-apply-save"[^>]*class="confirm-save"[^>]*>应用并保存风扇控制</);
});

test('曲线模式选中态：切到全速后只有「全速」带 active', () => {
  const buttons = ['quiet', 'balanced', 'performance', 'temp', 'full', 'off', 'manual'].map((mode) => {
    const classes = new Set();
    return {
      dataset: { curveMode: mode },
      classes,
      classList: {
        toggle: (name, on) => { if (on) classes.add(name); else classes.delete(name); },
      },
      setAttribute() {},
      addEventListener() {},
    };
  });
  const app = loadAppContext({
    querySelectorAll: (selector) => (selector === '[data-curve-mode]' ? buttons : []),
  });
  const setCurveMode = app.resolve('setCurveMode');
  setCurveMode('full');
  assert.deepEqual(buttons.filter((button) => button.classes.has('active')).map((button) => button.dataset.curveMode), ['full']);
  setCurveMode('off');
  assert.deepEqual(buttons.filter((button) => button.classes.has('active')).map((button) => button.dataset.curveMode), ['off']);
  assert.equal(app.resolve('fanOffMode'), true, '关停档同时要放开 0% 的曲线下限');
});

test('模式反推：手动恒定值识别为 manual，拖过节点落回 temp，关停优先', () => {
  const app = loadAppContext();
  const detect = app.resolve('detectCurveMode');
  const flat = (pwm) => [{ temp_c: 20, pwm_percent: pwm }, { temp_c: 85, pwm_percent: pwm }];
  assert.equal(detect({ curve: flat(55), hdd_curve: flat(55), nvme_curve: flat(55) }), 'manual');
  assert.equal(detect({ curve: flat(100), hdd_curve: flat(100), nvme_curve: flat(100) }), 'full');
  assert.equal(detect({ curve: flat(55), hdd_curve: flat(60), nvme_curve: flat(55) }), 'temp');
  assert.equal(detect({ off_mode: true, curve: flat(55), hdd_curve: flat(55), nvme_curve: flat(55) }), 'off');
});

test('硬盘面板：移动设备区块在槽位图之后、表格之前，带「刷新 SD 卡」按键', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  assert.match(html, /id="storage-mobile-list"/);
  assert.match(html, /id="storage-refresh-sdcard"[^>]*>刷新 SD 卡</);
  const visualAt = html.indexOf('id="storage-visual"');
  const mobileAt = html.indexOf('id="storage-mobile"');
  const tableAt = html.indexOf('<tbody id="storage-body">');
  assert.ok(visualAt > 0 && mobileAt > visualAt && tableAt > mobileAt, '移动设备区块应紧跟在槽位图之后');
});

test('硬盘面板刷新 SD 卡：走与按键映射同一个接口，结果回填到面板', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/gpio/refresh-sdcard')) return { summary: '已重新识别 1 个 USB 读卡器（2-1.4）' };
    return {};
  });
  const app = loadAppContext({ fetch });
  await app.click('storage-refresh-sdcard');
  const call = requests.find((item) => item.url.includes('api/gpio/refresh-sdcard'));
  assert.ok(call, '应发出刷新 SD 卡请求');
  assert.equal(call.init.method, 'POST');
  assert.match(app.element('storage-mobile-status').textContent, /已重新识别/);
});

test('移动设备图标：SD 卡与 U 盘各画一套，未知类型按 U 盘处理', () => {
  const app = loadAppContext();
  const icon = app.resolve('mobileDeviceIcon');
  assert.ok(icon('sd'));
  assert.ok(icon('usb'));
  assert.ok(icon('harddisk'), '未知类型也要给出图标，不能是 undefined');
});

test('刷新 SD 卡：没读到介质时面板保持静默，按键映射页回到自己的说明', async () => {
  const { fetch } = recordingFetch((href) => (href.includes('api/gpio/refresh-sdcard') ? { summary: '' } : {}));
  const app = loadAppContext({ fetch });
  await app.click('storage-refresh-sdcard');
  // 面板里不再出现「没有读到存储介质…」这类提示，状态行留空
  assert.equal(app.element('storage-mobile-status').textContent, '');
  // 按键映射页那一行带着原来说明文字，空结果时用它兜底
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  assert.match(html, /id="gpio-sdcard-status"[^>]*data-idle="[^"]+"/);
});

test('风扇模式弹窗：绿色「应用并保存风扇控制」排在最右边的按钮位', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const start = html.indexOf('<footer class="confirm-actions">');
  const actionBar = html.slice(start, html.indexOf('</footer>', start));
  const order = ['confirm-cancel', 'confirm-ok', 'confirm-secondary'].map((id) => actionBar.indexOf(`id="${id}"`));
  assert.ok(order.every((index) => index >= 0), '三个按钮都要在确认弹窗里');
  assert.ok(order[0] < order[1] && order[1] < order[2], '顺序应为 取消 → 主按钮 → 绿色保存按钮');
  // 自定义曲线弹窗的绿底按钮同样贴右（foot 行 flex-end）
  assert.ok(html.indexOf('class="curve-slot-foot"') > 0, '自定义曲线弹窗要有 foot 行');
});

test('移动设备渲染：没有设备与多设备两种情况都不报错', () => {
  const app = loadAppContext();
  const render = app.resolve('renderStorageMobile');
  render({});
  render({
    mobile: [
      { device: '/dev/sdf', name: 'SanDisk Extreme', kind: 'sd', kind_label: 'SD 卡', size_label: '59.5 GiB' },
      { device: '/dev/sdg', name: 'Kingston', kind: 'usb', kind_label: 'U 盘', size_label: '28.9 GiB', mount_point: '/vol02/USB' },
    ],
  });
});

test('按键映射：开机/关机提示音开关排在声光反馈下方，缺省为开并随保存提交', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const feedbackAt = html.indexOf('id="gpio-feedback"');
  const chimeAt = html.indexOf('id="gpio-power-chime"');
  assert.ok(feedbackAt > 0 && chimeAt > feedbackAt, '开关应排在「按键触发功能声光提示」下方');

  // fillGPIOInputs 会走一遍脚本编辑器收尾（renderBashHighlight 用 TextEncoder 算
  // 字节数），浏览器里它一直存在，这里按需注入。
  const app = loadAppContext({ TextEncoder });
  app.resolve('fillGPIOInputs')({ enabled: true });
  assert.equal(app.element('gpio-power-chime').checked, true, '老配置没有该字段时应按默认开启显示');
  app.element('gpio-power-chime').checked = false;
  assert.equal(app.resolve('gpioConfigFromInputs')().power_chime, false, '关掉后必须随保存请求下发，否则后端按缺省重新打开');
});

test('关停档报警临时静音：仅关停档可用，切换即提交 api/fan/alarm-mute', async () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const offAt = html.indexOf('id="curve-mode-off"');
  const muteAt = html.indexOf('id="fan-alarm-mute"');
  assert.ok(offAt > 0 && muteAt > offAt, '静音开关应放在曲线模式（关停）附近');

  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });

  app.resolve('syncFanAlarmMute')({ off_mode: true, alarm_muted: true });
  assert.equal(app.element('fan-alarm-mute').checked, true, '静音状态要按后端 status 回填');
  assert.equal(app.element('fan-alarm-mute').disabled, false, '关停档下要能操作');
  assert.match(app.element('fan-alarm-mute-status').textContent, /已临时静音/);

  // 非关停档没有 20 秒报警可静音：开关置灰，静音状态必然是解除
  app.resolve('syncFanAlarmMute')({ off_mode: false });
  assert.equal(app.element('fan-alarm-mute').disabled, true);
  assert.equal(app.element('fan-alarm-mute').checked, false);
  assert.match(app.element('fan-alarm-mute-status').textContent, /仅「关停」档下有效/);

  app.resolve('syncFanAlarmMute')({ off_mode: true });
  app.element('fan-alarm-mute').checked = true;
  await app.click('fan-alarm-mute', 'change');
  const post = requests.find((req) => req.url.includes('api/fan/alarm-mute'));
  assert.ok(post, '切换开关应调用专用静音接口');
  assert.equal(post.init.method, 'POST');
  assert.equal(JSON.parse(post.init.body).muted, true);
});

// 构造最小可用 status：只需 config.gpio 等字段齐全，其余渲染路径在 DOM 桩下为空操作。
function gpioStatus(gpio = {}) {
  return {
    version: '0.10.56',
    cpu_model: 'Intel N100',
    supported: true,
    profile: {},
    config: {
      enabled: true,
      pl1_w: 6,
      pl2_w: 15,
      reapply_seconds: 300,
      fan: {},
      gpio: {
        version: 1,
        enabled: true,
        storage_notify: true,
        feedback: true,
        power_chime: true,
        migrations: [],
        scripts: [],
        backups: [],
        autos: [],
        buttons: [],
        ...gpio,
      },
      history: {},
      log: {},
      ui_prefs: {},
    },
    effective_max_pl1_w: 15,
    effective_max_pl2_w: 15,
    packages: [],
    temperatures: [],
    cpu_temperature: {},
    gpu_runtime: [],
    fan_control: {},
    storage: {},
    gpio: {},
    serial: {},
    storage_notify: {},
  };
}

test('即时保存开关：只提交本次字段，不夹带其他未保存草稿', async () => {
  const { requests, fetch } = recordingFetch((href, init) => {
    if (href.includes('api/config/gpio')) {
      return gpioStatus(JSON.parse(init.body));
    }
    return gpioStatus({});
  });
  const app = loadAppContext({ fetch });
  app.resolve('render')(gpioStatus({}), true);

  // 用户改了两个开关但还没点保存：切提示音时不能把它们一起写进去。
  app.element('gpio-enabled').checked = false;
  app.element('gpio-storage-notify').checked = false;

  app.element('gpio-power-chime').checked = false;
  await app.element('gpio-power-chime').listeners.get('change')();
  let posts = requests.filter((item) => item.url.includes('api/config/gpio'));
  assert.equal(posts.length, 1);
  let body = JSON.parse(posts[0].init.body);
  assert.equal(body.power_chime, false);
  assert.equal(body.enabled, true, '不能把未保存的 enabled 草稿一起提交');
  assert.equal(body.storage_notify, true, '不能把未保存的提示音草稿一起提交');

  // 第二个开关同样只带自己这一项，并基于服务端刚确认的状态。
  app.element('gpio-feedback').checked = false;
  await app.element('gpio-feedback').listeners.get('change')();
  posts = requests.filter((item) => item.url.includes('api/config/gpio'));
  body = JSON.parse(posts[posts.length - 1].init.body);
  assert.equal(body.feedback, false);
  assert.equal(body.power_chime, false, '第二次提交要基于服务端已确认的状态');
  assert.equal(body.enabled, true);
});

test('即时保存开关：服务端拒绝时回填已确认状态，不能宣称已关闭', async () => {
  const { fetch } = recordingFetch((href) => (href.includes('api/config/gpio')
    ? { ok: false, error: '仅管理员可以修改按键配置' }
    : gpioStatus({})));
  const app = loadAppContext({ fetch });
  app.resolve('render')(gpioStatus({}), true);

  app.element('gpio-power-chime').checked = false;
  await app.element('gpio-power-chime').listeners.get('change')();
  assert.equal(app.element('gpio-power-chime').checked, true, '拒绝后要拨回已确认的开启状态');
  assert.doesNotMatch(app.element('gpio-power-chime-status').textContent, /已关闭/);
  assert.match(app.element('message-gpio').textContent, /保存失败/);
});

test('即时保存开关：请求结果不确定时回读服务端确认后再更新界面', async () => {
  const fetch = async (url) => {
    const href = String(url);
    if (href.includes('api/config/gpio')) {
      throw Object.assign(new Error('Failed to fetch'), { name: 'TypeError' });
    }
    return { ok: true, status: 200, json: async () => gpioStatus({ power_chime: false }) };
  };
  const app = loadAppContext({ fetch });
  app.resolve('render')(gpioStatus({}), true);

  app.element('gpio-power-chime').checked = false;
  await app.element('gpio-power-chime').listeners.get('change')();
  assert.equal(app.element('gpio-power-chime').checked, false, '服务端确认已关闭才显示关闭');
  assert.match(app.element('message-gpio').textContent, /不确定/);
});

test('即时保存开关：请求与回读都失败时标记状态未知', async () => {
  const fetch = async () => {
    throw Object.assign(new Error('Failed to fetch'), { name: 'TypeError' });
  };
  const app = loadAppContext({ fetch });
  app.resolve('render')(gpioStatus({}), true);

  app.element('gpio-feedback').checked = false;
  await app.element('gpio-feedback').listeners.get('change')();
  assert.match(app.element('gpio-feedback-status').textContent, /状态未知/);
});

// ---- 固件升级（BIOS / 网卡）与压力测试 ----

test('压力测试参数：一项都没勾返回 null，时长与内存按边界处理，永久测试发 0', () => {
  const app = loadAppContext();
  const options = app.resolve('stressOptionsFromForm');

  assert.equal(options(), null);

  app.element('stress-cpu').checked = true;
  let result = { ...options() };
  assert.deepEqual(result, { cpu: true, gpu: false, memory: false, duration_seconds: 300, memory_mb: 512, memory_auto: false });

  app.element('stress-duration').value = '5';
  app.element('stress-memory-mb').value = '16';
  result = { ...options() };
  assert.equal(result.duration_seconds, 10);
  assert.equal(result.memory_mb, 64);

  app.element('stress-duration').value = '9999';
  app.element('stress-memory-mb').value = '9000';
  result = { ...options() };
  assert.equal(result.duration_seconds, 3600);
  assert.equal(result.memory_mb, 9000, '内存不再有固定 4096 MB 上限');

  app.element('stress-permanent').checked = true;
  result = { ...options() };
  assert.equal(result.duration_seconds, 0, '永久测试应以 0 表示时长');
  assert.equal(result.memory_mb, 9000);

  app.element('stress-permanent').checked = false;
  app.element('stress-cpu').checked = false;
  app.element('stress-gpu').checked = true;
  assert.equal(options().gpu, true);
});

test('压力测试内存自动预留：默认不预留，勾选后禁用输入框并提交 memory_auto', () => {
  const app = loadAppContext();
  const options = app.resolve('stressOptionsFromForm');
  app.element('stress-memory').checked = true;
  app.element('stress-memory-mb').value = '512';

  assert.equal(options().memory_auto, false, '默认不预留');
  assert.equal(options().memory_mb, 512);

  app.element('stress-memory-auto').checked = true;
  app.resolve('updateStressMemoryAuto')();
  assert.equal(app.element('stress-memory-mb').disabled, true, '自动预留时不应再手填 MB');
  const auto = options();
  assert.equal(auto.memory_auto, true);
  assert.equal(auto.memory_mb, 0, '自动预留由后端按可用内存计算，前端传 0');

  app.element('stress-memory-auto').checked = false;
  app.resolve('updateStressMemoryAuto')();
  assert.equal(app.element('stress-memory-mb').disabled, false);
  assert.equal(options().memory_mb, 512);
});

test('压力测试启动：关停档先拦截，取消不启动，运行中显示告警条', async () => {
  const { requests, fetch } = recordingFetch(() => ({ config: { fan: { off_mode: true } } }));
  const app = loadAppContext({ fetch });
  const status = gpioStatus({});
  status.config.fan = { off_mode: true };
  app.resolve('render')(status, true);
  assert.equal(app.resolve('fanOffModeActive')(), true, '前置：应处于关停档');

  app.element('stress-cpu').checked = true;
  const baseline = requests.length;
  const pending = app.resolve('startStressRun')();
  await nextTick();
  assert.equal(requests.length, baseline, '拦截弹窗未处理前不得启动压测');
  await app.click('confirm-cancel');
  await pending;
  assert.equal(requests.length, baseline, '取消拦截不得启动压测');

  app.resolve('renderStressStatus')({ running: true, options: { cpu: true, duration_seconds: 0 } });
  assert.equal(app.element('stress-off-warning').hidden, false, '运行中且关停档要显示告警条');
  app.resolve('renderStressStatus')({ running: false, options: {} });
  assert.equal(app.element('stress-off-warning').hidden, true, '未运行时不提示关停档');
});

test('压力测试启动：选择恢复关停前状态后先保存风扇配置、回读确认非关停再启动', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    // 回读（含首屏刷新）返回权威状态：未处于关停档
    if (href.includes('api/status')) return { fan_control: { fans: [] }, config: { fan: {} } };
    return {};
  });
  const app = loadAppContext({ fetch });
  await nextTick(); // 首屏 refresh 落定，避免覆盖后续输入
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';
  const status = gpioStatus({});
  status.config.fan = { off_mode: true };
  app.resolve('render')(status, true);

  app.element('stress-cpu').checked = true;
  const pending = app.resolve('startStressRun')();
  await nextTick();
  await app.click('confirm-secondary');
  await pending;

  const fanPost = requests.find((item) => item.url.includes('api/config/fan'));
  assert.ok(fanPost, '恢复关停要先保存风扇配置');
  assert.equal(JSON.parse(fanPost.init.body).off_mode, false, '恢复后不再是关停档');
  const startPost = requests.find((item) => /api\/stress\/start$/.test(item.url));
  assert.ok(startPost, '回读确认非关停后要继续启动压测');
  assert.notEqual(JSON.parse(startPost.init.body).ack_off_mode, true, '恢复路径不带 ack，服务端门槛兜底竞态');
});

// 关停档压测拦截的公共前置：输入合法风扇参数并 render 一份关停档状态，
// 让 startStressRun 进入拦截弹窗分支。
function prepareStressOffMode(app) {
  app.element('fan-min').value = '40';
  app.element('fan-emergency').value = '85';
  const status = gpioStatus({});
  status.config.fan = { off_mode: true };
  app.resolve('render')(status, true);
  app.element('stress-cpu').checked = true;
}

const stressStartCount = (requests) => requests.filter((item) => /api\/stress\/start$/.test(item.url)).length;

test('压测恢复并开始：保存响应 JSON 截断 / 空响应时不得发起压测', async () => {
  // 截断：HTTP 200 但 json() 抛 SyntaxError
  const truncated = async (url, init = {}) => {
    const href = String(url);
    if (href.includes('api/config/fan') && init.method === 'POST') {
      return { ok: true, status: 200, json: () => Promise.reject(new SyntaxError('Unexpected end of JSON input')) };
    }
    return { ok: true, status: 200, json: async () => ({}) };
  };
  const requestsA = [];
  const appA = loadAppContext({
    fetch: async (url, init = {}) => {
      requestsA.push({ url: String(url), init });
      return truncated(url, init);
    },
  });
  await nextTick();
  prepareStressOffMode(appA);
  const pendingA = appA.resolve('startStressRun')();
  await nextTick();
  await appA.click('confirm-secondary');
  await pendingA;
  assert.equal(stressStartCount(requestsA), 0, '保存响应截断时不得启动压测');
  assert.match(appA.element('message-fan').textContent, /保存失败/, '保存失败原因要展示在风扇区');

  // 空响应：HTTP 200 但响应体为空（json() 同样抛错）
  const requestsB = [];
  const appB = loadAppContext({
    fetch: async (url, init = {}) => {
      const href = String(url);
      requestsB.push({ url: href, init });
      if (href.includes('api/config/fan') && init.method === 'POST') {
        return { ok: true, status: 200, json: () => Promise.reject(new SyntaxError('Unexpected end of input')) };
      }
      return { ok: true, status: 200, json: async () => ({}) };
    },
  });
  await nextTick();
  prepareStressOffMode(appB);
  const pendingB = appB.resolve('startStressRun')();
  await nextTick();
  await appB.click('confirm-secondary');
  await pendingB;
  assert.equal(stressStartCount(requestsB), 0, '保存响应为空时不得启动压测');
});

test('压测恢复并开始：保存被后端明确拒绝时不得发起压测', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/config/fan')) return { ok: false, error: '设备只读，配置未落盘' };
    return {};
  });
  const app = loadAppContext({ fetch });
  await nextTick();
  prepareStressOffMode(app);
  const pending = app.resolve('startStressRun')();
  await nextTick();
  await app.click('confirm-secondary');
  await pending;
  assert.equal(stressStartCount(requests), 0, '保存明确失败时不得启动压测');
  assert.match(app.element('message-fan').textContent, /保存失败/, '风扇区要显示保存失败原因');
});

test('压测恢复并开始：回读失败或回读仍是关停档时不得发起压测', async () => {
  // 回读失败：api/status 第 2 次（恢复后的回读）返回 500
  let statusCallsA = 0;
  const requestsA = [];
  const appA = loadAppContext({
    fetch: async (url, init = {}) => {
      const href = String(url);
      requestsA.push({ url: href, init });
      if (href.includes('api/status') && !init.method) {
        statusCallsA += 1;
        if (statusCallsA >= 2) return { ok: false, status: 500, json: async () => ({ error: '内部错误' }) };
      }
      return { ok: true, status: 200, json: async () => ({}) };
    },
  });
  await nextTick();
  prepareStressOffMode(appA);
  const pendingA = appA.resolve('startStressRun')();
  await nextTick();
  await appA.click('confirm-secondary');
  await pendingA;
  assert.ok(statusCallsA >= 2, '恢复后必须发起回读');
  assert.equal(stressStartCount(requestsA), 0, '回读失败时不得启动压测');
  assert.match(appA.element('stress-status-text').textContent, /无法回读服务端状态/, '要提示恢复结果未知');

  // 回读仍是关停档
  const requestsB = [];
  const appB = loadAppContext({
    fetch: async (url, init = {}) => {
      const href = String(url);
      requestsB.push({ url: href, init });
      if (href.includes('api/status') && !init.method) {
        return { ok: true, status: 200, json: async () => ({ fan_control: { off_mode: true, fans: [] }, config: { fan: { off_mode: true } } }) };
      }
      return { ok: true, status: 200, json: async () => ({}) };
    },
  });
  await nextTick();
  prepareStressOffMode(appB);
  const pendingB = appB.resolve('startStressRun')();
  await nextTick();
  await appB.click('confirm-secondary');
  await pendingB;
  assert.equal(stressStartCount(requestsB), 0, '回读仍是关停档时不得启动压测');
  assert.match(appB.element('stress-status-text').textContent, /仍报告风扇处于关停档/, '要提示仍在关停档');
});

test('压测恢复并开始：回读与启动之间被切回关停时，服务端门槛错误要如实展示', async () => {
  // 回读说已退出关停，但 stress/start 被服务端门槛拒绝（另一客户端又切回关停）
  const { requests, fetch } = recordingFetch((href) => {
    if (/api\/stress\/start$/.test(href)) return { ok: false, error: '风扇处于「关停」档：压测会持续升温但风扇不会起转，请先恢复风扇曲线，或确认风险后重试' };
    if (href.includes('api/status')) return { fan_control: { fans: [] }, config: { fan: {} } };
    return {};
  });
  const app = loadAppContext({ fetch });
  await nextTick();
  prepareStressOffMode(app);
  const pending = app.resolve('startStressRun')();
  await nextTick();
  await app.click('confirm-secondary');
  await pending;
  assert.equal(stressStartCount(requests), 1, '回读确认非关停后允许发起启动');
  assert.match(app.element('stress-status-text').textContent, /启动失败.*关停/, '服务端门槛拒绝要展示给用户');
  assert.equal(app.element('stress-start').disabled, false, '启动被拒后按钮要恢复可点');
});

test('压测仍然开始：明确风险确认后带 ack_off_mode 直接启动，不触发恢复', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  await nextTick();
  prepareStressOffMode(app);
  const pending = app.resolve('startStressRun')();
  await nextTick();
  await app.click('confirm-ok'); // 红色主按钮「仍然开始压测」
  await pending;
  assert.equal(requests.filter((item) => item.url.includes('api/config/fan')).length, 0, '危险直启不得先改风扇配置');
  const startPost = requests.find((item) => /api\/stress\/start$/.test(item.url));
  assert.ok(startPost, '明确确认后要发起启动');
  assert.equal(JSON.parse(startPost.init.body).ack_off_mode, true, '必须带 ack_off_mode 让服务端门槛放行');
});

test('手动升级 BIOS：默认折叠，开关可展开，在线刷写运行中自动展开', async () => {
  const app = loadAppContext();
  const body = app.element('firmware-bios-manual-body');
  const toggle = app.element('firmware-bios-manual-toggle');

  // index.html 里内容区默认带 hidden、开关默认不勾选
  app.resolve('setManualBIOSVisible')(false);
  assert.equal(toggle.checked, false, '默认不显示手动升级');
  assert.equal(body.hidden, true);

  toggle.checked = true;
  await toggle.listeners.get('change')();
  assert.equal(body.hidden, false, '打开开关要显示手动升级内容');

  toggle.checked = false;
  await toggle.listeners.get('change')();
  assert.equal(body.hidden, true, '关闭开关要重新折叠');

  // 在线刷写运行中自动展开，进度日志不能被折叠藏住
  app.resolve('renderFirmwareStatus')({ flashrom: { present: true }, fpt: {}, nic: {}, job: { kind: 'bios', state: 'running', logs: ['下载固件'] } });
  assert.equal(toggle.checked, true);
  assert.equal(body.hidden, false);
});

test('固件任务日志：空日志显示等待提示，多行按行拼接', () => {
  const firmwareLogText = resolve('firmwareLogText');
  assert.equal(firmwareLogText(null), '等待操作…');
  assert.equal(firmwareLogText({ logs: [] }), '等待操作…');
  assert.equal(firmwareLogText({ logs: ['第一步', '第二步'] }), '第一步\n第二步');
});

test('固件状态渲染：刷写工具徽标、网卡字段、目标版本与运行中按钮禁用', () => {
  const app = loadAppContext();
  const renderFirmwareStatus = app.resolve('renderFirmwareStatus');

  renderFirmwareStatus({ flashrom: { present: false }, fpt: { present: false }, nic: {}, job: { kind: 'idle', state: 'idle' } });
  assert.equal(app.element('firmware-tool-badge').textContent, '刷写工具缺失');
  assert.match(app.element('firmware-tool-badge').className, /error/);
  assert.equal(app.element('firmware-nic-model').textContent, '—');
  assert.equal(app.element('firmware-nic-target').textContent, '14.32.1900');
  assert.match(app.element('firmware-nic-hint').textContent, /尚未检测到/);
  assert.equal(app.element('firmware-bios-flash').disabled, false);

  renderFirmwareStatus({ flashrom: { present: true, path: '/usr/sbin/flashrom' }, fpt: { present: false }, nic: {}, job: {} });
  assert.equal(app.element('firmware-tool-badge').textContent, 'flashrom 可用');

  renderFirmwareStatus({
    flashrom: { present: false },
    fpt: { present: true, builtin: true, path: '/opt/tad-module/tools/fpt-linux', size: 4096 },
    nic: {}, job: {},
  });
  assert.equal(app.element('firmware-tool-badge').textContent, 'FPT 内置');

  renderFirmwareStatus({
    flashrom: { present: false },
    fpt: { present: true, path: '/data/firmware/fpt-linux', size: 2048, uploaded_at: '2026-10-22T10:00:00Z' },
    nic: {
      present: true, model: 'ConnectX-4 Lx', pci_address: '01:00.0', fw_version: '14.28.1002',
      psid: 'MT_0000000001', mft_installed: true, target_fw: '14.32.1900',
    },
    job: { kind: 'nic', state: 'running', logs: ['检测网卡', '安装 MFT'] },
  });
  assert.equal(app.element('firmware-tool-badge').textContent, '自定义 FPT');
  assert.equal(app.element('firmware-nic-model').textContent, 'ConnectX-4 Lx');
  assert.equal(app.element('firmware-nic-mft').textContent, '已安装');
  assert.equal(app.element('firmware-nic-log').textContent, '检测网卡\n安装 MFT');
  assert.match(app.element('firmware-nic-hint').textContent, /已检测到/);
  assert.equal(app.element('firmware-fpt-upload').disabled, true);
  assert.equal(app.element('firmware-bios-flash').disabled, true);
  assert.equal(app.element('firmware-nic-flash').disabled, true);
});

test('网卡操作按钮：检测到网卡才显示，未装 MFT 时出现手动安装按钮', () => {
  const app = loadAppContext();
  const renderFirmwareStatus = app.resolve('renderFirmwareStatus');

  // 未检测到网卡：三个按钮全部隐藏
  renderFirmwareStatus({ flashrom: { present: true }, fpt: {}, nic: {}, job: {} });
  assert.equal(app.element('firmware-nic-flash').hidden, true);
  assert.equal(app.element('firmware-nic-flash-force').hidden, true);
  assert.equal(app.element('firmware-nic-mft-install').hidden, true);

  // 检测到网卡但未装 MFT：显示「安装 MFT 工具」与升级按钮，提示手动安装
  renderFirmwareStatus({
    flashrom: { present: true },
    fpt: {},
    nic: { present: true, model: 'ConnectX-4 Lx', pci_address: '01:00.0', mft_installed: false, target_fw: '14.32.1900', error: '未安装 MFT 工具' },
    job: {},
  });
  assert.equal(app.element('firmware-nic-mft-install').hidden, false);
  assert.equal(app.element('firmware-nic-flash').hidden, false);
  assert.equal(app.element('firmware-nic-flash').disabled, false);
  assert.equal(app.element('firmware-nic-flash-force').hidden, true);
  assert.match(app.element('firmware-nic-hint').textContent, /安装 MFT 工具/);

  // MFT 已安装且固件一致：强制升级出现，安装 MFT 隐藏，主按钮禁用
  renderFirmwareStatus({
    flashrom: { present: true },
    fpt: {},
    nic: { present: true, model: 'ConnectX-4 Lx', fw_version: '14.32.1900', mft_installed: true, target_fw: '14.32.1900' },
    job: {},
  });
  assert.equal(app.element('firmware-nic-mft-install').hidden, true);
  assert.equal(app.element('firmware-nic-flash').hidden, false);
  assert.equal(app.element('firmware-nic-flash').disabled, true);
  assert.equal(app.element('firmware-nic-flash-force').hidden, false);
});

test('手动安装 MFT：确认对话框后才 POST install-mft', async () => {
  const { requests, fetch } = recordingFetch(() => ({ job: { kind: 'nic', state: 'running', logs: ['下载 MFT 工具'] } }));
  const app = loadAppContext({ fetch, FormData });
  const baseline = requests.length;

  const cancelled = app.resolve('installMFTTool')();
  await nextTick();
  await app.click('confirm-cancel');
  await cancelled;
  assert.equal(requests.length, baseline, '取消确认不得发请求');
  assert.match(app.element('firmware-nic-log').textContent, /已取消/);

  const pending = app.resolve('installMFTTool')();
  await nextTick();
  await app.click('confirm-ok');
  await pending;
  assert.equal(requests.length, baseline + 1);
  const sent = requests[requests.length - 1];
  assert.match(sent.url, /api\/firmware\/nic\/install-mft$/);
  assert.equal(sent.init.method, 'POST');
});

test('BIOS 刷写：双重对话框都确认后才提交 confirm=FLASH 与镜像', async () => {
  const { requests, fetch } = recordingFetch(() => ({ job: { kind: 'bios', state: 'running', logs: ['刷写中'] } }));
  const app = loadAppContext({ fetch, FormData });
  const baseline = requests.length; // 页面初始化会先拉状态，只看本用例新增的请求

  app.element('firmware-bios-file').files = [{ name: 'bios.bin' }];
  // 第一对话框（备份确认）取消：不发请求
  const cancelFirst = app.resolve('flashBIOS')();
  await nextTick();
  assert.equal(requests.length, baseline, '第一对话框确认前不得发请求');
  await app.click('confirm-cancel');
  await cancelFirst;
  assert.equal(requests.length, baseline, '取消第一对话框不得发请求');
  assert.match(app.element('firmware-bios-log').textContent, /已取消/);

  // 第一对话框确认、第二对话框（刷写最终确认）取消：同样不发请求
  const cancelSecond = app.resolve('flashBIOS')();
  await nextTick();
  await app.click('confirm-ok');
  await nextTick();
  await app.click('confirm-cancel');
  await cancelSecond;
  assert.equal(requests.length, baseline, '取消第二对话框不得发请求');

  const pending = app.resolve('flashBIOS')();
  await nextTick();
  await app.click('confirm-ok');
  await nextTick();
  await app.click('confirm-ok');
  await pending;
  assert.equal(requests.length, baseline + 1);
  const sent = requests[requests.length - 1];
  assert.match(sent.url, /api\/firmware\/bios\/flash$/);
  assert.equal(sent.init.method, 'POST');
  assert.equal(sent.init.body.get('confirm'), 'FLASH');
  assert.ok(sent.init.body.get('image'), '要带上 BIOS 镜像文件');
  assert.equal(app.element('firmware-bios-log').textContent, '刷写中');
});

test('网卡刷写：双重对话框都确认后才提交 confirm=UPGRADE', async () => {
  const { requests, fetch } = recordingFetch(() => ({ job: { kind: 'nic', state: 'running', logs: ['安装 MFT'] } }));
  const app = loadAppContext({ fetch, FormData });
  const baseline = requests.length;

  // 第一对话框（型号/断电确认）取消：不发请求
  const cancelFirst = app.resolve('flashNIC')();
  await nextTick();
  await app.click('confirm-cancel');
  await cancelFirst;
  assert.equal(requests.length, baseline, '取消第一对话框不得发请求');
  assert.match(app.element('firmware-nic-log').textContent, /已取消/);

  const pending = app.resolve('flashNIC')();
  await nextTick();
  assert.equal(requests.length, baseline, '对话框确认前不得发请求');
  await app.click('confirm-ok');
  await nextTick();
  await app.click('confirm-ok');
  await pending;
  assert.equal(requests.length, baseline + 1);
  const sent = requests[requests.length - 1];
  assert.match(sent.url, /api\/firmware\/nic\/flash$/);
  assert.equal(sent.init.body.get('confirm'), 'UPGRADE');
});

test('BIOS 在线升级状态渲染：有新版本显示徽标/更新日志并放开刷写按钮', () => {
  const app = loadAppContext();
  const renderBIOSOnline = app.resolve('renderBIOSOnline');

  renderBIOSOnline({
    model: 'N100',
    current_version: '05.27.04.10',
    current_build_date: '2026-05-13 10:20',
    candidate_zip: 'TAD6S4N10G-BIOS-N100-2026_06_25.zip',
    candidate_version: '05.27.05.10',
    candidate_build_date: '2026-06-24 21:55',
    update_available: true,
    changelog: '1. 修复风扇曲线\n2. 更新微码',
  });
  assert.equal(app.element('bios-online-model').textContent, 'N100');
  assert.equal(app.element('bios-online-current').textContent, '05.27.04.10（2026-05-13 10:20）');
  assert.equal(app.element('bios-online-latest').textContent, '05.27.05.10（2026-06-24 21:55）');
  assert.equal(app.element('bios-online-zip').textContent, 'TAD6S4N10G-BIOS-N100-2026_06_25.zip');
  assert.equal(app.element('bios-online-badge').textContent, '有新版本');
  assert.match(app.element('bios-online-badge').className, /success/);
  assert.match(app.element('bios-online-changelog').textContent, /更新日志（TAD6S4N10G-BIOS-N100-2026_06_25\.zip）：\n1\. 修复风扇曲线/);
  assert.equal(app.element('bios-online-changelog').hidden, false);
  // 有可升级候选即放开刷写（刷写本身走双重对话框确认，不再依赖勾选框）
  assert.equal(app.element('bios-online-flash').disabled, false);
});

test('BIOS 在线升级状态渲染：已是最新隐藏日志并禁用刷写，错误状态走 error 徽标', () => {
  const app = loadAppContext();
  const renderBIOSOnline = app.resolve('renderBIOSOnline');

  renderBIOSOnline({
    model: 'N100',
    current_version: '05.27.05.10',
    current_build_date: '2026-06-24 21:55',
    candidate_zip: 'TAD6S4N10G-BIOS-N100-2026_06_25.zip',
    candidate_version: '05.27.05.10',
    candidate_build_date: '2026-06-24 21:55',
    update_available: false,
  });
  // 已是最新：badge 与 hint 都保持空白，只禁用刷写按钮
  assert.equal(app.element('bios-online-badge').textContent, '');
  assert.equal(app.element('bios-online-hint').textContent, '');
  assert.equal(app.element('bios-online-changelog').hidden, true);
  assert.equal(app.element('bios-online-flash').disabled, true, '无更新时不得放开刷写');

  renderBIOSOnline({ model: '', error: '读取云端 BIOS 索引失败: 404' });
  assert.equal(app.element('bios-online-badge').textContent, '检查失败');
  assert.match(app.element('bios-online-badge').className, /error/);
  assert.match(app.element('bios-online-hint').textContent, /404/);
});

test('BIOS 在线升级检查：点击后发 POST 请求并渲染结果，刷写需双重对话框确认', async () => {
  const { requests, fetch } = recordingFetch((url) => {
    if (/bios-online\/check$/.test(url)) {
      return {
        model: 'N100',
        current_version: '05.27.04.10',
        current_build_date: '2026-05-13 10:20',
        candidate_zip: 'TAD6S4N10G-BIOS-N100-2026_06_25.zip',
        candidate_version: '05.27.05.10',
        candidate_build_date: '2026-06-24 21:55',
        update_available: true,
      };
    }
    return { job: { kind: 'bios', state: 'running', logs: ['下载固件'] } };
  });
  const app = loadAppContext({ fetch, FormData });
  const baseline = requests.length;

  await app.resolve('checkBIOSOnline')();
  // 渐进式改造后终态渲染发生在 POST .then(微任务链)里,冲刷几拍再断言
  await nextTick();
  await nextTick();
  await nextTick();
  const checkReq = requests.slice(baseline).find((item) => /bios-online\/check$/.test(item.url));
  assert.ok(checkReq, '检查要 POST bios-online/check');
  assert.equal(checkReq.init.method, 'POST');
  assert.equal(app.element('bios-online-badge').textContent, '有新版本');

  // 第一对话框（备份确认）取消：不发刷写请求
  const beforeFlash = requests.length;
  const cancelled = app.resolve('flashBIOSOnline')();
  await nextTick();
  await app.click('confirm-cancel');
  await cancelled;
  assert.equal(requests.length, beforeFlash, '取消第一对话框不得发请求');
  assert.match(app.element('bios-online-hint').textContent, /已取消刷写/);

  const pending = app.resolve('flashBIOSOnline')();
  await nextTick();
  await app.click('confirm-ok');
  await nextTick();
  await app.click('confirm-ok');
  await pending;
  const flashReq = requests.slice(beforeFlash).find((item) => /bios-online\/flash$/.test(item.url));
  assert.ok(flashReq, '刷写要 POST bios-online/flash');
  assert.equal(flashReq.init.body.get('confirm'), 'FLASH');
});

// ---- 开关机计划 ----

// 迷你元素桩：开关机计划渲染要真实记录子节点树（行/单元格/徽标），
// 通用 Proxy 桩拿不到结构。只实现渲染路径用到的 DOM 子集。
function miniEl(tag) {
  const el = {
    tagName: tag,
    children: [],
    textContent: '',
    className: '',
    hidden: false,
    disabled: false,
    checked: false,
    value: '',
    attrs: {},
    listeners: {},
    dataset: {},
    style: {},
  };
  const classSet = new Set();
  el.classList = {
    add: (...names) => names.forEach((name) => classSet.add(name)),
    remove: (...names) => names.forEach((name) => classSet.delete(name)),
    toggle: (name, force) => {
      const on = force === undefined ? !classSet.has(name) : Boolean(force);
      if (on) classSet.add(name);
      else classSet.delete(name);
      return on;
    },
    contains: (name) => classSet.has(name),
  };
  el.appendChild = (child) => { el.children.push(child); return child; };
  el.append = (...kids) => { el.children.push(...kids); };
  el.replaceChildren = (...nodes) => { el.children = nodes; };
  el.setAttribute = (name, val) => { el.attrs[String(name)] = String(val); };
  el.getAttribute = (name) => (String(name) in el.attrs ? el.attrs[String(name)] : null);
  el.addEventListener = (type, handler) => { (el.listeners[type] = el.listeners[type] || []).push(handler); };
  el.querySelectorAll = (selector) => (selector === 'tr' ? el.children.filter((node) => node.tagName === 'tr') : []);
  el.click = () => (el.listeners.click || []).forEach((handler) => handler({ preventDefault() {}, stopPropagation() {} }));
  return el;
}

const SCHEDULE_IDS = [
  'schedule-total', 'schedule-enabled', 'schedule-next', 'schedule-rows', 'schedule-empty',
  'schedule-footer', 'schedule-hint', 'schedule-add', 'schedule-edit', 'schedule-delete',
  'schedule-enable', 'schedule-disable', 'schedule-apply', 'schedule-test',
  'schedule-editor', 'schedule-editor-title', 'schedule-editor-days',
  'schedule-editor-time', 'schedule-editor-action',
];

function loadScheduleApp(overrides = {}) {
  const els = {};
  SCHEDULE_IDS.forEach((id) => { els[id] = miniEl('div'); });
  const app = loadAppContext({
    ...overrides,
    // 风扇曲线初始化要 canvas.getContext，回退到通用 Proxy 桩；其余元素走迷你桩记录结构
    createElement: (tag) => (tag === 'canvas' ? stubElement() : miniEl(tag)),
    getElementById: (id, fallback) => els[id] || fallback(id),
  });
  return { app, els };
}

const flushMicrotasks = async () => {
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
};

test('开关机计划渲染：统计卡片、计划行文案与 RTC 页脚', () => {
  const { app, els } = loadScheduleApp();
  app.resolve('renderPowerSchedule')({
    total: 2,
    enabled_count: 1,
    entries: [
      { enabled: true, days: 'weekday', time: '10:00', action: 'poweron' },
      { enabled: false, days: 'weekend', time: '02:00', action: 'poweroff' },
    ],
    next_trigger: '2026-10-24 10:00 开机',
    rtc_time: '2026-10-23 02:00:00',
    rtc_alarm_irq: 'yes',
    applied: true,
  });
  assert.equal(els['schedule-total'].textContent, '2');
  assert.equal(els['schedule-enabled'].textContent, '1');
  assert.equal(els['schedule-next'].textContent, '2026-10-24 10:00 开机');
  const rows = els['schedule-rows'].children;
  assert.equal(rows.length, 2);
  assert.equal(rows[0].children[0].children[0].checked, true, '首行复选框勾选');
  assert.equal(rows[0].children[1].textContent, '平日 10:00');
  assert.equal(rows[0].children[2].textContent, '开机');
  assert.match(rows[0].children[3].children[0].className, /schedule-status on/);
  assert.equal(rows[1].children[1].textContent, '假日 02:00');
  assert.equal(rows[1].children[2].textContent, '关机');
  assert.match(rows[1].children[3].children[0].className, /schedule-status off/);
  assert.equal(els['schedule-empty'].hidden, true, '有计划时不显示空提示');
  assert.equal(
    els['schedule-footer'].textContent,
    'RTC: 2026-10-23 02:00:00 · alarm_IRQ: yes · 服务: enabled · 计划写入: 是',
  );
  // 未选中行：编辑/删除/启停全部禁用
  assert.equal(els['schedule-edit'].disabled, true);
  assert.equal(els['schedule-delete'].disabled, true);
  assert.equal(els['schedule-enable'].disabled, true);
  assert.equal(els['schedule-disable'].disabled, true);
});

test('开关机计划交互：行选中驱动工具栏，启用按钮按选中行保存', async () => {
  const entries = [
    { enabled: true, days: 'weekday', time: '10:00', action: 'poweron' },
    { enabled: false, days: 'weekend', time: '02:00', action: 'poweroff' },
  ];
  const { requests, fetch } = recordingFetch((url) => {
    if (/power-schedule\/config$/.test(url)) return { total: 2, enabled_count: 2, entries: entries.map((e) => ({ ...e, enabled: true })), applied: true };
    return { total: 2, enabled_count: 1, entries };
  });
  const { app, els } = loadScheduleApp({ fetch });
  app.resolve('renderPowerSchedule')({ total: 2, enabled_count: 1, entries, applied: false });

  const rows = els['schedule-rows'].children;
  rows[1].click(); // 选中已停用的假日关机行
  assert.equal(rows[1].classList.contains('selected'), true);
  assert.equal(rows[0].classList.contains('selected'), false);
  assert.equal(els['schedule-edit'].disabled, false);
  assert.equal(els['schedule-enable'].disabled, false, '已停用行可启用');
  assert.equal(els['schedule-disable'].disabled, true, '已停用行不能再停用');

  await app.resolve('setPowerScheduleEnabled')(true);
  const saveReq = requests.find((item) => /power-schedule\/config$/.test(item.url));
  assert.ok(saveReq, '启用要 POST power-schedule/config');
  const body = JSON.parse(saveReq.init.body);
  assert.equal(body.entries.length, 2);
  assert.equal(body.entries[1].enabled, true);
  assert.equal(body.entries[1].days, 'weekend');
});

test('开关机计划编辑器：非法时间拦截不发请求，合法新增保存并带默认启用', async () => {
  const { requests, fetch } = recordingFetch((url) => {
    if (/power-schedule\/config$/.test(url)) {
      const sent = JSON.parse(requests[requests.length - 1].init.body);
      return { total: sent.entries.length, enabled_count: sent.entries.length, entries: sent.entries, applied: true };
    }
    return { total: 0, enabled_count: 0, entries: [] };
  });
  const { app, els } = loadScheduleApp({ fetch });
  app.resolve('renderPowerSchedule')({ total: 0, enabled_count: 0, entries: [] });
  assert.equal(els['schedule-empty'].hidden, false, '无计划显示空提示');

  app.resolve('openScheduleEditor')(-1);
  assert.equal(els['schedule-editor'].hidden, false);
  assert.equal(els['schedule-editor-title'].textContent, '新增计划');

  els['schedule-editor-time'].value = '25:00';
  app.resolve('submitScheduleEditor')();
  await flushMicrotasks();
  assert.match(els['schedule-hint'].textContent, /时间格式无效/);
  assert.equal(requests.filter((item) => /power-schedule\/config$/.test(item.url)).length, 0, '非法时间不得发保存请求');

  els['schedule-editor-days'].value = 'daily';
  els['schedule-editor-time'].value = '08:30';
  els['schedule-editor-action'].value = 'poweroff';
  app.resolve('submitScheduleEditor')();
  await flushMicrotasks();
  const saveReq = requests.find((item) => /power-schedule\/config$/.test(item.url));
  assert.ok(saveReq, '合法新增要发保存请求');
  const body = JSON.parse(saveReq.init.body);
  assert.deepEqual(body.entries, [{ enabled: true, days: 'daily', time: '08:30', action: 'poweroff' }]);
  assert.equal(els['schedule-editor'].hidden, true, '提交后编辑器要关闭');
});

test('压力测试状态渲染：指标格式、剩余时长与按钮状态', () => {
  const app = loadAppContext();
  const renderStressStatus = app.resolve('renderStressStatus');

  renderStressStatus({
    running: true,
    options: { cpu: true, gpu: true, memory: true, duration_seconds: 300, memory_mb: 512 },
    elapsed_seconds: 65,
    cpu_percent: 97.53,
    memory_used_bytes: 2 * 1024 * 1024 * 1024,
    memory_total_bytes: 16 * 1024 * 1024 * 1024,
    memory_allocated_bytes: 512 * 1024 * 1024,
    load1: 7.321,
    cpu_temp_c: 71.26,
    gpu_freq_mhz: 1100,
    gpu_backend: 'vaapi',
  });
  assert.match(app.element('stress-status-text').textContent, /剩余 235 秒/);
  assert.equal(app.element('stress-elapsed').textContent, '1 分 5 秒');
  assert.equal(app.element('stress-cpu-percent').textContent, '97.5%');
  assert.equal(app.element('stress-memory-used').textContent, '2.0 GB / 16.0 GB');
  assert.equal(app.element('stress-load').textContent, '7.32');
  assert.equal(app.element('stress-cpu-temp').textContent, '71.3 °C');
  assert.equal(app.element('stress-gpu-freq').textContent, '1100 MHz');
  assert.equal(app.element('stress-gpu-backend').textContent, 'VAAPI / ffmpeg');
  assert.match(app.element('stress-gpu-hint').textContent, /GPU 压测运行中/);
  assert.equal(app.element('stress-start').disabled, true);
  assert.equal(app.element('stress-stop').disabled, false);

  renderStressStatus({ running: true, options: { cpu: true, duration_seconds: 0 }, elapsed_seconds: 30 });
  assert.match(app.element('stress-status-text').textContent, /永久运行/);

  renderStressStatus({ running: false, options: { cpu: true, duration_seconds: 300 }, elapsed_seconds: 300 });
  assert.match(app.element('stress-status-text').textContent, /已结束/);
  assert.equal(app.element('stress-start').disabled, false);
  assert.equal(app.element('stress-stop').disabled, true);
});

test('GPU 压测异常：展示后端返回的错误信息', () => {
  const app = loadAppContext();
  app.resolve('renderStressStatus')({
    running: true,
    options: { gpu: true, duration_seconds: 0 },
    gpu_backend: 'vaapi',
    gpu_error: 'GPU 压测进程退出：VAAPI 初始化失败',
  });
  assert.match(app.element('stress-gpu-hint').textContent, /VAAPI 初始化失败/);
  assert.match(app.element('stress-gpu-hint').className, /error/);
});

test('压力测试启动：一项都没勾时不发请求并给出提示', async () => {
  const { requests, fetch } = recordingFetch(() => ({}));
  const app = loadAppContext({ fetch });
  const baseline = requests.length;
  await app.resolve('startStressRun')();
  assert.equal(requests.length, baseline);
  assert.match(app.element('stress-status-text').textContent, /至少勾选/);
});

test('切到固件页：立即拉取一次固件状态', async () => {
  const { requests, fetch } = recordingFetch(() => ({ fpt: {}, nic: {}, job: {} }));
  const app = loadAppContext({
    fetch,
    requestAnimationFrame: (callback) => { callback(); return 0; },
  });
  app.resolve('activateTab')('tab-firmware');
  await new Promise((resolveTick) => setImmediate(resolveTick));
  assert.ok(requests.some((item) => /api\/firmware$/.test(item.url)), '进入固件页要拉取 api/firmware');
});


test('BIOS 在线检查重复触发只有一个轮询，任务刷新不提前启用按钮', async () => {
  let nextTimer = 0;
  const timers = new Map();
  const app = loadAppContext({
    setInterval: (callback) => { const id = nextTimer++; timers.set(id, callback); return id; },
    clearInterval: (id) => { timers.delete(id); },
  });
  const check = app.resolve('checkBIOSOnline');
  const before = timers.size;
  await check();
  assert.equal(timers.size, before + 1);
  const first = app.resolve('biosOnlinePollTimer');
  app.resolve('renderFirmwareJob')({ kind: 'bios', state: 'idle', logs: [] });
  assert.equal(app.element('bios-online-check').disabled, true);
  await check();
  assert.equal(timers.size, before + 1, '重复点击不能遗留旧轮询');
  assert.equal(timers.has(first), false, '旧interval必须被清除');
  app.resolve('stopBIOSOnlinePoll')();
  assert.equal(timers.size, before);
  assert.equal(app.element('bios-online-check').disabled, false);
  app.resolve('renderFirmwareJob')({ kind: 'bios', state: 'running', logs: [] });
  assert.equal(app.element('bios-online-check').disabled, true);
});

test('head bootstrap scripts are same-origin and ordered around the stylesheet', () => {
  const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
  const head = html.match(/<head>([\s\S]*?)<\/head>/)[1];
  const scripts = [...head.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)];
  assert.equal(scripts.length, 2);
  const names = ['theme-boot.js', 'style-heal.js'];
  scripts.forEach((script, index) => {
    const src = script[1].match(/^ src="([a-z-]+\.js)\?v=[^"]+"$/);
    assert.equal(src?.[1], names[index]);
    assert.equal(script[2].trim(), '');
    assert.ok(fs.statSync(path.join(__dirname, names[index])).isFile());
  });
  const stylesheet = head.indexOf('<link rel="stylesheet" href="styles.css?');
  assert.ok(scripts[0].index < stylesheet && stylesheet < scripts[1].index);
});

test('external theme bootstrap keeps the parent and system theme fallbacks', () => {
  const boot = fs.readFileSync(path.join(__dirname, 'theme-boot.js'), 'utf8');
  for (const [stored, parent, system, expected] of [
    ['light', 'dark', true, 'light'],
    ['auto', 'dark', false, 'dark'],
    ['invalid', 'invalid', true, 'dark'],
    [null, 'inaccessible', false, 'light'],
  ]) {
    const document = {
      documentElement: { dataset: {} },
      querySelector() { return { setAttribute(name, value) { assert.equal(name, 'content'); this.value = value; document.color = value; } }; },
    };
    const window = {
      get parent() {
        if (parent === 'inaccessible') throw new Error('cross origin');
        return { document: { body: { getAttribute: () => parent } } };
      },
      matchMedia: () => ({ matches: system }),
    };
    vm.runInNewContext(boot, { window, document, localStorage: { getItem: () => stored } });
    assert.equal(document.documentElement.dataset.theme, expected);
    assert.equal(document.color, expected === 'dark' ? '#141519' : '#f3f4f6');
  }
});

test('external style bootstrap reloads stale CSS once and leaves healthy CSS untouched', async () => {
  const boot = fs.readFileSync(path.join(__dirname, 'style-heal.js'), 'utf8');
  for (const [css, alreadyHealed, reload] of [
    ['/* old stylesheet */', false, true],
    ['/* old stylesheet */', true, false],
    ['--chart-grid-line: #fff;', false, false],
  ]) {
    const href = 'https://host.test/webman/3rdparty/tad-module/styles.css?v=version';
    const link = { href, dataset: alreadyHealed ? { healed: '1' } : {} };
    let calls = 0;
    const fetch = async (url, options) => {
      calls++;
      assert.equal(url, href);
      assert.equal(options.cache, 'reload');
      return { text: async () => css };
    };
    vm.runInNewContext(boot, { window: { fetch }, fetch, document: { querySelector: () => link }, Date: { now: () => 123 } });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(calls, 1);
    assert.equal(link.href, reload ? href.replace('version', '123') : href);
  }
});

test('power status displays absent RAPL without replacing working readings', () => {
  const app = loadAppContext();
  const render = (status) => app.resolve('render')(status, true);
  const text = (id) => app.element(id).textContent;
  render({ supported: true, power_supported: false });
  assert.equal(text('current-pl1'), '平台不支持 RAPL');
  assert.equal(text('current-pl2'), '平台不支持 RAPL');
  assert.equal(text('health'), '运行正常');
  render({ power_supported: true, packages: [{ has_pl1: true, pl1_w: 15 }] });
  assert.equal(text('current-pl1'), '15 W');
  assert.equal(text('current-pl2'), '不可用');
  render({});
  assert.equal(text('current-pl1'), '不可用');
});
