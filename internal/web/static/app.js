// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ
//
// Embedded dashboard logic for go-mtec2mqtt. Plain ES module-free
// vanilla JS: load language + catalog + config once, then live-update
// from the SSE stream. No framework, no build step.
//
// Localisation: the server reports the configured LANGUAGE via
// /api/config; the matching bundle in i18n/<lang>.json drives all static
// chrome (data-i18n attributes) and the dynamic strings via t()/tf().
// Register names and enum labels are localised server-side, so they
// arrive ready to render.
"use strict";

// Group → page-section mapping. Labels are resolved from the i18n bundle
// under "group.<key>"; groups not listed here still render under
// "System" with a prettified name.
const GROUPS = {
  "now-base": "live",
  "now-grid": "live",
  "now-inverter": "live",
  "now-backup": "live",
  "now-battery": "live",
  "now-pv": "live",
  day: "energy",
  total: "energy",
  config: "system",
  static: "system",
};

const SECTION_EL = {
  live: () => document.getElementById("live-groups"),
  energy: () => document.getElementById("energy-groups"),
  system: () => document.getElementById("system-groups"),
};

// i18n + catalog state, loaded once.
let I18N = {};
let LANG = "en";
let LOCALE = "en-US";
let registersByKey = {}; // output key (mqtt||name) -> register info
let writables = []; // register infos with writable=true
let latestSnapshot = { groups: {} };

// ---------- i18n ----------
function t(key) {
  return key in I18N ? I18N[key] : key;
}

// tf looks up key and substitutes {name} placeholders from params.
function tf(key, params) {
  let s = t(key);
  // Replacement is passed as a function, not a string: params[k] may be
  // user-supplied (e.g. a register value typed into the write form), and
  // String.replace() would otherwise interpret "$&", "$$", "$1" etc. in a
  // string replacement as special patterns.
  for (const k in params) s = s.replace("{" + k + "}", () => params[k]);
  return s;
}

async function loadI18n(lang) {
  LANG = lang === "de" ? "de" : "en";
  LOCALE = LANG === "de" ? "de-DE" : "en-US";
  document.documentElement.lang = LANG;
  try {
    I18N = await fetchJSON("i18n/" + LANG + ".json");
  } catch (e) {
    I18N = {}; // fall back to raw keys / English HTML defaults
  }
  applyStaticI18n();
}

// applyStaticI18n fills every element carrying a data-i18n / data-i18n-title
// attribute from the loaded bundle. Missing keys leave the HTML default.
function applyStaticI18n() {
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    const key = el.dataset.i18n;
    if (key in I18N) el.textContent = I18N[key];
  });
  document.querySelectorAll("[data-i18n-title]").forEach((el) => {
    const key = el.dataset.i18nTitle;
    if (key in I18N) el.title = I18N[key];
  });
}

// ---------- bootstrap ----------
document.addEventListener("DOMContentLoaded", init);

async function init() {
  initTheme();
  initNav();
  // Language first so all subsequent rendering is localised. Config is
  // otherwise non-critical, so a failure just leaves English defaults.
  let config = null;
  try {
    // Relative URL (no leading slash) so the SPA works both when served
    // directly and behind the Home-Assistant ingress path prefix.
    config = await fetchJSON("api/config");
  } catch (e) {
    /* non-critical */
  }
  await loadI18n((config && config.language) || "en");
  try {
    const regs = await fetchJSON("api/registers");
    indexRegisters(regs);
    buildControlList();
  } catch (e) {
    toast(t("toast.regsFail") + e.message, "err");
  }
  if (config) renderConfig(config);
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
  const es = new EventSource("api/events");
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
    t("footer.updated") + ts.toLocaleTimeString(LOCALE);
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
    ok: ["badge-ok", t("status.ok")],
    degraded: ["badge-warn", t("status.degraded")],
    starting: ["badge-neutral", t("status.starting")],
  };
  const [cls, text] = map[h.status] || ["badge-neutral", h.status || "—"];
  badge.className = "badge " + cls;
  badge.textContent = text;
}

function renderHealth(h) {
  const items = [
    tile(t("health.status"), capitalise(h.status || "—"), statusTone(h.status)),
    tile(t("health.modbus"), h.modbus_connected ? t("val.connected") : t("val.disconnected"),
      h.modbus_connected ? "ok" : "err", h.modbus_addr),
    tile(t("health.mqtt"), h.mqtt_server || "—", null),
    tile(t("health.topic"), h.mqtt_topic || "—", null),
    tile(t("health.hass"), h.hass_enabled ? t("val.active") : t("val.off"), h.hass_enabled ? "ok" : null),
    tile(t("health.uptime"), formatUptime(h.uptime_seconds), null),
    tile(t("health.serial"), h.serial || "—", null),
    tile(t("health.firmware"), h.firmware || "—", null),
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
    const section = GROUPS[g] || "system";
    const card = groupCard(g, groupLabel(g), groups[g], ghealth[g]);
    SECTION_EL[section]().appendChild(card);
  }
  for (const [, fn] of Object.entries(SECTION_EL)) {
    if (!fn().children.length) {
      fn().innerHTML = `<div class="card empty">${t("group.empty")}</div>`;
    }
  }
}

