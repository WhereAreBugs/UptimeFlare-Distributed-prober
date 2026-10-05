"use strict";
const $ = (id) => document.getElementById(id),
  time = (n) => (n ? new Date(n * 1000).toLocaleString() : "—"),
  bytes = (n) => `${(n / 1048576).toFixed(2)} MiB`;
const node = (tag, text) => {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  return e;
};
const colors = {
  up: "#059669",
  mixed: "#eab308",
  down: "#df484a",
  unknown: "#70778c",
};
let page = 1,
  total = 0,
  generation = 0;
const cards = new Map();
async function api(path) {
  const r = await fetch(path, { cache: "no-store" });
  if (!r.ok)
    throw Error(r.status === 401 ? "需要鉴权" : "暂时无法读取探针数据");
  return r.json();
}
function list(id, values) {
  const dl = $(id);
  dl.replaceChildren();
  for (const [label, value] of values)
    dl.append(node("dt", label), node("dd", value));
}
async function status() {
  try {
    const { status: s, probe: p, registered } = await api("/api/status");
    $("error").hidden = true;
    $("name").textContent = p.name || "探针详情";
    $("registered").textContent = registered ? "已注册" : "待注册";
    list("registration", [
      ["地区 / ASN", p.location || "—"],
      ["系统", `${s.os} / ${s.arch}`],
      ["版本", s.build],
      ["启动时间", time(s.started_at)],
      ["配置同步", time(s.last_config_at)],
      ["监控目标", s.monitor_count],
      ["探测状态", s.checking_paused ? "容量已满，暂停采集" : "运行中"],
    ]);
    list("queue", [
      ["待上传", `${s.queue_count} 条`],
      ["待上传负载", bytes(s.queue_bytes)],
      ["数据库", `${bytes(s.database_bytes)} / ${bytes(s.database_limit)}`],
      ["队首采样", time(s.oldest_queued_at)],
      ["最近确认上传", time(s.last_upload_at)],
      [
        "上传状态",
        { idle: "等待", uploading: "上传中", retrying: "等待重试" }[
          s.upload_state
        ] || "—",
      ],
      ["下次上传", time(s.next_upload_at)],
    ]);
    $("usage").value = Math.min(1, s.database_bytes / s.database_limit);
  } catch (e) {
    $("error").hidden = false;
    $("error").textContent = e.message;
  }
}
const compact = window.matchMedia("(max-width: 48em)");
const labels = {
  up: "可达",
  mixed: "部分可达",
  down: "不可达",
  unknown: "未知",
};
const average = (n) => (n === null ? "—" : `${n.toFixed(1)} ms`);
const utcDate = (n) => new Date(n * 1000).toISOString().slice(0, 10);
function rangeLabel(start, end, daily) {
  return daily
    ? `${utcDate(start)}${
        end - start > 86400 ? " – " + utcDate(end - 1) : ""
      } UTC`
    : `${time(start)} – ${time(end)}`;
}
function selection(host, start, end, daily, status, latency) {
  host.replaceChildren();
  for (const [label, value] of [
    ["时段", rangeLabel(start, end, daily)],
    ["可达性", labels[status]],
    ["平均延迟", average(latency)],
  ])
    host.append(node("dt", label), node("dd", value));
}
function timeline(host, buckets, daily) {
  const bar = node("div"),
    endpoints = node("div"),
    selected = node("dl");
  bar.className = "timeline";
  bar.setAttribute("role", "group");
  bar.setAttribute(
    "aria-label",
    daily ? "90 天可用率历史" : "12 小时可达性历史",
  );
  endpoints.className = "endpoints";
  selected.className = "selection";
  for (const s of ProbeHistory.segments(buckets, compact.matches)) {
    const button = node("button");
    button.type = "button";
    button.style.flexGrow = s.count;
    button.style.background = colors[s.status];
    const label = `${rangeLabel(s.start, s.end, daily)} · ${
      labels[s.status]
    } · ${average(s.average)}`;
    const details = `${label} · ${
      s.checks
        ? (((s.checks - s.failures) * 100) / s.checks).toFixed(3) + "%"
        : "无数据"
    } · ${s.checks} 次检测，${s.failures} 次失败`;
    button.setAttribute("aria-label", compact.matches ? label : details);
    if (!compact.matches) button.title = details;
    button.onclick = () =>
      selection(selected, s.start, s.end, daily, s.status, s.average);
    bar.append(button);
  }
  endpoints.append(
    node("span", daily ? "90 天前" : "12 小时前"),
    node("span", daily ? "今天 UTC" : "现在"),
  );
  host.append(bar, endpoints, selected);
}
function drawChart(host, buckets, range) {
  host.replaceChildren();
  const ns = "http://www.w3.org/2000/svg",
    daily = range === "90d";
  const points = ProbeHistory.series(buckets, range),
    values = points.filter((p) => p.value !== null);
  const width = Math.max(
      320,
      Math.round(host.getBoundingClientRect().width || 640),
    ),
    height = 180;
  const left = 48,
    right = width - 12,
    top = 14,
    bottom = 146;
  const peak = Math.max(1, ...values.map((p) => p.value));
  const unit = 10 ** Math.floor(Math.log10(peak)),
    max = daily ? 100 : Math.ceil(peak / unit) * unit;
  const svg = document.createElementNS(ns, "svg"),
    selected = node("dl");
  selected.className = "selection";
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("role", "img");
  svg.setAttribute(
    "aria-label",
    daily ? "90 天可用率折线图" : "12 小时平均延迟折线图",
  );
  const element = (tag, attributes, text) => {
    const e = document.createElementNS(ns, tag);
    for (const [key, value] of Object.entries(attributes))
      e.setAttribute(key, value);
    if (text !== undefined) e.textContent = text;
    svg.append(e);
    return e;
  };
  const x = (i) => left + (i * (right - left)) / (points.length - 1);
  const y = (value) => bottom - (value / max) * (bottom - top);
  for (let i = 0; i <= 4; i++) {
    const value = (max * i) / 4,
      py = y(value);
    element("line", {
      x1: left,
      x2: right,
      y1: py,
      y2: py,
      class: "chart-grid",
    });
    element(
      "text",
      { x: left - 6, y: py + 4, "text-anchor": "end", class: "chart-label" },
      `${value >= 10 ? value.toFixed(0) : value.toFixed(1)}${daily ? "%" : ""}`,
    );
    const index = Math.round(((points.length - 1) * i) / 4),
      date = new Date(points[index].time * 1000);
    const label = daily
      ? utcDate(points[index].time).slice(5)
      : date.toLocaleTimeString([], {
          hour: "2-digit",
          minute: "2-digit",
          hour12: false,
        });
    element(
      "text",
      {
        x: x(index),
        y: bottom + 20,
        "text-anchor": i === 0 ? "start" : i === 4 ? "end" : "middle",
        class: "chart-label",
      },
      label,
    );
  }
  element("text", { x: left, y: 10, class: "chart-label" }, daily ? "%" : "ms");
  let d = "",
    gap = true;
  points.forEach((p, i) => {
    if (p.value === null) {
      gap = true;
      return;
    }
    d += `${gap ? "M" : "L"}${x(i).toFixed(2)},${y(p.value).toFixed(2)} `;
    gap = false;
  });
  element("path", { d, fill: "none", stroke: "#70778c", "stroke-width": 2 });
  points.forEach((p, i) => {
    if (p.value === null) return;
    const dot = element("circle", {
      cx: x(i),
      cy: y(p.value),
      r: values.length === 1 ? 3 : 1.5,
      fill: "#70778c",
    });
    if (!compact.matches) {
      const title = document.createElementNS(ns, "title");
      title.textContent = `${
        daily ? utcDate(p.time) + " UTC" : time(p.time)
      } · ${p.value.toFixed(daily ? 3 : 1)}${daily ? "%" : " ms"}`;
      dot.append(title);
    }
  });
  svg.onclick = (event) => {
    const rect = svg.getBoundingClientRect(),
      position = ((event.clientX - rect.left) / rect.width) * width;
    const index = Math.max(
      0,
      Math.min(
        points.length - 1,
        Math.round(((position - left) / (right - left)) * (points.length - 1)),
      ),
    );
    const p = points[index];
    selection(selected, p.time, p.time + p.step, daily, p.status, p.average);
  };
  host.append(svg, selected);
  if (!values.length) host.append(node("p", "暂无历史数据"));
}
function renderHistory(card) {
  const host = card.history,
    data = card.data,
    now = Date.now() / 1000;
  host.replaceChildren();
  card.renderWindow = Math.floor(now / 300);
  const recent = ProbeHistory.window(data, now, "12h"),
    daily = ProbeHistory.window(data, now, "90d");
  host.append(node("h3", "12 小时历史"));
  timeline(host, recent, false);
  host.append(node("h3", "90 天历史"));
  timeline(host, daily, true);
  const controls = node("div"),
    chart = node("div");
  controls.className = "chart-controls";
  chart.className = "chart";
  const buttons = [];
  for (const [range, label] of [
    ["12h", "12 小时延迟"],
    ["90d", "90 天可用率"],
  ]) {
    const b = node("button", label);
    b.type = "button";
    buttons.push([range, b]);
    b.onclick = () => {
      card.range = range;
      draw();
    };
    controls.append(b);
  }
  host.append(controls, chart);
  function draw() {
    const range = card.range || "12h";
    for (const [value, b] of buttons)
      b.setAttribute("aria-pressed", value === range ? "true" : "false");
    drawChart(chart, range === "90d" ? daily : recent, range);
  }
  draw();
}
async function loadHistory(card, id) {
  if (card.loading) return;
  card.loading = true;
  try {
    const data = await api(`/api/history?monitor=${encodeURIComponent(id)}`);
    if (!card.details.open || cards.get(id) !== card) return;
    card.data = data;
    card.loadedTime = data.latest?.time;
    renderHistory(card);
  } catch (e) {
    if (card.details.open) card.history.textContent = e.message;
  } finally {
    card.loading = false;
  }
}
async function monitors() {
  const current = ++generation;
  try {
    const data = await api(`/api/monitors?page=${page}`);
    if (current !== generation) return;
    total = data.total;
    const ids = data.monitors.map((m) => m.id);
    for (const [id, card] of cards) {
      if (!ids.includes(id)) {
        card.details.remove();
        cards.delete(id);
      }
    }
    for (const m of data.monitors) {
      let card = cards.get(m.id);
      if (!card) {
        const details = node("details"),
          summary = node("summary"),
          meta = node("p"),
          history = node("div");
        meta.className = "meta";
        details.append(summary, meta, history);
        card = { details, summary, meta, history };
        cards.set(m.id, card);
        $("monitors").append(details);
        details.addEventListener("toggle", () => {
          if (!details.open) return;
          for (const other of cards.values())
            if (other !== card) other.details.open = false;
          loadHistory(card, m.id);
        });
      }
      // Existing cards can move between enabled/paused pages without stale order.
      $("monitors").append(card.details);
      card.latest = m.latest;
      const l = m.latest,
        st = m.paused
          ? "关闭"
          : !l ||
            l.time > Date.now() / 1000 + 300 ||
            Date.now() / 1000 - l.time > 2 * m.intervalSeconds
          ? "未知"
          : l.up
          ? "正常"
          : "异常";
      card.summary.textContent = `${m.name || "未命名目标"} · ${st}`;
      card.meta.textContent = `${m.method} · 每 ${
        m.intervalSeconds
      } 秒 · 超时 ${(m.timeout || 5000) / 1000} 秒 · 最近采样 ${time(l?.time)}${
        l ? " · " + l.latency_ms.toFixed(1) + " ms" : ""
      }`;
      if (card.details.open) {
        if (!card.data || card.loadedTime !== l?.time)
          await loadHistory(card, m.id);
        else if (card.renderWindow !== Math.floor(Date.now() / 300000))
          renderHistory(card);
      }
    }
    $("total").textContent = `${total} 个目标`;
    $("page").textContent = `${page} / ${Math.max(1, Math.ceil(total / 10))}`;
    $("previous").disabled = page === 1;
    $("next").disabled = page * 10 >= total;
  } catch (e) {
    $("error").hidden = false;
    $("error").textContent = e.message;
  }
}
$("previous").onclick = () => {
  page--;
  monitors();
};
$("next").onclick = () => {
  page++;
  monitors();
};
status();
monitors();
setInterval(() => {
  if (!document.hidden) {
    status();
    monitors();
  }
}, 15000);

compact.addEventListener("change", () => {
  for (const card of cards.values())
    if (card.details.open && card.data) renderHistory(card);
});
let resizeTimer;
window.addEventListener("resize", () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => {
    for (const card of cards.values())
      if (card.details.open && card.data) renderHistory(card);
  }, 100);
});
