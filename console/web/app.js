"use strict";

const byId = (id) => document.getElementById(id);
const targets = { clojure: "Clojure 单机", cluster: "Go 集群", "go-a": "Go A", "go-b": "Go B" };
const scenarioTitles = { recovery: "漏水恢复", isolation: "用户隔离", shared: "跨实例共享" };
const colors = { allowed: "#174eb8", limited: "#af550c", errors: "#b3304c" };
const historyState = { points: [], instances: [], interval_seconds: 2 };
const clock = new Intl.DateTimeFormat("zh-CN", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
});
const compactClock = new Intl.DateTimeFormat("zh-CN", {
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});
const numbers = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 });
const logRows = [];
let sampleError = "";
let pollController;
let pollTimer;
let selectedPoints = [];
let displayEnd = 0;
let plotBounds;
let busy = false;

function cell(tag, value, className) {
  const node = document.createElement(tag);
  node.textContent = value;
  if (className) node.className = className;
  return node;
}

function format(value) {
  return Number.isFinite(value) ? numbers.format(value) : "—";
}

function formatTime(epoch, formatter = clock) {
  return Number.isFinite(epoch) ? formatter.format(new Date(epoch * 1000)) : "—";
}

function latency(value) {
  return value === -1 ? ">500" : format(value);
}

function setFeedback(text, failed = false) {
  byId("request-feedback").textContent = text;
  byId("request-feedback").classList.toggle("error", failed);
}

function setLive(text, state) {
  const live = byId("live");
  live.className = "live " + state;
  live.lastElementChild.textContent = text;
}

function requestRate(point, key) {
  const total = point.allowed + point.limited + point.errors;
  return total > 0 ? (point.rps * point[key]) / total : 0;
}

function drawChart() {
  const canvas = byId("chart");
  const width = canvas.clientWidth;
  const height = canvas.clientHeight;
  if (!width || !height) return;
  const dpr = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * dpr);
  canvas.height = Math.round(height * dpr);
  const ctx = canvas.getContext("2d");
  ctx.scale(dpr, dpr);
  const left = 35,
    right = width - 10,
    top = 20,
    bottom = height - 23;
  const selectedRange = Number(byId("watch-range").value);
  const coverage = selectedPoints.length
    ? displayEnd - selectedPoints[0].at + historyState.interval_seconds
    : 0;
  const range = coverage ? Math.min(selectedRange, Math.max(30, coverage)) : selectedRange;
  byId("chart-caption").textContent = coverage
    ? "每秒请求量 · 观测跨度 " + (coverage < 120 ? format(coverage) + " 秒" : format(coverage / 60) + " 分钟")
    : "各采样时段的请求速率";
  const start = displayEnd - range;
  const interval = historyState.interval_seconds;
  const x = (epoch) => left + ((right - left) * (epoch - start)) / range;
  const series = Object.keys(colors);
  let max = 0;
  let usable = false;
  for (const point of selectedPoints) {
    for (const key of series) {
      if (Number.isFinite(point[key])) {
        max = Math.max(max, requestRate(point, key));
        usable = true;
      }
    }
  }
  const unit = Math.pow(10, Math.floor(Math.log10(Math.max(max, 1))));
  const ceiling = [1, 2, 5, 10].map((value) => value * unit).find((value) => value >= max) || unit * 10;
  const y = (value) => bottom - ((bottom - top) * value) / ceiling;
  ctx.font = "10px ui-monospace, monospace";
  ctx.fillStyle = "#536778";
  ctx.fillText("次 / 秒", 1, 10);
  ctx.textAlign = "right";
  for (let i = 0; i <= 4; i++) {
    const value = (ceiling * i) / 4;
    const line = y(value);
    ctx.fillText(format(value), left - 5, line + 3);
    ctx.strokeStyle = "#e1e7e9";
    ctx.beginPath();
    ctx.moveTo(left, line);
    ctx.lineTo(right, line);
    ctx.stroke();
  }
  const ticks = width < 360 ? 2 : 4;
  for (let i = 0; i <= ticks; i++) {
    const epoch = start + (range * i) / ticks;
    ctx.textAlign = i === 0 ? "left" : i === ticks ? "right" : "center";
    ctx.fillText(formatTime(epoch, range < 300 ? clock : compactClock), x(epoch), height - 4);
  }
  // 未知值与丢失的采样窗口必须断开，不跨缺口补画流量。
  for (const key of [...series].reverse()) {
    const dots = [];
    const partialDots = [];
    let previous;
    ctx.beginPath();
    ctx.lineWidth = 1.7;
    ctx.strokeStyle = colors[key];
    for (const point of selectedPoints) {
      if (!Number.isFinite(point[key])) {
        previous = undefined;
        continue;
      }
      const px = x(point.at),
        py = y(requestRate(point, key));
      if (point.partial) {
        // 可见实例变少时，聚合值可能下降。用单独的空心点区分部分流量。
        partialDots.push([px, py]);
        previous = undefined;
        continue;
      }
      if (!previous || point.at - previous.at > interval * 1.75) {
        ctx.moveTo(px, py);
        dots.push([px, py]);
      } else {
        ctx.lineTo(px, py);
      }
      previous = point;
    }
    ctx.stroke();
    ctx.fillStyle = colors[key];
    for (const [px, py] of dots) ctx.fillRect(px - 1, py - 1, 2, 2);
    for (const [px, py] of partialDots) {
      ctx.beginPath();
      ctx.arc(px, py, 2.5, 0, Math.PI * 2);
      ctx.stroke();
    }
  }
  plotBounds = { start, range, left, right };
  byId("chart-empty").hidden = usable;
}

