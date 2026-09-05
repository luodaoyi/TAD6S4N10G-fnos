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

function loadAppContext() {
  const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');
  const matchMedia = () => ({ matches: false, addEventListener() {} });
  const sandbox = {
    console,
    URLSearchParams,
    setInterval: () => 0,
    clearInterval: () => {},
    setTimeout: () => 0,
    clearTimeout: () => {},
    requestAnimationFrame: () => 0,
    fetch: () => new Promise(() => {}),
    Image: function Image() {},
    ResizeObserver: class ResizeObserver {
      observe() {}
      disconnect() {}
      unobserve() {}
    },
    navigator: { userAgent: 'node-test' },
    location: { reload() {}, href: '', pathname: '/' },
    document: {
      readyState: 'complete',
      getElementById: () => stubElement(),
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
    location: sandbox.location,
  };
  sandbox.globalThis = sandbox;
  const context = vm.createContext(sandbox);
  vm.runInContext(source, context, { filename: 'app.js' });
  return (name) => vm.runInContext(name, context);
}

const resolve = loadAppContext();

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