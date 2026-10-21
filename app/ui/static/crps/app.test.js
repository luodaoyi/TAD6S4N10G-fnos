'use strict';

// crps/app.js 回归测试（node:test，无第三方依赖）：vm 加载真实脚本，行为级断言。
// 覆盖复审要求：未配置页面首次渲染与 60 秒轮询均不得自动发 detect POST，
// detect 仅允许用户点击「自动识别保存」触发（扫描会改写 Pin19-22 方向并可能写 crps.json）。
// 运行：node --test app/ui/static/crps/app.test.js

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// 可记录事件监听的元素桩：value/textContent 等按元素持久保存，供交互用例读回断言
function listeningStub() {
  const store = { checked: false, value: '', textContent: '' };
  const listeners = new Map();
  const stub = new Proxy(function stub() {}, {
    get(_target, prop) {
      if (prop === Symbol.toPrimitive || prop === 'toString') return () => '';
      if (prop === 'addEventListener') return (type, handler) => { listeners.set(type, handler); };
      if (prop in store) return store[prop];
      return listeningStub();
    },
    set(_target, prop, value) {
      store[prop] = value;
      return true;
    },
    apply() {
      return listeningStub();
    },
  });
  stub.listeners = listeners;
  return stub;
}

// 记录型 fetch 桩：按 URL 返回可配置响应体，断言实际发出的请求
function recordingFetch(responseFor) {
  const requests = [];
  const fetch = async (url, init = {}) => {
    const href = String(url);
    requests.push({ url: href, init });
    return { ok: true, status: 200, json: async () => responseFor(href, init) };
  };
  return { requests, fetch };
}

// 未配置态后端响应（页面加载即处于引导态）
function unconfiguredResponses(href) {
  if (href.includes('api/crps/config')) {
    return { pins: ['Pin19', 'Pin20', 'Pin21', 'Pin22'], scl_pin: '', sda_pin: '', configured: false };
  }
  if (href.includes('api/crps/status')) {
    return { error: 'gpio-not-configured', ok: null, ts: 0 };
  }
  return {};
}

function loadCrpsContext({ fetch } = {}) {
  const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const timers = []; // setInterval 记录：测试手动推进模拟时间流逝
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id)) elements.set(id, listeningStub());
    return elements.get(id);
  };
  const matchMedia = () => ({ matches: false, addEventListener() {} });
  const sandbox = {
    console,
    setInterval: (fn) => { timers.push(fn); return timers.length; },
    clearInterval: () => {},
    setTimeout: () => 0,
    clearTimeout: () => {},
    fetch,
    navigator: { userAgent: 'node-test' },
    location: { reload() {}, href: '', pathname: '/', origin: 'http://localhost' },
    document: {
      readyState: 'complete',
      visibilityState: 'visible',
      getElementById: (id) => element(id),
      createElement: () => listeningStub(),
      createElementNS: () => listeningStub(),
      createTextNode: () => listeningStub(),
      querySelector: () => listeningStub(),
      querySelectorAll: () => [],
      addEventListener() {},
      body: listeningStub(),
      documentElement: listeningStub(),
    },
  };
  sandbox.window = {
    matchMedia,
    addEventListener() {},
    innerWidth: 1280,
    innerHeight: 800,
    devicePixelRatio: 1,
    location: sandbox.location,
  };
  sandbox.globalThis = sandbox;
  const context = vm.createContext(sandbox);
  vm.runInContext(source, context, { filename: 'crps/app.js' });
  return {
    element,
    timers,
    click: (id, type = 'click') => {
      const handler = element(id).listeners.get(type);
      return handler ? handler({ preventDefault() {}, stopPropagation() {} }) : undefined;
    },
  };
}

const tick = () => new Promise((done) => setImmediate(done));