function renderSamples() {
  const body = byId("samples");
  body.replaceChildren();
  for (const point of selectedPoints.slice(-20).reverse()) {
    const row = document.createElement("tr");
    const hasSample = Number.isFinite(point.allowed);
    const quality = !hasSample ? "缺失" : point.partial ? "部分" : "完整";
    for (const value of [
      formatTime(point.at),
      format(point.allowed),
      format(point.limited),
      format(point.errors),
      latency(point.p95_ms),
      quality,
    ]) {
      row.append(cell("td", value));
    }
    body.append(row);
  }
}

function renderHealth() {
  const list = byId("instances");
  list.replaceChildren();
  const instances = historyState.instances;
  for (const entry of instances) {
    const li = document.createElement("li");
    const dot = document.createElement("i");
    let state = "等待采样";
    if (entry.at > 0 && entry.up === true) {
      state = entry.ready === true ? "就绪" : entry.ready === false ? "未就绪" : "就绪状态未知";
      dot.className = "instance-dot " + (entry.ready === true ? "ok" : "warn");
    } else if (entry.at > 0 && entry.ready === true) {
      state = "就绪 · 指标缺失";
      dot.className = "instance-dot warn";
    } else {
      dot.className = "instance-dot " + (entry.at > 0 && entry.up === false ? "bad" : "");
      if (entry.at > 0 && entry.up === false) state = "探测失败";
    }
    li.append(dot, cell("strong", targets[entry.id] || entry.id), cell("span", state));
    list.append(li);
  }
  if (!instances.length) list.append(cell("li", "等待采样", "placeholder"));
  const ready = instances.filter((entry) => entry.ready).length;
  byId("online").textContent = instances.length ? ready + " / " + instances.length + " 就绪" : "—";
  const last = Math.max(
    ...historyState.points.map((point) => point.at),
    ...instances.map((entry) => entry.at),
  );
  byId("last-sample").textContent = formatTime(last > 0 ? last : null);
}

function renderHistory() {
  const points = historyState.points;
  displayEnd = points.length ? points[points.length - 1].at : Date.now() / 1000;
  const start = displayEnd - Number(byId("watch-range").value);
  selectedPoints = points.filter((point) => point.at > start && point.at <= displayEnd);
  const latest = selectedPoints.at(-1);
  const currentPartial = latest?.partial && Number.isFinite(latest.rps);
  byId("rps-detail").textContent = currentPartial ? "次 / 秒 · 部分实例" : "次 / 秒 · 最近采样";
  byId("p95-detail").textContent = currentPartial ? "毫秒 · 部分实例上界" : "毫秒 · 分桶上界";
  byId("rps").textContent = format(latest?.rps);
  byId("latency").textContent = latency(latest?.p95_ms);
  for (const key of ["allowed", "limited", "errors"]) {
    const known = selectedPoints.filter((point) => Number.isFinite(point[key]));
    byId(key).textContent = known.length ? format(known.reduce((sum, p) => sum + p[key], 0)) : "—";
  }
  const decisions = selectedPoints.reduce((sum, p) => sum + (p.allowed || 0) + (p.limited || 0), 0);
  const rejected = selectedPoints.reduce((sum, p) => sum + (p.limited || 0), 0);
  byId("limit-ratio").textContent = decisions
    ? "次 · 拒绝率 " + format((100 * rejected) / decisions) + "%"
    : "次 · 所选时段";
  const partial = selectedPoints.some((point) => point.partial || !Number.isFinite(point.allowed));
  const note = partial ? "所选时段含缺失采样，统计仅包含已采集的请求。空心点表示部分实例的流量。" : "";
  byId("data-note").textContent = [sampleError, note].filter(Boolean).join(" ");
  renderHealth();
  renderSamples();
  drawChart();
}

