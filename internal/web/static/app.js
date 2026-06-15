// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ
//
// Embedded dashboard logic for go-mtec2mqtt. Plain ES module-free
// vanilla JS: load catalog + config once, then live-update from the SSE
// stream. No framework, no build step.
"use strict";

// Group metadata: display order, German label, and which page section
// the group's card belongs to. Groups not listed here still render under
// "System" with a prettified name.
const GROUPS = {
  "now-base": { label: "Basis", section: "live" },
  "now-grid": { label: "Netz", section: "live" },
  "now-inverter": { label: "Wechselrichter", section: "live" },
  "now-backup": { label: "Backup / Notstrom", section: "live" },
  "now-battery": { label: "Batterie", section: "live" },
  "now-pv": { label: "PV / Solar", section: "live" },
  day: { label: "Heute", section: "energy" },
  total: { label: "Gesamt", section: "energy" },
  config: { label: "Geräte-Konfiguration", section: "system" },
  static: { label: "Geräte-Info", section: "system" },
};

const SECTION_EL = {
  live: () => document.getElementById("live-groups"),
  energy: () => document.getElementById("energy-groups"),
  system: () => document.getElementById("system-groups"),
};

// Catalog state, loaded once.
let registersByKey = {}; // output key (mqtt||name) -> register info
let writables = []; // register infos with writable=true
let latestSnapshot = { groups: {} };

// ---------- bootstrap ----------
document.addEventListener("DOMContentLoaded", init);

async function init() {
  initTheme();
  initNav();
  try {
    const regs = await fetchJSON("/api/registers");
    indexRegisters(regs);
    buildControlList();
  } catch (e) {
    toast("Register konnten nicht geladen werden: " + e.message, "err");
  }
  try {
    renderConfig(await fetchJSON("/api/config"));
  } catch (e) {
    /* config is non-critical */
  }
  connectSSE();
}

function indexRegisters(regs) {
  registersByKey = {};
  writables = [];
  for (const r of regs) {
    const key = r.mqtt || r.name;
    if (key) registersByKey[key] = r;
    if (r.writable) writables.push(r);
  }
}

// ---------- SSE ----------
function connectSSE() {
  const es = new EventSource("/api/events");
  es.addEventListener("update", (ev) => {
    streamDot(true);
    try {
      render(JSON.parse(ev.data));
    } catch (e) {
      /* ignore malformed frame */
    }
  });
  es.onerror = () => streamDot(false); // EventSource auto-reconnects
}

// ---------- render ----------
function render(view) {
  const health = view.health || {};
  latestSnapshot = view.snapshot || { groups: {} };
  renderHeader(health);
  renderHealth(health);
  renderGroups(latestSnapshot, health);
  updateControlValues(latestSnapshot);
  const ts = new Date();
  document.getElementById("last-update").textContent =
    "Stand: " + ts.toLocaleTimeString("de-DE");
}

function renderHeader(h) {
  const equip = h.equipment ? "M-TEC " + h.equipment : "M-TEC Inverter";
  document.getElementById("device-title").textContent = equip;
  const sub = [];
  if (h.serial) sub.push("SN " + h.serial);
  if (h.firmware) sub.push("FW " + h.firmware);
  document.getElementById("device-sub").textContent = sub.join("  ·  ");
  if (h.version) document.getElementById("version").textContent = "v" + h.version;

  const badge = document.getElementById("conn-badge");
  const map = {
    ok: ["badge-ok", "Verbunden"],
    degraded: ["badge-warn", "Modbus getrennt"],
    starting: ["badge-neutral", "Initialisiere…"],
  };
  const [cls, text] = map[h.status] || ["badge-neutral", h.status || "—"];
  badge.className = "badge " + cls;
  badge.textContent = text;
}

function renderHealth(h) {
  const items = [
    tile("Status", capitalise(h.status || "—"), statusTone(h.status)),
    tile("Modbus", h.modbus_connected ? "verbunden" : "getrennt",
      h.modbus_connected ? "ok" : "err", h.modbus_addr),
    tile("MQTT-Broker", h.mqtt_server || "—", null),
    tile("Topic", h.mqtt_topic || "—", null),
    tile("Home Assistant", h.hass_enabled ? "aktiv" : "aus", h.hass_enabled ? "ok" : null),
    tile("Laufzeit", formatUptime(h.uptime_seconds), null),
    tile("Seriennummer", h.serial || "—", null),
    tile("Firmware", h.firmware || "—", null),
  ];
  setGrid("health-grid", items);
}

