// 桌面窗口 config 透传回归(luodaoyi review #40 第 6 条):
// AppWindow 构造函数必须把 DSM 传入的身份 config(appInstance/id/title 等)
// 合并透传给 SYNO.SDS.AppWindow 父类——此前 constructor() 丢弃入参,
// 真机表现为点开出现两个窗口(占位窗口永远转圈 + iframe 窗口另立条目)。
// 用最小 Ext mock 隔离加载 synology/app/tadmodule.js,不依赖 DSM 桌面。
'use strict';
const assert = require('assert');
const fs = require('fs');
const path = require('path');

const src = fs.readFileSync(
  path.join(__dirname, '..', 'synology', 'app', 'tadmodule.js'), 'utf8');

const registry = {};
global.Ext = {
  ns: () => {},
  // Ext.apply(target, source):source 覆盖 target——与 Ext 语义一致,
  // 这正是"DSM 传入字段必须能覆盖默认值"的机制本体。
  apply(target, source) {
    for (const k of Object.keys(source || {})) target[k] = source[k];
    return target;
  },
  define(name, def) {
    // Ext4 的 extend 是按名字面量("SYNO.SDS.AppInstance"),逐段从全局解析
    // ——顺带验证源码里的基类引用没有写错。
    const base = String(def.extend).split('.').reduce(
      (obj, key) => (obj == null ? obj : obj[key]), global);
    if (typeof base !== 'function') {
      throw new Error(`extend 引用无法解析: ${def.extend}`);
    }
    function Class(config) {
      Object.defineProperty(this, 'callParent', {
        value: (args) => { base.apply(this, args); },
      });
      if (def.constructor) def.constructor.call(this, config);
    }
    Class.prototype = Object.create(base.prototype);
    // def 的普通属性(appWindowName/appInstance 等)挂原型,同 Ext4。
    for (const k of Object.keys(def)) {
      if (k !== 'extend' && k !== 'constructor') Class.prototype[k] = def[k];
    }
    registry[name] = Class;
    return Class;
  },
};
global.SYNO = {
  SDS: {
    AppInstance: function () { this.baseConstructed = true; },
    AppWindow: function (config) { this.baseReceived = config; },
  },
};

new Function('Ext', 'SYNO', src)(global.Ext, global.SYNO);

const NS = 'com.wuddc.packages.tadmodule';
const AppInstance = registry[`${NS}.AppInstance`];
const AppWindow = registry[`${NS}.AppWindow`];

assert.ok(AppInstance, 'AppInstance 已按类名注册(ClassManager 语义)');
assert.ok(AppWindow, 'AppWindow 已按类名注册(Ext.create 可解析)');
assert.strictEqual(AppInstance.prototype.appWindowName, `${NS}.AppWindow`,
  'appWindowName 指向已注册的窗口类');

const fakeInstance = { appId: `${NS}.AppInstance` };
const win = new AppWindow({
  appInstance: fakeInstance,
  id: 'sds-window-1',
  title: 'DSM 传入标题',
});
assert.strictEqual(win.appInstance, fakeInstance, 'appInstance 绑定保留(身份不丢)');
assert.strictEqual(win.baseReceived.id, 'sds-window-1', 'DSM 身份字段透传父类');
assert.strictEqual(win.baseReceived.title, 'DSM 传入标题', 'DSM 传入值覆盖默认值');
assert.strictEqual(win.baseReceived.width, 980, '默认尺寸保留');
assert.strictEqual(win.baseReceived.items[0].autoEl.tag, 'iframe', 'iframe 项保留');
assert.strictEqual(win.baseReceived.items[0].autoEl.src, '/webman/3rdparty/tad-module/',
  'iframe 指向反代入口');

const inst = new AppInstance();
assert.strictEqual(inst.baseConstructed, true, 'AppInstance 正常完成父类构造');

console.log('tadmodule 窗口 config 透传:全部断言通过');
