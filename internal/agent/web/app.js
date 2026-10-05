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
  up: "#22c55e",
  mixed: "#eab308",
  down: "#ef4444",
  unknown: "#9ca3af",
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
function renderHistory(host, data) {
  host.replaceChildren();
  const now = Math.floor(Date.now() / 300000) * 300,
    byTime = new Map(data.buckets.map((b) => [b.time, b]));
  const segments = [];
  for (let t = now - 43200; t <= now; t += 300) {
    const b = byTime.get(t),
      kind =
        !b || !b.checks
          ? "unknown"
          : !b.failures
          ? "up"
          : b.failures === b.checks
          ? "down"
          : "mixed";
    let last = segments.at(-1);
    if (!last || last.kind !== kind) {
      last = { kind, start: t, end: t, checks: 0, latency: 0, width: 0 };
      segments.push(last);
    }
    last.end = t + 300;
    last.width++;
    last.checks += b?.checks || 0;
    last.latency += b?.latency_sum || 0;
  }
  const bar = node("div");
  bar.className = "timeline";
  const selected = node("div");
  selected.className = "selection";
  for (const s of segments) {
    const b = node("button");
    b.style.flexGrow = s.width;
    b.style.background = colors[s.kind];
    const label = `${time(s.start)} – ${time(s.end)} · ${
      { up: "可达", mixed: "部分可达", down: "不可达", unknown: "未知" }[s.kind]
    } · ${s.checks ? (s.latency / s.checks).toFixed(1) + " ms" : "—"}`;
    b.setAttribute("aria-label", label);
    b.title = label;
    b.onclick = () => {
      selected.textContent = label;
    };
    bar.append(b);
  }
  host.append(bar, selected);
  const samples = data.buckets.filter((b) => b.checks);
  if (!samples.length) {
    host.append(node("p", "暂无历史数据"));
    return;
  }
  const ns = "http://www.w3.org/2000/svg",
    svg = document.createElementNS(ns, "svg");
  svg.setAttribute("viewBox", "0 0 600 120");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", "最近 12 小时平均延迟");
  const max = Math.max(1, ...samples.map((b) => b.latency_sum / b.checks));
  let d = "",
    last = 0;
  for (const b of samples) {
    const x = (600 * (b.time - (now - 43200))) / 43200,
      y = 110 - (100 * b.latency_sum) / b.checks / max;
    d += `${!last || b.time - last > 300 ? "M" : "L"}${x.toFixed(
      2,
    )},${y.toFixed(2)} `;
    last = b.time;
  }
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", d);
  path.setAttribute("fill", "none");
  path.setAttribute("stroke", "#39857b");
  path.setAttribute("stroke-width", "2");
  svg.append(path);
  if (samples.length === 1) {
    const dot = document.createElementNS(ns, "circle");
    dot.setAttribute("cx", (600 * (samples[0].time - (now - 43200))) / 43200);
    dot.setAttribute(
      "cy",
      110 - (100 * samples[0].latency_sum) / samples[0].checks / max,
    );
    dot.setAttribute("r", "3");
    dot.setAttribute("fill", "#39857b");
    svg.append(dot);
  }
  host.append(svg, node("p", `平均延迟 · 纵轴 0 – ${max.toFixed(1)} ms`));
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
        details.addEventListener("toggle", async () => {
          if (!details.open) return;
          for (const other of cards.values())
            if (other !== card) other.details.open = false;
          try {
            renderHistory(
              history,
              await api(`/api/history?monitor=${encodeURIComponent(m.id)}`),
            );
            card.loadedTime = card.latest?.time;
          } catch (e) {
            history.textContent = e.message;
          }
        });
      }
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
      if (card.details.open && card.loadedTime !== l?.time) {
        try {
          renderHistory(
            card.history,
            await api(`/api/history?monitor=${encodeURIComponent(m.id)}`),
          );
          card.loadedTime = l?.time;
        } catch (e) {
          card.history.textContent = e.message;
        }
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