async function poll() {
  clearTimeout(pollTimer);
  pollController?.abort();
  const controller = new AbortController();
  pollController = controller;
  const timeout = setTimeout(() => controller.abort(), 6500);
  try {
    const response = await fetch("/api/history?target=" + byId("watch-target").value, {
      signal: controller.signal,
      cache: "no-store",
    });
    if (!response.ok) throw Error("采样服务返回 HTTP " + response.status);
    const data = await response.json();
    if (controller !== pollController) return;
    historyState.points = data.points || [];
    historyState.instances = data.instances || [];
    historyState.interval_seconds = data.interval_seconds || 2;
    sampleError = "";
    setLive("每 " + historyState.interval_seconds + " 秒采样", "ok");
  } catch (error) {
    if (controller !== pollController) return;
    sampleError = "采样服务暂不可用，当前显示最近可用数据。";
    setLive("采样连接中断", "bad");
  } finally {
    clearTimeout(timeout);
    if (controller === pollController) {
      renderHistory();
      pollTimer = setTimeout(poll, historyState.interval_seconds * 1000);
    }
  }
}

function renderBucket(bucket) {
  const gauge = byId("gauge");
  byId("level").textContent = format(bucket?.level);
  byId("capacity").textContent = bucket ? "/ " + format(bucket.capacity) : "";
  byId("leak").textContent = bucket ? format(bucket.leak_rate) + " / 秒" : "—";
  byId("as-of").textContent = formatTime(bucket?.as_of);
  const level = bucket?.level || 0;
  const capacity = bucket?.capacity || 1;
  gauge.firstElementChild.style.width = Math.max(0, Math.min(100, (100 * level) / capacity)) + "%";
  gauge.setAttribute("aria-valuemax", capacity);
  gauge.setAttribute("aria-valuenow", level);
  gauge.setAttribute(
    "aria-valuetext",
    bucket ? "已占 " + format(level) + "，容量 " + format(capacity) : "暂无快照",
  );
}

function describeRow(row) {
  if (row.status === 429) return ["限流", "limited"];
  if (row.status === 404 && row.error === "bucket_not_found") return ["无桶记录", "neutral"];
  if (!(row.status >= 200 && row.status < 300)) return ["失败", "fail"];
  if (row.allowed === true) return ["放行", ""];
  if (row.allowed === false) return ["限流", "limited"];
  if (Number.isFinite(row.level) && (row.title === "查询" || row.operation === "GET")) return ["已查询", "neutral"];
  return ["失败", "fail"];
}

function renderRecords() {
  const body = byId("records");
  body.replaceChildren();
  for (const event of logRows) {
    const tr = document.createElement("tr");
    const [label, style] = describeRow(event);
    const instance = event.instance || "—";
    const destination = (targets[event.target] || event.target || "—") + " / " + instance;
    const user = cell("td", event.user_id, "record-user");
    user.title = event.user_id;
    tr.append(cell("td", event.title), user, cell("td", destination), cell("td", event.status || "—"));
    const outcome = document.createElement("td");
    outcome.append(cell("span", label, "badge " + style));
    tr.append(outcome, cell("td", format(event.level)), cell("td", format(event.elapsed_ms)));
    body.append(tr);
  }
  byId("clear").disabled = !logRows.length;
  byId("empty-log").hidden = !!logRows.length;
}

function appendRows(rows) {
  logRows.unshift(...rows);
  if (logRows.length > 100) logRows.length = 100;
  renderRecords();
}

function setBusy(value) {
  busy = value;
  for (const control of document.querySelectorAll(
    "#request-form input, #request-form select, #request-form button, [data-scenario]",
  )) {
    control.disabled = value;
  }
}

async function callTarget(target, user, kind) {
  const start = performance.now();
  const request = kind !== "read";
  const path = "/api/targets/" + target + "/v1/" + (request ? "check" : "bucket-state");
  const result = { target, user_id: user, title: request ? "请求" : "查询", status: null };
  try {
    const response = await fetch(path, {
      method: "POST",
      cache: "no-store",
      signal: AbortSignal.timeout(10000),
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ user_id: user }),
    });
    result.status = response.status;
    const data = await response.json();
    result.instance = data.instance;
    result.allowed = data.allowed;
    result.bucket = data.bucket;
    result.level = data.bucket?.level;
    result.error = data.error;
  } catch {
    result.error = "请求超时或服务未返回有效响应";
  }
  result.elapsed_ms = performance.now() - start;
  return result;
}