function groupLabel(g) {
  const key = "group." + g;
  return key in I18N ? I18N[key] : prettify(g);
}

function groupCard(groupKey, label, view, gh) {
  const card = el("div", "card group-card");
  const head = el("div", "group-head");
  head.appendChild(el("h3", null, label));
  if (gh && Number.isFinite(gh.age_seconds)) {
    head.appendChild(el("span", "group-age", tf("group.age", { age: formatAge(gh.age_seconds) })));
  }
  card.appendChild(head);

  const grid = el("div", "status-grid");
  const values = (view && view.values) || {};
  const keys = Object.keys(values).sort((a, b) =>
    labelFor(a).localeCompare(labelFor(b), LOCALE));
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
    host.innerHTML = `<div class="empty">${t("ctl.none")}</div>`;
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

  const current = el("span", "c-current");
  current.dataset.key = key;
  current.textContent = "—";

  // A synthetic / catalog switch renders as an immediate on/off toggle.
  if (r.component === "switch") {
    row.appendChild(current);
    const toggle = el("input");
    toggle.type = "checkbox";
    toggle.className = "c-switch";
    toggle.dataset.key = key;
    toggle.addEventListener("change", () =>
      writeValue(key, toggle.checked ? "1" : "0"));
    const ctrls = el("div");
    ctrls.style.display = "flex";
    ctrls.style.gap = "8px";
    ctrls.appendChild(toggle);
    row.appendChild(ctrls);
    return row;
  }

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
    input.placeholder = r.unit ? tf("ctl.placeholderUnit", { unit: r.unit }) : t("ctl.placeholder");
  }

  row.appendChild(current);

  const btn = el("button", "btn", t("btn.set"));
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
  document.querySelectorAll(".c-switch[data-key]").forEach((cb) => {
    const k = cb.dataset.key;
    if (k in flat) cb.checked = !!flat[k];
  });
  document.querySelectorAll(".c-current[data-key]").forEach((sp) => {
    const k = sp.dataset.key;
    if (!(k in flat)) return;
    const reg = registersByKey[k];
    if (reg && reg.component === "switch") {
      sp.textContent = t("ctl.current") + t(flat[k] ? "val.on" : "val.off");
      return;
    }
    sp.textContent = t("ctl.current") + fmtValue(flat[k]) + (reg && reg.unit ? " " + reg.unit : "");
  });
}

// writeRegister is the input+button flow: validate, disable, write.
async function writeRegister(key, value, btn) {
  if (value === "" || value === null || value === undefined) {
    toast(t("toast.enterValue"), "err");
    return;
  }
  if (btn) btn.disabled = true;
  try {
    await writeValue(key, value);
  } finally {
    if (btn) btn.disabled = false;
  }
}

// writeValue posts a single register write and toasts the result.
async function writeValue(key, value) {
  try {
    const res = await fetch("api/write", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ key, value }),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    toast(tf("toast.set", { key, value }), "ok");
  } catch (e) {
    toast(t("toast.errorPrefix") + e.message, "err");
  }
}

// ---------- config ----------
function renderConfig(c) {
  const items = [
    tile(t("cfg.modbus"), c.modbus_ip + ":" + c.modbus_port, null, t("cfg.slave") + " " + c.modbus_slave),
    tile(t("cfg.mqtt"), c.mqtt_server + ":" + c.mqtt_port, null, c.mqtt_topic),
    tile(t("cfg.hass"), c.hass_enable ? t("val.active") : t("val.off"), null, c.hass_base_topic),
    tile(t("cfg.refresh_now"), c.refresh_now + " " + t("unit.seconds"), null),
    tile(t("cfg.refresh_config"), c.refresh_config + " " + t("unit.seconds"), null),
    tile(t("cfg.refresh_day"), c.refresh_day + " " + t("unit.seconds"), null),
    tile(t("cfg.refresh_total"), c.refresh_total + " " + t("unit.seconds"), null),
    tile(t("cfg.refresh_static"), c.refresh_static + " " + t("unit.seconds"), null),
    tile(t("cfg.debug"), c.debug ? t("val.on") : t("val.off"), null),
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
  if (typeof v === "boolean") return v ? t("val.yes") : t("val.no");
  if (typeof v === "number") {
    if (Number.isInteger(v)) return v.toLocaleString(LOCALE);
    return v.toLocaleString(LOCALE, { maximumFractionDigits: 2 });
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