function renderGroups(snapshot, health) {
  const groups = snapshot.groups || {};
  const ghealth = (health && health.groups) || {};
  // Clear all section containers.
  for (const fn of Object.values(SECTION_EL)) fn().innerHTML = "";

  // Order: known groups first (GROUPS order), then any extras.
  const order = Object.keys(GROUPS).filter((g) => g in groups);
  for (const g of Object.keys(groups)) if (!order.includes(g)) order.push(g);

  for (const g of order) {
    const meta = GROUPS[g] || { label: prettify(g), section: "system" };
    const card = groupCard(g, meta.label, groups[g], ghealth[g]);
    SECTION_EL[meta.section]().appendChild(card);
  }
  for (const [sec, fn] of Object.entries(SECTION_EL)) {
    if (!fn().children.length) {
      fn().innerHTML = `<div class="card empty">Noch keine Daten für „${sec}“.</div>`;
    }
  }
}

function groupCard(groupKey, label, view, gh) {
  const card = el("div", "card group-card");
  const head = el("div", "group-head");
  head.appendChild(el("h3", null, label));
  if (gh && Number.isFinite(gh.age_seconds)) {
    head.appendChild(el("span", "group-age", "aktualisiert vor " + formatAge(gh.age_seconds)));
  }
  card.appendChild(head);

  const grid = el("div", "status-grid");
  const values = (view && view.values) || {};
  const keys = Object.keys(values).sort((a, b) =>
    labelFor(a).localeCompare(labelFor(b), "de"));
  if (!keys.length) {
    grid.appendChild(el("div", "empty", "—"));
  }
  for (const k of keys) {
    grid.appendChild(valueTile(k, values[k]));
  }
  card.appendChild(grid);
  return card;
}

function valueTile(key, value) {
  const reg = registersByKey[key];
  const item = el("div", "status-item");
  item.appendChild(el("div", "label", labelFor(key)));
  const v = el("div", "value");
  v.textContent = fmtValue(value);
  const unit = reg && reg.unit;
  if (unit) {
    const u = el("span", "unit");
    u.textContent = unit;
    v.appendChild(u);
  }
  const tone = valueTone(value, reg);
  if (tone) item.classList.add("tone-" + tone);
  item.appendChild(v);
  return item;
}

// ---------- control (writable registers) ----------
function buildControlList() {
  const host = document.getElementById("control-list");
  host.innerHTML = "";
  if (!writables.length) {
    host.innerHTML = `<div class="empty">Keine schreibbaren Register im Katalog.</div>`;
    return;
  }
  for (const r of writables) {
    host.appendChild(controlRow(r));
  }
}

function controlRow(r) {
  const key = r.mqtt || r.name;
  const row = el("div", "control-row");

  const name = el("div", "c-name");
  name.appendChild(el("strong", null, r.name || key));
  const sub = [key];
  if (r.unit) sub.push(r.unit);
  name.appendChild(el("small", null, sub.join(" · ")));
  row.appendChild(name);

  let input;
  if (r.value_items && Object.keys(r.value_items).length) {
    input = el("select");
    for (const code of Object.keys(r.value_items)) {
      const opt = el("option", null, r.value_items[code]);
      opt.value = r.value_items[code]; // server reverse-maps label -> code
      input.appendChild(opt);
    }
  } else {
    input = el("input");
    input.type = "number";
    input.step = "any";
    input.placeholder = r.unit ? "Wert (" + r.unit + ")" : "Wert";
  }

  const current = el("span", "c-current");
  current.dataset.key = key;
  current.textContent = "—";
  row.appendChild(current);

  const btn = el("button", "btn", "Setzen");
  btn.addEventListener("click", () => writeRegister(key, input.value, btn));

  const ctrls = el("div");
  ctrls.style.display = "flex";
  ctrls.style.gap = "8px";
  ctrls.appendChild(input);
  ctrls.appendChild(btn);
  row.appendChild(ctrls);
  return row;
}

function updateControlValues(snapshot) {
  const flat = {};
  for (const g of Object.values(snapshot.groups || {})) {
    Object.assign(flat, g.values || {});
  }
  document.querySelectorAll(".c-current[data-key]").forEach((sp) => {
    const k = sp.dataset.key;
    if (k in flat) {
      const reg = registersByKey[k];
      sp.textContent = "aktuell: " + fmtValue(flat[k]) + (reg && reg.unit ? " " + reg.unit : "");
    }
  });
}

