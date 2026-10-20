// CRPS 电源监控 前端逻辑 (融合版: API 走 ../api/crps/)
(function () {
  "use strict";

  // ---- 主题引导 (与主应用同口径): CSP 禁止内联 <script>, 放这里执行 ----
  // 跟随 localStorage 'tad-theme' (dark/light/auto); auto 档优先读 fnOS
  // 父窗口 body[theme-mode] (同源内嵌时), 不可读再退 prefers-color-scheme。
  var themeMode = "auto";
  try {
    var stored = localStorage.getItem("tad-theme");
    if (stored === "dark" || stored === "light" || stored === "auto") themeMode = stored;
  } catch (e) { /* localStorage 不可用时跟随系统 */ }
  // parentThemeMode 读父窗口(fnOS 同源内嵌)的主题模式, 不可读返回 ""
  var parentThemeMode = function () {
    try {
      var p = window.parent;
      if (p === window || !p.document || !p.document.body) return "";
      var m = (p.document.body.getAttribute("theme-mode") || "").trim().toLowerCase();
      return m === "dark" || m === "light" ? m : "";
    } catch (e) { return ""; } // 父窗口跨域不可读
  };
  // applyTheme 按当前主题模式计算并应用 dark/light 到 <html data-theme>
  var applyTheme = function () {
    var theme = themeMode;
    if (themeMode === "auto") {
      theme = parentThemeMode() ||
        (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
    }
    document.documentElement.dataset.theme = theme;
  };
  applyTheme();
  window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", applyTheme);
  try {
    window.addEventListener("storage", function (e) {
      if (!e.key || e.key === "tad-theme") {
        var s = localStorage.getItem("tad-theme");
        themeMode = (s === "dark" || s === "light" || s === "auto") ? s : "auto";
        applyTheme();
      }
    });
  } catch (e) { /* 忽略 */ }

  // 图表取设计令牌 (暗色切换后随 render 重绘自动跟上)
  var cssVar = function (name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  };
  // hexToRgba 把 #RRGGBB 颜色转 rgba() 字符串 (a 为透明度)
  var hexToRgba = function (hex, a) {
    var n = parseInt(hex.replace("#", ""), 16);
    return "rgba(" + ((n >> 16) & 255) + "," + ((n >> 8) & 255) + "," + (n & 255) + "," + a + ")";
  };

  // $ getElementById 简写
  var $ = function (id) { return document.getElementById(id); };

  var METRIC_DEFS = [
    { key: "vin", label: "输入电压", unit: "V", fmt: "%.1f", w: "%.1f" },
    { key: "iin", label: "输入电流", unit: "A", fmt: "%.2f", w: "%.2f" },
    { key: "vout", label: "输出电压", unit: "V", fmt: "%.2f", w: "%.1f" },
    { key: "iout", label: "输出电流", unit: "A", fmt: "%.1f", w: "%.1f" },
    { key: "pin", label: "输入功率", unit: "W", fmt: "%.1f", w: "%.1f" },
    { key: "pf", label: "功率因数", unit: "", fmt: "%.3f", w: "%.3f" },
    { key: "pout", label: "输出功率", unit: "W", fmt: "%.1f", w: "%.1f" },
    { key: "efficiency", label: "效率", unit: "%", fmt: "%.1f", w: "%.1f" },
    { key: "temp1", label: "进风口温度", unit: "°C", fmt: "%.1f", w: "%.1f" },
    { key: "temp2", label: "出风口温度", unit: "°C", fmt: "%.1f", w: "%.1f" },
  ];

  // fmt 按 "%.Nf" 规格格式化数值; null/非数值显示 "--"
  var fmt = function (v, spec) {
    if (v === null || v === undefined) return "--";
    var num = Number(v);
    if (isNaN(num)) return "--";
    var s = spec || "%.1f";
    return s.replace("%.", ".").indexOf(".") >= 0
      ? num.toFixed(parseInt(s.replace(/%.(\d+)f/, "$1"), 10))
      : num.toFixed(0);
  };

  var grid = $("metrics");

  // buildGrid 按 METRIC_DEFS 生成参数卡片网格
  function buildGrid() {
    grid.innerHTML = "";
    METRIC_DEFS.forEach(function (m) {
      var c = document.createElement("div");
      c.className = "card";
      c.id = "m-" + m.key;
      c.innerHTML =
        '<div class="label">' + m.label + "</div>" +
        '<div class="value"><span class="num">--</span><span class="unit">' + m.unit + "</span></div>" +
        (m.key === "pf" ? '<div class="pf-bar"><div class="pf-fill" id="pf-fill"></div></div>' : "") +
        '<div class="extra"></div>';
      grid.appendChild(c);
    });
  }

  // setMetric 更新单个参数卡片的数值与附加说明
  function setMetric(key, val, spec, extra) {
    var c = $("m-" + key);
    if (!c) return;
    c.querySelector(".num").textContent = fmt(val, spec);
    if (extra !== undefined && extra !== null) {
      c.querySelector(".extra").textContent = extra;
    }
  }

  var lastOk = null;

  // showNetErr 后端不可达: 已配置时保留原内容只报 banner; 初始/未配置态内容本就隐藏, banner 例外显示
  function showNetErr(text) {
    setBanner(false, text);
    if (!document.body.classList.contains("cfg-ok")) {
      $("status-banner").classList.add("net-err"); // setBanner 重置过 className, 须在其后补
    }
  }

  // render 用一轮 /api/status 数据刷新整个页面 (参数/横幅/风扇/厂商信息)
  function render(data) {
    if (!data) {
      showNetErr("无法连接后端服务");
      return;
    }
    // 未配置/未检测到电源(没插线): 隐藏监控/耗电/风扇/电源信息, 只留 GPIO 配置区引导接线。
    // 判定: 带 error (=gpio-not-configured/no-gpio 降级) 或 ok==null (总线通但状态字都读不到, 实机没插线的常态)
    var wasOk = document.body.classList.contains("cfg-ok");
    var nowOk = !data.error && data.ok != null;
    document.body.classList.toggle("cfg-ok", nowOk);
    if (nowOk && !wasOk) pollEnergy(); // 刚显示: canvas 离开 display:none, 立即补拉耗电渲染, 不等 10s 轮询
    $("status-banner").classList.remove("net-err");
    METRIC_DEFS.forEach(function (m) {
      var v = data[m.key];
      var extra = "";
      if (m.key === "vout" && data.vout_calc && data.vout === null) {
        extra = "由 POUT/IOUT 推算";
      }
      if (m.key === "iin" && data.iin === null && data.iin_calc != null) {
        v = data.iin_calc;
        extra = "由 PIN/VIN 推算";
      }
      setMetric(m.key, v, m.fmt, extra);
    });
    // 功率因数动态条: 宽度/颜色随负载实时变化
    var pfFill = $("pf-fill");
    if (pfFill && data.pf != null) {
      pfFill.style.width = Math.min(100, Math.max(0, data.pf * 100)) + "%";
      pfFill.style.background = data.pf >= 0.95 ? "#22c55e" : data.pf >= 0.85 ? "#f59e0b" : "#ef4444";
    }
    // 风扇转速只显示在风扇控制区(避免与上方参数网格重复)
    $("fan-rpm").textContent = fmt(data.fan, "%.0f");
    if (data.fan_mode) { lastKnownMode = data.fan_mode; setModeUI(data.fan_mode); }

    var ok = data.ok === true;
    var bannerText;
    if (data.error === "gpio-not-configured") {
      bannerText = "尚未设置 GPIO 引脚，请在上方配置 PSCL / PSDA";
    } else if (data.error === "no-gpio") {
      bannerText = "未检测到电源(GPIO) 连接";
    } else if (ok) {
      bannerText = "电源状态正常";
    } else {
      bannerText = "电源异常 / 状态字 0x" + (data.status_raw || "????");
    }
    setBanner(ok, bannerText);
    $("status-banner").className = "status-banner " + (ok ? "ok" : "err");

    // 更新头部连接状态文案
    var sub;
    if (data.error === "gpio-not-configured") {
      sub = "GPIO 未配置，等待设置 PSCL/PSDA";
    } else if (data.error === "no-gpio") {
      sub = "PMBus 未检测到电源 (GPIO)";
    } else if (ok) {
      sub = "PMBus 已连接";
    } else {
      sub = "PMBus 连接异常 (0x" + (data.status_raw || "????") + ")";
    }
    $("header-sub").textContent = sub;

    if (data.mfr_id || data.mfr_model) {
      $("mfr-info").innerHTML =
        '<span class="k">厂商</span> ' + esc(data.mfr_id || "--") +
        '<br><span class="k">型号</span> ' + esc(data.mfr_model || "--");
    }
    $("ts").textContent = "更新于 " + (new Date(data.ts * 1000).toLocaleTimeString());
    lastOk = data;
  }

  // esc HTML 转义, 防注入
  function esc(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  // setBanner 设置状态横幅样式与文案
  function setBanner(ok, text) {
    var b = $("status-banner");
    b.className = "status-banner " + (ok ? "ok" : ok === false ? "err" : "");
    $("status-text").textContent = text;
  }

  // poll 拉取一次 /api/status 并渲染
  function poll() {
    fetch("../api/crps/status")
      .then(function (r) { return r.json(); })
      .then(render)
      .catch(function (e) {
        showNetErr("读取失败: " + e.message);
        $("header-sub").textContent = "PMBus 连接异常";
      });
  }

  // ---- 风扇控制 ----
  var slider = $("fan-slider");
  var pctLabel = $("fan-pct");
  var msg = $("fan-msg");
  var modeAuto = $("mode-auto");
  var modeManual = $("mode-manual");
  var manualCtl = $("manual-ctl");
  var lastSent = null;
  var lastKnownMode = null; // 最近一次轮询确认的模式 (POST 失败回滚用)
  var modeGuardUntil = 0; // 用户刚操作模式后的保护期: 期间忽略服务器 fan_mode, 避免快照滞后把按钮拉回旧值跳动

  // applyModeUI 直接应用模式按钮 UI, 不看保护期 (POST 成功/失败回滚走这里, 立即生效)
  function applyModeUI(mode) {
    modeAuto.classList.toggle("active", mode === "auto");
    modeManual.classList.toggle("active", mode === "manual");
    manualCtl.style.display = mode === "manual" ? "flex" : "none";
  }

  // setModeUI 轮询入口应用模式 UI: 保护期内忽略快照旧值
  function setModeUI(mode) {
    if (Date.now() < modeGuardUntil) return;
    applyModeUI(mode);
  }

  var fanBusy = false, fanPending = null; // 连点合并: 忙时只记最新目标, 当前请求完成后补发
  var fanPollTimer = null; // 设置成功后延后拉新转速的定时器 (连点重置)

  // sendFan 提交风扇设置 POST 并按结果更新 UI; 完成后补发连点期间记下的最新目标
  function sendFan(body, doneMsg) {
    fanBusy = true;
    msg.className = "";
    msg.textContent = "正在设置…";
    fetch("../api/crps/fan", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        fanBusy = false;
        if (d.ok) {
          msg.className = "ok";
          lastKnownMode = d.mode;
          applyModeUI(d.mode);
          msg.textContent = doneMsg(d);
          // 后端 ~1.2s 后已刷新状态快照, 立即拉一次, 转速不再等 3s 轮询周期
          clearTimeout(fanPollTimer);
          fanPollTimer = setTimeout(poll, 1300);
        } else {
          msg.className = "err";
          lastSent = null; // 失败清零: 用户重选同一占空比时会重新发送
          if (lastKnownMode) applyModeUI(lastKnownMode);
          msg.textContent = "设置失败: " + (d.error || "未知错误");
        }
        if (fanPending) { var p = fanPending; fanPending = null; sendFan(p.body, p.doneMsg); }
      })
      .catch(function (e) {
        fanBusy = false;
        msg.className = "err";
        lastSent = null; // 失败清零: 用户重选同一占空比时会重新发送
        if (lastKnownMode) applyModeUI(lastKnownMode);
        msg.textContent = "设置失败: " + e.message;
        if (fanPending) { var p2 = fanPending; fanPending = null; sendFan(p2.body, p2.doneMsg); }
      });
  }

  // requestFan 点击入口: 乐观即时切 UI; 忙时只记最新目标, 避免请求堆积变慢
  function requestFan(body, doneMsg) {
    applyModeUI(body.mode);
    if (fanBusy) {
      fanPending = { body: body, doneMsg: doneMsg };
      msg.className = "";
      msg.textContent = "正在设置…";
      return;
    }
    sendFan(body, doneMsg);
  }

  modeAuto.addEventListener("click", function () {
    modeGuardUntil = Date.now() + 5000;
    // 实测光宝固件: FAN_COMMAND_1=10% 会触发恢复自动温控(约2700转低速档), 可软切回, 无需断电。
    // 注意: 自动转速下限随温度变化(低温约2600-2700, 高温固件会钳制在更高值如≥3100),
    // 提示不写死具体转速, 避免误导。其他值(0/0xFFFF/0x00FF)实测无效。
    requestFan({ mode: "auto" }, function () {
      return "已切回电源自动温控";
    });
  });
  modeManual.addEventListener("click", function () {
    modeGuardUntil = Date.now() + 5000;
    requestFan({ mode: "manual", pct: parseInt(slider.value, 10) }, function (d) {
      return "已切换：手动 " + d.pct + "%";
    });
  });

  slider.addEventListener("input", function () {
    pctLabel.textContent = slider.value + "%";
  });

  slider.addEventListener("change", function () {
    var pct = parseInt(slider.value, 10);
    if (lastSent === pct) return;
    lastSent = pct;
    requestFan({ mode: "manual", pct: pct }, function (d) {
      return "已设置风扇占空比 " + d.pct + "%";
    });
  });

  // ---- GPIO 引脚配置 ----
  var sclSel = $("scl-pin");
  var sdaSel = $("sda-pin");
  var gpioSave = $("gpio-save");
  var gpioMsg = $("gpio-msg");
  var gpioPins = [];

  // gpioMsgShow 显示 GPIO 配置区的提示消息 (ok/err 样式)
  function gpioMsgShow(ok, text) {
    if (!gpioMsg) return;
    gpioMsg.className = "gpio-msg " + (ok ? "ok" : "err");
    gpioMsg.textContent = text;
  }

  // loadGpioConfig 拉取 GPIO 配置并重建针脚下拉 (preserveWarn 为需保留的写盘失败告警)
  function loadGpioConfig(preserveWarn) {
    if (!sclSel || !sdaSel) return;
    fetch("../api/crps/config")
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (!d || !Array.isArray(d.pins)) return;
        gpioPins = d.pins;
        var curScl = d.scl_pin, curSda = d.sda_pin;
        [sclSel, sdaSel].forEach(function (sel) {
          sel.innerHTML = "";
          gpioPins.forEach(function (p) {
            var o = document.createElement("option");
            o.value = p;
            // 动态标注: 当前设置为 PSCL/PSDA 的针脚显示对应角色
            var tag = "";
            if (p === curScl) tag = " (PSCL)";
            if (p === curSda) tag = " (PSDA)";
            o.textContent = p + tag;
            sel.appendChild(o);
          });
        });
        if (d.scl_pin && gpioPins.indexOf(d.scl_pin) >= 0) sclSel.value = d.scl_pin;
        if (d.sda_pin && gpioPins.indexOf(d.sda_pin) >= 0) sdaSel.value = d.sda_pin;
        // 保存后刷新: 写盘失败的告警须保留, 不被连接状态覆盖
        if (preserveWarn) {
          gpioMsgShow(false, preserveWarn);
        } else if (d.configured === false) {
          gpioMsgShow(false, "尚未设置 GPIO 引脚：选择 PSCL/PSDA 后点“保存并应用”开始读取");
        } else {
          gpioMsgShow(d.readable === true, d.readable === true ? "当前接线可读（已读到电源数据）" : "未检测到电源：请检查 PSCL/PSDA 接线，或点「自动识别保存」重新识别");
        }
      })
      .catch(function () { gpioMsgShow(false, "无法读取 GPIO 配置"); });
  }

  if (gpioSave) gpioSave.addEventListener("click", function () {
    var scl = sclSel.value, sda = sdaSel.value;
    if (!scl || !sda) { gpioMsgShow(false, "请选择针脚"); return; }
    if (scl === sda) { gpioMsgShow(false, "PSCL 与 PSDA 不能是同一根针"); return; }
    gpioMsgShow(true, "正在应用…");
    fetch("../api/crps/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ scl_pin: scl, sda_pin: sda })
    })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d && d.ok) {
          // msg !== "ok" 表示已应用但写盘失败 (重启后配置丢失), 刷新下拉后仍须保留该告警
          var saveWarn = (d.msg && d.msg !== "ok") ? d.msg : "";
          gpioMsgShow(true, "已应用 PSCL=" + d.scl_pin + " PSDA=" + d.sda_pin + (saveWarn ? "（" + saveWarn + "）" : "（已持久化）") + "，连接状态见上方");
          loadGpioConfig(saveWarn);   // 刷新下拉动态标注
          setTimeout(poll, 300);   // 立即刷新一次状态
        } else {
          gpioMsgShow(false, "应用失败: " + ((d && d.error) || (d && d.msg) || "未知错误"));
        }
      })
      .catch(function (e) { gpioMsgShow(false, "应用失败: " + e.message); });
  });

  // ---- 自动识别保存 PSCL/PSDA ----
  // 仅用户点击触发: detect 会对 Pin19-22 全组合做 SuperIO 扫描, 已配置但断电/拔线时
  // 若后台自动扫描, 可能在其他组合上读到数据并改写 crps.json (复审要求, 不做自动 opt-in)。
  var gpioDetect = $("gpio-detect");

  if (gpioDetect) gpioDetect.addEventListener("click", function () {
    gpioMsgShow(true, "正在自动识别 Pin19-22…（约几秒，请稍候）");
    gpioDetect.disabled = true;
    fetch("../api/crps/detect", { method: "POST" })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        gpioDetect.disabled = false;
        if (d && d.found && d.ok) {
          // 后端已自动应用+保存: found/applied/saved 分离, 直接展示结果
          if (sclSel && d.scl_pin) sclSel.value = d.scl_pin;
          if (sdaSel && d.sda_pin) sdaSel.value = d.sda_pin;
          gpioMsgShow(!!d.saved, d.msg || ("已识别 PSCL=" + d.scl_pin + " PSDA=" + d.sda_pin + "，已应用"));
          loadGpioConfig(d.saved ? "" : d.msg); // 写盘失败告警在刷新后保留
          setTimeout(poll, 300);
        } else if (d && d.found) {
          gpioMsgShow(false, (d && d.msg) || "已找到接线但应用失败，请重试。");
        } else {
          // 保留固定引导文案, 末尾附带后端 msg 便于排查 (如 I2C 超时/总线忙等细节)
          gpioMsgShow(false, "检测信号失败，请重新接线保证正确连接信号线。" + ((d && d.msg) ? "（" + d.msg + "）" : ""));
        }
      })
      .catch(function (e) {
        gpioDetect.disabled = false;
        gpioMsgShow(false, "识别失败: " + e.message);
      });
  });

  // ---- 查看连线方法(点击切换接线图弹层, 点弹层外关闭) ----
  var dlLog = $("dl-log");
  var wiringPop = $("wiring-pop");
  if (dlLog && wiringPop) {
    dlLog.addEventListener("click", function (e) {
      e.stopPropagation();
      wiringPop.classList.toggle("show");
    });
    document.addEventListener("click", function (e) {
      if (!wiringPop.contains(e.target)) wiringPop.classList.remove("show");
    });
  }

  // ---- 日志弹窗 (入口已移除: "查看日志"按钮改为"查看连线方法"点击弹接线图; 日志功能保留, 可经 /api/crps/log 访问) ----
  var logCopy = $("log-copy");
  if (logCopy) logCopy.addEventListener("click", function () {
    var body = $("log-body");
    if (!body) return;
    var txt = body.textContent;
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(txt).then(function () {
        gpioMsgShow(true, "日志已复制到剪贴板");
      }, function () { fallbackCopy(txt); });
    } else { fallbackCopy(txt); }
  });
  var logClose = $("log-close");
  if (logClose) logClose.addEventListener("click", function () {
    var mask = $("log-mask");
    if (mask) mask.style.display = "none";
  });
  // fallbackCopy 剪贴板 API 不可用时的 textarea 复制兜底
  function fallbackCopy(txt) {
    try {
      var ta = document.createElement("textarea");
      ta.value = txt; ta.style.position = "fixed"; ta.style.opacity = "0";
      document.body.appendChild(ta); ta.select();
      document.execCommand("copy"); document.body.removeChild(ta);
      gpioMsgShow(true, "日志已复制到剪贴板");
    } catch (e) { gpioMsgShow(false, "复制失败, 请长按日志文本手动复制"); }
  }

  // ---- 耗电统计 ----
  var enCv = $("energy-chart-canvas");
  var enCtx = enCv ? enCv.getContext("2d") : null;
  var enTip = $("energy-tooltip");
  var enMsg = $("energy-msg");
  var enBars = []; // 柱状图命中区域 {x,y,w,h,date,wh}

  // enVal Wh 转 kWh 显示字符串 (按量级取 3/2 位小数)
  function enVal(wh) {
    var kwh = wh / 1000;
    return kwh < 10 ? kwh.toFixed(3) : kwh.toFixed(2);
  }

  var enLastDays = null; // 最近一次按日明细 (resize 时重绘用)

  // renderEnergy 用 /api/energy 数据刷新耗电统计区 (汇总数字 + 柱状图)
  function renderEnergy(d) {
    if (!d || !d.ok) return;
    enLastDays = d.days || [];
    $("en-total").textContent = enVal(d.total_wh);
    $("en-today").textContent = enVal(d.today_wh);
    $("en-month").textContent = enVal(d.month_wh);
    $("en-power").textContent = d.pin_w != null ? Number(d.pin_w).toFixed(1) : "--";
    var since = "";
    if (d.first_ts) {
      var t = new Date(d.first_ts * 1000);
      var p2 = function (n) { return String(n).padStart(2, "0"); };
      since = "自 " + t.getFullYear() + "-" + p2(t.getMonth() + 1) + "-" + p2(t.getDate()) + " 累计";
    }
    $("en-since").textContent = since;
    $("en-session").textContent = "本次运行 " + enVal(d.session_wh || 0) + " kWh";
    if (enCtx) drawEnergyChart(d.days || []);
  }

  // pollEnergy 拉取一次 /api/energy 并渲染
  function pollEnergy() {
    fetch("../api/crps/energy")
      .then(function (r) { return r.json(); })
      .then(renderEnergy)
      .catch(function () { /* 静默, 下次再取 */ });
  }

  // drawEnergyChart 按最近 30 天明细绘制耗电柱状图 (含网格/刻度/命中区)
  function drawEnergyChart(days) {
    var dpr = window.devicePixelRatio || 1;
    var w = enCv.clientWidth, h = enCv.clientHeight || 140; // 高度读 CSS 实际值: 手机端媒体查询会加高画布
    if (!w) return; // 首帧容器未布局完成: 跳过本轮, 等 poll/resize 重绘, 避免 fallback 宽度拉伸变形
    if (enCv.width !== w * dpr || enCv.height !== h * dpr) {
      enCv.width = w * dpr;
      enCv.height = h * dpr;
    }
    enCtx.setTransform(dpr, 0, 0, dpr, 0, 0);
    enCtx.clearRect(0, 0, w, h);
    enBars = [];
    var data = days.slice(-30);
    if (!data.length) {
      enCtx.fillStyle = cssVar("--muted"); enCtx.font = "13px sans-serif"; enCtx.textAlign = "center";
      enCtx.fillText("暂无数据（每 15 秒自动累计）", w / 2, h / 2);
      return;
    }
    var padL = w < 500 ? 30 : 40, padR = 8, padT = 16, padB = 22; // 手机端收紧左边距, 给柱子让宽度
    var iw = w - padL - padR, ih = h - padT - padB;
    var maxWh = 10;
    data.forEach(function (d) { if (d.wh > maxWh) maxWh = d.wh; });
    maxWh = maxWh * 1.15;
    var slot = iw / 30;                 // 固定 30 天槽位: 柱子从左侧依次排开, 不随天数均分整幅宽度
    var bw = Math.min(44, slot * 0.6);
    // 网格 + Y 轴 kWh 刻度
    enCtx.strokeStyle = cssVar("--chart-grid"); enCtx.lineWidth = 1;
    enCtx.fillStyle = cssVar("--muted"); enCtx.font = "10px sans-serif"; enCtx.textAlign = "right";
    for (var g = 0; g <= 4; g++) {
      var gy = padT + ih * g / 4;
      enCtx.beginPath(); enCtx.moveTo(padL, gy); enCtx.lineTo(w - padR, gy); enCtx.stroke();
      var kv = maxWh * (1 - g / 4) / 1000;
      enCtx.fillText(kv < 1 ? kv.toFixed(2) : kv.toFixed(1), padL - 4, gy + 3);
    }
    enCtx.textAlign = "right";
    enCtx.fillText("kWh", w - 4, padT - 5); // 单位放右上角, 与左侧刻度数字完全错开
    enCtx.textAlign = "left";
    // 柱体 (今日=最新一根高亮)
    // 日期标签稀疏化: 槽位太窄(手机 30 天)时每 10 天标一个, 避免文字重叠
    var labelStep = slot < 12 ? 10 : Math.max(1, Math.ceil(data.length / 7));
    data.forEach(function (d, i) {
      var cx = padL + slot * (i + 0.5);
      var bh = d.wh > 0 ? Math.max(2, d.wh / maxWh * ih) : 0;
      enCtx.fillStyle = i === data.length - 1
        ? cssVar("--primary")
        : hexToRgba(cssVar("--primary") || "#3478f6", 0.35);
      if (bh > 0) enCtx.fillRect(cx - bw / 2, padT + ih - bh, bw, bh);
      enBars.push({ x: cx - bw / 2 - 3, y: padT, w: bw + 6, h: ih, date: d.date, wh: d.wh });
      if (i % labelStep === 0) {
        enCtx.fillStyle = cssVar("--muted"); enCtx.textAlign = "center";
        enCtx.fillText(d.date.slice(5), cx, padT + ih + 13);
      }
    });
  }

  if (enCv) {
    // 容器宽度变化 (窗口/iframe 缩放) 时按最新 clientWidth 重绘, 防止拉伸变形
    window.addEventListener("resize", function () {
      if (enLastDays) drawEnergyChart(enLastDays);
    });
    enCv.addEventListener("mousemove", function (ev) {
      if (!enTip) return;
      var rect = enCv.getBoundingClientRect();
      var mx = ev.clientX - rect.left, my = ev.clientY - rect.top;
      var hit = null;
      for (var i = 0; i < enBars.length; i++) {
        var b = enBars[i];
        if (mx >= b.x && mx <= b.x + b.w && my >= b.y && my <= b.y + b.h) { hit = b; break; }
      }
      if (!hit) { enTip.style.display = "none"; return; }
      enTip.textContent = hit.date + " · " + (hit.wh / 1000).toFixed(3) + " kWh";
      var left = mx + 10;
      if (left + 150 > rect.width) left = mx - 160;
      enTip.style.left = left + "px";
      enTip.style.top = (my - 30) + "px";
      enTip.style.display = "block";
    });
    enCv.addEventListener("mouseleave", function () {
      if (enTip) enTip.style.display = "none";
    });
  }

  // 统计清零
  var enReset = $("energy-reset");
  if (enReset) enReset.addEventListener("click", function () {
    if (!confirm("确定要清零所有耗电统计吗？累计/今日/本月与每日明细将全部归零，不可恢复。")) return;
    enMsg.className = "export-msg"; enMsg.textContent = "正在清零…";
    fetch("../api/crps/energy", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ action: "reset" })
    })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        if (d && d.ok) {
          enMsg.className = "export-msg ok"; enMsg.textContent = "耗电统计已清零";
          pollEnergy();
        } else {
          enMsg.className = "export-msg err"; enMsg.textContent = "清零失败: " + ((d && d.error) || "未知错误");
        }
      })
      .catch(function (e) {
        enMsg.className = "export-msg err"; enMsg.textContent = "清零失败: " + e.message;
      });
  });

  // ---- 启动 ----
  // 宿主切走标签时 iframe 被隐藏 (display:none), document.hidden 不会变化,
  // 用 IntersectionObserver 判定可见性: 不可见时暂停轮询, 回到前台立即补一次。
  var pageVisible = true;
  if ("IntersectionObserver" in window) {
    new IntersectionObserver(function (entries) {
      var v = entries[0].isIntersecting;
      if (pageVisible === v) return;
      pageVisible = v;
      if (v) { poll(); pollEnergy(); }
    }).observe(document.documentElement);
  }
  buildGrid();
  loadGpioConfig();
  poll();
  setInterval(function () { if (pageVisible) poll(); }, 3000);
  pollEnergy();
  setInterval(function () { if (pageVisible) pollEnergy(); }, 10000);
})();
