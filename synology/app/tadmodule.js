// TAD6S4N 模块 —— DSM 桌面应用（SPM 同机制：SYNO.SDS 框架注册）。
// 窗口内容是 iframe，指向本套件经 DSM nginx 反代的页面
// /webman/3rdparty/tad-module/（与 DSM 同源，登录态自动携带，
// 后端 CSP frame-ancestors 'self' 允许同源内嵌）。
// 注意：类名命名空间用 tadmodule（JS 标识符安全），与套件名 tad-module 无关。
//
// 真机教训（DSM 7.4.1，「点开出现两个窗口」）：
// 1. AppWindow 构造函数必须接收并透传 DSM 传入的 config——Ext.apply(默认值,
//    config) 让身份字段（appInstance 绑定等）覆盖默认值。此前 constructor
//    忽略入参，窗口丢失 appInstance 绑定：DSM 启动流程给实例挂的占位窗口
//    永远停在加载态，我们的 iframe 窗口又注册成另一条任务栏项——一次点开
//    出现两个，关掉正常的占位的还在。
// 2. 类定义用 Ext.define（进 ClassManager，DSM 按 appWindowName 解析得到）；
//    Ext.extend 赋值只挂命名空间，不进类注册表。整段照抄 SPM 参照实现的写法。
Ext.ns("com.wuddc.packages.tadmodule");

Ext.define("com.wuddc.packages.tadmodule.AppInstance", {
    extend: "SYNO.SDS.AppInstance",
    appWindowName: "com.wuddc.packages.tadmodule.AppWindow",
    constructor: function () {
        this.callParent(arguments);
    }
});

Ext.define("com.wuddc.packages.tadmodule.AppWindow", {
    extend: "SYNO.SDS.AppWindow",
    appInstance: null,
    constructor: function (config) {
        config = config || {};
        this.appInstance = config.appInstance;
        config = Ext.apply({
            width: 980,
            height: 640,
            resizable: true,
            maximizable: true,
            minimizable: true,
            layout: "fit",
            bodyStyle: "padding:0",
            items: [{
                xtype: "box",
                autoEl: {
                    tag: "iframe",
                    src: "/webman/3rdparty/tad-module/",
                    frameborder: "0",
                    style: "width:100%;height:100%;border:0;display:block;background:#f3f4f6"
                }
            }]
        }, config);
        this.callParent([config]);
    }
});