async function writeRegister(key, value, btn) {
  if (value === "" || value === null || value === undefined) {
    toast("Bitte einen Wert eingeben.", "err");
    return;
  }
  btn.disabled = true;
  try {
    const res = await fetch("/api/write", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key, value }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    toast(key + " = " + value + " gesetzt", "ok");
  } catch (e) {
    toast("Fehler: " + e.message, "err");
  } finally {
    btn.disabled = false;
  }
}

// ---------- config ----------
function renderConfig(c) {
  const items = [
    tile("Modbus", c.modbus_ip + ":" + c.modbus_port, null, "Slave " + c.modbus_slave),
    tile("MQTT", c.mqtt_server + ":" + c.mqtt_port, null, c.mqtt_topic),
    tile("Home Assistant", c.hass_enable ? "aktiv" : "aus", null, c.hass_base_topic),
    tile("Refresh now", c.refresh_now + " s", null),
    tile("Refresh config", c.refresh_config + " s", null),
    tile("Refresh day", c.refresh_day + " s", null),
    tile("Refresh total", c.refresh_total + " s", null),
    tile("Refresh static", c.refresh_static + " s", null),
    tile("Debug", c.debug ? "an" : "aus", null),
  ];
  setGrid("config-grid", items);
}

// ---------- small helpers ----------
function labelFor(key) {
  const r = registersByKey[key];
  return (r && r.name) || prettify(key);
}

function fmtValue(v) {
  if (v === null || v === undefined || v === "") return "–";
  if (typeof v === "boolean") return v ? "Ja" : "Nein";
  if (typeof v === "number") {
    if (Number.isInteger(v)) return v.toLocaleString("de-DE");
    return v.toLocaleString("de-DE", { maximumFractionDigits: 2 });
  }
  return String(v);
}

function valueTone(value, reg) {
  if (typeof value === "string") {
    if (value === "OK") return "ok";
    if (reg && reg.value_items && value !== "OK" && value !== "Unknown") return "warn";
  }
  return null;
}

function statusTone(s) {
  return { ok: "ok", degraded: "warn", starting: null }[s] || null;
}

function tile(label, value, tone, sub) {
  return { label, value: sub ? value + "  ·  " + sub : value, tone };
}

function setGrid(id, items) {
  const host = document.getElementById(id);
  host.innerHTML = "";
  for (const it of items) {
    const item = el("div", "status-item" + (it.tone ? " tone-" + it.tone : ""));
    item.appendChild(el("div", "label", it.label));
    item.appendChild(el("div", "value", it.value));
    host.appendChild(item);
  }
}

function formatUptime(sec) {
  sec = Number(sec) || 0;
  const d = Math.floor(sec / 86400);
  const h = Math.floor((sec % 86400) / 3600);
  const m = Math.floor((sec % 3600) / 60);
  if (d) return `${d} d ${h} h`;
  if (h) return `${h} h ${m} min`;
  if (m) return `${m} min`;
  return `${Math.floor(sec)} s`;
}

function formatAge(sec) {
  sec = Math.max(0, Math.floor(Number(sec) || 0));
  if (sec < 60) return sec + " s";
  if (sec < 3600) return Math.floor(sec / 60) + " min";
  return Math.floor(sec / 3600) + " h";
}

function prettify(key) {
  return String(key)
    .replace(/[_\-]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

function capitalise(s) {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined && text !== null) e.textContent = text;
  return e;
}

async function fetchJSON(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(res.status + " " + res.statusText);
  return res.json();
}

let toastTimer = null;
function toast(msg, kind) {
  const t = document.getElementById("toast");
  t.textContent = msg;
  t.className = "toast " + (kind || "ok");
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 3500);
}

function streamDot(live) {
  document.getElementById("stream-dot").classList.toggle("live", !!live);
}

// ---------- nav + theme ----------
function initNav() {
  const items = [...document.querySelectorAll(".nav-item")];
  const sections = items
    .map((i) => document.getElementById(i.dataset.target))
    .filter(Boolean);
  const obs = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (e.isIntersecting) {
          items.forEach((i) =>
            i.classList.toggle("active", i.dataset.target === e.target.id));
        }
      }
    },
    { rootMargin: "-40% 0px -55% 0px" }
  );
  sections.forEach((s) => obs.observe(s));
}

function initTheme() {
  const saved = localStorage.getItem("mtec-theme");
  if (saved) document.documentElement.dataset.theme = saved;
  document.getElementById("theme-toggle").addEventListener("click", () => {
    const cur = document.documentElement.dataset.theme === "light" ? "dark" : "light";
    document.documentElement.dataset.theme = cur;
    localStorage.setItem("mtec-theme", cur);
  });
}