async function manual(kind) {
  if (busy || !byId("request-form").reportValidity()) return;
  const user = byId("user-id").value;
  const target = byId("target").value;
  const count = kind === "burst" ? 6 : 1;
  setBusy(true);
  setFeedback("正在请求 " + (targets[target] || target) + "…");
  try {
    const rows = await Promise.all(Array.from({ length: count }, () => callTarget(target, user, kind)));
    appendRows(rows);
    const snapshots = rows
      .filter((row) => row.bucket)
      .sort((a, b) => a.bucket.as_of - b.bucket.as_of || a.level - b.level);
    if (snapshots.length) renderBucket(snapshots.at(-1).bucket);
    else renderBucket(null);
    if (kind === "read") {
      const row = rows[0];
      if (row.status === 404 && row.error === "bucket_not_found") setFeedback("该用户还没有可查询的桶。");
      else if (row.status === 200 && row.bucket) setFeedback("已读取最近结算的桶快照。");
      else
        setFeedback(
          row.status ? "查询失败（HTTP " + row.status + "）。" : "查询失败，未收到有效响应。",
          true,
        );
    } else {
      const passed = rows.filter((row) => row.allowed === true).length;
      const limited = rows.filter((row) => row.status === 429 || row.allowed === false).length;
      const failures = count - passed - limited;
      const unavailable = rows.some((row) => row.status === 503);
      const summary = "放行 " + passed + " 次，限流 " + limited + " 次，失败 " + failures + " 次。";
      setFeedback(summary + (unavailable ? " 限流服务暂不可用（HTTP 503）。" : ""), failures > 0);
    }
  } finally {
    setBusy(false);
  }
}

async function runScenario(name) {
  if (busy) return;
  const target = name === "shared" ? "cluster" : byId("target").value;
  const result = byId("scenario-result");
  result.hidden = false;
  result.classList.remove("bad");
  result.textContent = "正在验证" + scenarioTitles[name] + "…";
  setBusy(true);
  try {
    const response = await fetch("/api/scenarios/" + name + "?target=" + target, {
      method: "POST",
      signal: AbortSignal.timeout(90000),
      cache: "no-store",
    });
    const data = await response.json();
    if (!Array.isArray(data.rows)) throw Error(data.error || "HTTP " + response.status);
    appendRows(
      data.rows.map((row) => ({ ...row, title: scenarioTitles[name] + " · " + (row.operation || "请求") })),
    );
    result.classList.toggle("bad", !data.passed);
    result.textContent = [
      scenarioTitles[name] + (data.passed ? " · 验证通过" : " · 验证未通过"),
      data.expected,
      data.summary,
    ]
      .filter(Boolean)
      .join("\n");
  } catch (error) {
    result.classList.add("bad");
    result.textContent = scenarioTitles[name] + " · 场景执行失败。" + error.message;
  } finally {
    setBusy(false);
  }
}

byId("watch-target").addEventListener("change", () => {
  historyState.points = [];
  historyState.instances = [];
  sampleError = "";
  renderHistory();
  poll();
});
byId("watch-range").addEventListener("change", renderHistory);
byId("request-form").addEventListener("submit", (event) => {
  event.preventDefault();
  manual("once");
});
byId("burst").addEventListener("click", () => manual("burst"));
byId("read").addEventListener("click", () => manual("read"));
byId("user-id").value = "demo-" + Math.random().toString(36).slice(2, 9);
for (const [id, event] of [
  ["target", "change"],
  ["user-id", "input"],
]) {
  byId(id).addEventListener(event, () => {
    renderBucket(null);
    setFeedback("等待操作");
  });
}
for (const button of document.querySelectorAll("[data-scenario]")) {
  button.addEventListener("click", () => runScenario(button.dataset.scenario));
}
byId("clear").addEventListener("click", () => {
  logRows.length = 0;
  renderRecords();
});
byId("chart").addEventListener("pointermove", (event) => {
  if (!plotBounds || !selectedPoints.length) return;
  const { start, range, left, right } = plotBounds;
  const time = start + ((event.offsetX - left) * range) / (right - left);
  const nearest = selectedPoints.reduce((a, b) => (Math.abs(a.at - time) < Math.abs(b.at - time) ? a : b));
  const tip = byId("chart-tip");
  if (Math.abs(nearest.at - time) > historyState.interval_seconds * 2) {
    tip.hidden = true;
    return;
  }
  tip.textContent =
    formatTime(nearest.at) +
    " · 放行 " +
    format(nearest.allowed) +
    " / 限流 " +
    format(nearest.limited) +
    " / 错误 " +
    format(nearest.errors) +
    (nearest.partial ? " 次 · 部分采样" : " 次");
  tip.hidden = false;
});
byId("chart").addEventListener("pointerleave", () => {
  byId("chart-tip").hidden = true;
});
const links = [...document.querySelectorAll("nav a")];
function showCurrentSection() {
  const current =
    links.filter((link) => byId(link.hash.slice(1)).getBoundingClientRect().top <= 130).at(-1) || links[0];
  for (const link of links) {
    if (link === current) link.setAttribute("aria-current", "location");
    else link.removeAttribute("aria-current");
  }
}
window.addEventListener("scroll", showCurrentSection, { passive: true });
new ResizeObserver(drawChart).observe(byId("chart"));
renderHistory();
poll();