test('未配置页面：首次渲染与 60 秒轮询均不自动发 detect POST', async () => {
  const { requests, fetch } = recordingFetch(unconfiguredResponses);
  const app = loadCrpsContext({ fetch });
  await tick(); // 等启动序列 (loadGpioConfig + 首次 poll) 的微任务落定
  assert.equal(
    requests.filter((req) => req.url.includes('/api/crps/detect')).length, 0,
    '首次渲染不得发 detect',
  );

  // 模拟 63 秒：手动推进全部已登记的 interval（3s 状态轮询 ×21 次，10s 耗电轮询同场 fires）
  for (let i = 0; i < 21; i++) {
    for (const fn of [...app.timers]) fn();
    await tick();
  }
  assert.equal(
    requests.filter((req) => req.url.includes('/api/crps/detect')).length, 0,
    '60 秒轮询周期内不得自动发 detect',
  );
  assert.ok(
    requests.some((req) => req.url.includes('api/crps/status')),
    '状态轮询本身应正常进行（页面在渲染，而非静默挂掉）',
  );
});

test('「自动识别保存」显式点击才发一次 detect POST；失败文案附带后端 msg', async () => {
  const { requests, fetch } = recordingFetch((href) => {
    if (href.includes('api/crps/detect')) {
      return { ok: false, found: false, msg: '未探测到电源接线，请检查线缆后重试' };
    }
    return unconfiguredResponses(href);
  });
  const app = loadCrpsContext({ fetch });
  await tick();
  assert.equal(
    requests.filter((req) => req.url.includes('/api/crps/detect')).length, 0,
    '点击前无 detect 请求',
  );

  app.click('gpio-detect');
  await tick();
  const detects = requests.filter((req) => req.url.includes('/api/crps/detect'));
  assert.equal(detects.length, 1, '点击恰好发一次 detect');
  assert.equal(detects[0].init.method, 'POST');

  // 失败文案 = 固定引导 + 后端 msg 附注（复审要求的可排查性）
  const msgText = app.element('gpio-msg').textContent;
  assert.match(msgText, /检测信号失败/);
  assert.match(msgText, /未探测到电源接线/, '后端 msg 应附加在固定文案后');
});


test('断线后 GPIO 提示跟随轮询状态: 可读→红字→恢复绿字', async () => {
  // 后端契约: STATUS_WORD 读回 nil 时 ok=null (通信断), 读回非零值时 ok=false 但 status_raw 有值 (电源故障)
  let statusSeq = 0;
  const { fetch } = recordingFetch((href) => {
    if (href.includes('api/crps/config')) {
      return { pins: ['Pin19', 'Pin20', 'Pin21', 'Pin22'], scl_pin: 'Pin19', sda_pin: 'Pin21', configured: true, readable: true };
    }
    if (href.includes('api/crps/status')) {
      statusSeq++;
      return statusSeq === 1
        ? { ok: true, ts: 1700000000, bus: 'gpio', addr: '0x5b', fan: 2600, fan_mode: 'auto' }
        : statusSeq === 2
          ? { ok: null, ts: 1700000000, bus: 'gpio', addr: '0x5b' } // 真正断线: STATUS_WORD 读回 nil
          : { ok: true, ts: 1700000000, bus: 'gpio', addr: '0x5b', fan: 2600, fan_mode: 'auto' };
    }
    return {};
  });
  const app = loadCrpsContext({ fetch });
  await tick();
  assert.match(app.element('gpio-msg').textContent, /当前接线可读/, '初始已配置可读 → 绿字');

  // 断线: 轮询拿到 ok=null → GPIO 提示同步红字
  for (const fn of [...app.timers]) fn();
  await tick();
  assert.match(app.element('gpio-msg').textContent, /未检测到电源/, '断线后应显示红字引导');
  assert.ok(app.element('gpio-msg').className.includes('err'), '断线提示应为 err 样式');

  // 恢复: 轮询拿到 ok=true → 回到绿字
  for (const fn of [...app.timers]) fn();
  await tick();
  assert.match(app.element('gpio-msg').textContent, /当前接线可读/, '恢复后应回到绿字');
});

test('电源故障 (ok=false+status_raw) 不得误报为断线, 应区分通信可读但状态字异常', async () => {
  const { fetch } = recordingFetch((href) => {
    if (href.includes('api/crps/config')) {
      return { pins: ['Pin19', 'Pin20', 'Pin21', 'Pin22'], scl_pin: 'Pin19', sda_pin: 'Pin21', configured: true, readable: true };
    }
    if (href.includes('api/crps/status')) {
      // STATUS_WORD 读回 0x0008 (风扇告警位), 通信链路正常但电源报告故障
      return { ok: false, status_raw: '0008', ts: 1700000000, bus: 'gpio', addr: '0x5b', fan: 4200, fan_mode: 'auto' };
    }
    return {};
  });
  const app = loadCrpsContext({ fetch });
  await tick();

  // 轮询刷新后: 通信可读 → 不应显示 "未检测到电源"
  for (const fn of [...app.timers]) fn();
  await tick();
  const msg = app.element('gpio-msg').textContent;
  assert.doesNotMatch(msg, /未检测到电源/, 'ok=false+status_raw 不得误报为断线');
  assert.match(msg, /状态字异常.*0008/, '应提示状态字异常并附原始值');
});

test('写盘失败告警优先: 轮询状态刷新不得覆盖告警文字', async () => {
  const { fetch } = recordingFetch((href, init = {}) => {
    if (href.includes('api/crps/config')) {
      if (init.method === 'POST') return { ok: true, scl_pin: 'Pin19', sda_pin: 'Pin21', msg: '写盘失败：设备只读' };
      return { pins: ['Pin19', 'Pin20', 'Pin21', 'Pin22'], scl_pin: 'Pin19', sda_pin: 'Pin21', configured: true, readable: true };
    }
    if (href.includes('api/crps/status')) return { ok: true, ts: 1700000000, bus: 'gpio', addr: '0x5b', fan: 2600, fan_mode: 'auto' };
    return {};
  });
  const app = loadCrpsContext({ fetch });
  await tick();
  app.element('scl-pin').value = 'Pin19';
  app.element('sda-pin').value = 'Pin21';
  app.click('gpio-save');
  await tick();
  assert.match(app.element('gpio-msg').textContent, /写盘失败/, '保存返回写盘告警应显示');

  // 告警存活: 后续轮询即使 ok=true 也不覆盖告警
  for (const fn of [...app.timers]) fn();
  await tick();
  assert.match(app.element('gpio-msg').textContent, /写盘失败/, '轮询状态刷新不得覆盖写盘告警');
});

test('手动模式钳制 pct≥11: 滑条 10 (固件回自动触发值) 不再原样发送', async () => {
  const { requests, fetch } = recordingFetch((href, init = {}) => {
    if (href.includes('api/crps/fan') && init.method === 'POST') return { ok: true, mode: 'manual', pct: JSON.parse(init.body).pct };
    if (href.includes('api/crps/config')) return { pins: ['Pin19', 'Pin20', 'Pin21', 'Pin22'], scl_pin: 'Pin19', sda_pin: 'Pin21', configured: true, readable: true };
    if (href.includes('api/crps/status')) return { ok: true, ts: 1700000000, bus: 'gpio', addr: '0x5b', fan: 2600, fan_mode: 'manual' };
    return {};
  });
  const app = loadCrpsContext({ fetch });
  await tick();

  // 旧缓存 HTML 组合: 滑条仍是 min=10 的值 10 —— 点手动不得发送 10 (否则后端按 auto 处理、UI 跳回自动)
  app.element('fan-slider').value = '10';
  app.click('mode-manual');
  await tick();
  const fanPosts = requests.filter((q) => q.url.includes('api/crps/fan') && q.init.method === 'POST');
  assert.equal(fanPosts.length, 1, '点击手动应恰好发一次风扇 POST');
  assert.equal(JSON.parse(fanPosts[0].init.body).pct, 11, 'pct=10 应被钳制为 11');
  assert.equal(app.element('fan-slider').value, '11', '滑条显示应同步钳制值');
  assert.equal(app.element('fan-pct').textContent, '11%', '百分比标签应同步钳制值');

  // 滑条 change 走同一钳制
  app.element('fan-slider').value = '10';
  app.element('fan-slider').listeners.get('change')();
  await tick();
  const fanPosts2 = requests.filter((q) => q.url.includes('api/crps/fan') && q.init.method === 'POST');
  assert.equal(fanPosts2.length, 2, 'change 应补发一次');
  assert.equal(JSON.parse(fanPosts2[1].init.body).pct, 11, 'change 的 pct=10 同样钳制为 11');
});
