// The Decisions page for an active approver device: every decision waiting
// on this daemon's owner (note cards, pull requests, work), answered with
// POST /v0/asks/{id}/decide from this device. Grouping and wording come from
// the Mezze app's own Decisions code (zm-decisions.js, generated from t3code's
// packages/zm-client). Every value is set as text: data is never HTML.
import { askDefault, askHeadline, asksForReader, groupAsks, isPullRequestAsk, pullRequestSubject, workAction } from "./zm-decisions.js";

const root = document.getElementById("decisions");
const who = document.getElementById("who");
const msg = document.getElementById("msg");

function el(tag, attrs, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") n.className = v;
    else n[k] = v;
  }
  for (const kid of kids) {
    if (kid === undefined || kid === null || kid === false || kid === "") continue;
    n.append(typeof kid === "string" ? document.createTextNode(kid) : kid);
  }
  return n;
}

let busy = false;

// A render rebuilds every card, so what the person typed or ticked lives
// here until zm takes their answer: a refresh never loses a draft. A draft is
// keyed by its group (the first ask's id: an ask joined to it names it in
// `same_as`), so when the first is answered elsewhere and a joined ask becomes
// the card, the words stay. Folds keep their open state the same way.
const drafts = new Map();
const openFolds = new Set();
let boxes = new Map(); // draft key → this render's "your own words" box
const groupId = (a) => a.same_as || a.id;
// A box can keep focus with no keyboard (an iPhone PWA back from the
// background), so focus with no input for a minute stops holding refreshes off.
const IDLE_MS = 60000;
let lastInput = 0;
const typing = () => [...boxes.values()].includes(document.activeElement) && Date.now() - lastInput < IDLE_MS;

// `force`: render even while the person is typing (after their own tap).
// `keepMsg`: keep the message a tap just wrote.
async function load(how = {}) {
  if (busy) return;
  // The periodic refresh never rebuilds a card under someone typing: it took
  // the iPhone keyboard (and dictation) away mid-sentence. The next refresh
  // after they leave the box catches up.
  if (!how.force && typing()) return;
  try {
    const r = await fetch("/v0/approvals/device", { credentials: "same-origin", cache: "no-store" });
    const j = await r.json().catch(() => null);
    if (!r.ok || !j) {
      // An unreadable answer is an error, never an empty view (that would
      // forget every draft).
      msg.textContent = (j && j.error && j.error.message) || (r.ok ? "zm sent an answer this page can't read" : r.statusText);
      return;
    }
    if (!how.force && (busy || typing())) return; // they tapped or started typing while it loaded
    if (!how.keepMsg) msg.textContent = "";
    render(j);
  } catch (e) {
    msg.textContent = "Can't reach zm: " + String(e);
  }
}

// `key`: the draft this answer sends, dropped once zm takes it.
async function decide(ask, body, card, verb = "decide", key = groupId(ask)) {
  msg.textContent = "";
  busy = true;
  for (const b of card.querySelectorAll("button")) b.disabled = true;
  try {
    const r = await fetch("/v0/asks/" + encodeURIComponent(ask.id) + "/" + verb, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "Potluck-Protocol": "potluck/1" },
      body: JSON.stringify(body),
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) msg.textContent = (j.error && j.error.message) || r.statusText;
    else {
      if (verb === "decide") drafts.delete(key);
      if (j.mail_error) msg.textContent = "Decided. " + j.mail_error;
    }
  } catch (e) {
    msg.textContent = String(e);
  } finally {
    busy = false;
    load({ force: true, keepMsg: true });
  }
}

// Got it: the person's own action, the only thing that marks a for-you item
// read (zm SPEC 9 "For you").
async function gotIt(mail, card) {
  msg.textContent = "";
  busy = true;
  for (const b of card.querySelectorAll("button")) b.disabled = true;
  try {
    const r = await fetch("/v0/approvals/got-it", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "Potluck-Protocol": "potluck/1" },
      body: JSON.stringify({ id: mail.id }),
    });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) msg.textContent = (j.error && j.error.message) || r.statusText;
  } catch (e) {
    msg.textContent = String(e);
  } finally {
    busy = false;
    load({ force: true, keepMsg: true });
  }
}

function knowCard(m) {
  const sent = m.created ? new Date(m.created).toLocaleString([], { weekday: "short", hour: "numeric", minute: "2-digit" }) : "";
  const card = el(
    "section",
    { class: "card" },
    el("div", { class: "muted" }, "From ", m.from || "someone", sent ? " · " + sent : "", m.ref ? " · " + m.ref : ""),
    el("div", { class: "body" }, m.body || ""),
  );
  const b = el("button", { class: "decide approve", type: "button" }, "Got it");
  b.addEventListener("click", () => gotIt(m, card));
  card.append(el("div", { class: "row" }, b));
  return card;
}

const where = (entry) => [entry.project_slug, entry.ask && entry.ask.area].filter(Boolean).join("/");

function prCard(entry, more) {
  const a = entry.ask;
  const card = el(
    "section",
    { class: "card pr" },
    el("div", { class: "muted" }, where(entry), " · pull request · from ", a.asked_by || "an agent", urgent(a), more),
    el("div", { class: "subj" }, pullRequestSubject(a) || ""),
    el("div", { class: "body" }, a.body || ""),
    el("p", { class: "muted" }, "Decided by this device: its signed receipt is what the merge gate checks."),
  );
  const row = el("div", { class: "row" });
  for (const opt of a.options && a.options.length ? a.options : ["approve", "decline"]) {
    const b = el("button", { class: "decide " + opt, type: "button" }, opt);
    b.addEventListener("click", () => decide(a, { decision: opt }, card));
    row.append(b);
  }
  card.append(row);
  return card;
}

function noteCard(entry, more, key) {
  const a = entry.ask;
  const c = a.card;
  const card = el(
    "section",
    { class: "card" },
    el("div", { class: "muted" }, where(entry), " · from ", a.asked_by || "an agent", urgent(a), more),
    a.for ? el("div", { class: "muted" }, "For ", a.for.contact || "", " at ", a.for.group || "") : null,
    a.subject && a.subject.work ? el("div", { class: "subj" }, a.subject.work) : null,
    el("h2", {}, askHeadline(a)),
    c.context ? el("p", {}, c.context) : null,
    c.background ? el("details", { class: "muted" }, el("summary", {}, "More"), el("p", {}, c.background)) : null,
    el("p", {}, el("b", {}, c.question || "")),
  );
  // A card the card judge could not check is held: nobody it is for was asked. It cannot be
  // answered; this device (a paired approver) may release it, as it is (SPEC 16 "Card judge").
  if (a.not_checked) {
    card.append(
      el("p", { class: "default" }, el("b", {}, "Not checked: "), "the checker could not read this card" +
        (a.not_checked_reason ? " (" + a.not_checked_reason + ")" : "") + ". Nobody it is for has been asked yet; it goes out on its own once the check passes."),
    );
    const rel = el("button", { class: "decide", type: "button" }, "Release it unchecked");
    rel.addEventListener("click", () => {
      if (confirm("Send this card as it is, without the check?")) decide(a, { decision: "release-unchecked" }, card, "decide", key);
    });
    card.append(el("div", { class: "row" }, rel));
    return card;
  }
  // A default that applies at the deadline (SPEC 16 "Defaults with deadlines").
  const dflt = askDefault(a);
  if (dflt) {
    card.append(el("p", { class: "default" }, el("b", {}, dflt.line)));
    if (dflt.kind === "applied") {
      const undo = el("button", { class: "decide", type: "button" }, "Undo");
      undo.addEventListener("click", () => decide(a, {}, card, "device-undo"));
      const keep = el("button", { class: "decide", type: "button" }, "Keep it");
      keep.addEventListener("click", () => decide(a, { decision: dflt.recommend }, card, "decide", key));
      card.append(el("div", { class: "row" }, undo, keep));
    }
  }
  const draft = drafts.get(key) || { text: "", asContact: false };
  const text = el("textarea", { rows: 2, maxLength: 2000, placeholder: "Your own words (optional)", value: draft.text });
  text.setAttribute("aria-label", "your own words");
  text.addEventListener("focus", () => (lastInput = Date.now()));
  text.addEventListener("input", () => {
    lastInput = Date.now();
    drafts.set(key, Object.assign(draft, { text: text.value }));
  });
  boxes.set(key, text);
  let asContact = null;
  // A risky action (SPEC 16 "Risky actions": it cannot be undone or spends
  // money): its choice stays off until the card's word is typed here.
  const risky = c.action && c.action.risk ? c.action : null;
  let word = null;
  let riskyButton = null;
  const wordTyped = () => word && word.value.trim() === risky.confirm_word;
  if (risky) {
    word = el("input", { type: "text", autocomplete: "off", value: draft.confirm || "",
      placeholder: risky.confirm_word, style: "text-transform:none;width:100%;letter-spacing:normal;box-sizing:border-box" });
    word.setAttribute("autocapitalize", "off");
    word.setAttribute("spellcheck", "false");
    boxes.set(key + "\u0000word", word); // a refresh never rebuilds it under someone typing
    word.setAttribute("aria-label", `type ${risky.confirm_word} to choose ${risky.on.toUpperCase()}`);
    word.addEventListener("focus", () => (lastInput = Date.now()));
    word.addEventListener("input", () => {
      lastInput = Date.now();
      drafts.set(key, Object.assign(draft, { confirm: word.value }));
      if (riskyButton && !busy) riskyButton.disabled = !wordTyped();
    });
  }
  const withWords = (body) => {
    if (text.value.trim()) body.text = text.value.trim();
    if (a.for) body.as = asContact.checked ? "contact" : "owner";
    if (risky && body.decision === risky.on) body.confirm = word.value.trim();
    return body;
  };
  for (const ch of c.choices || []) {
    const rec = ch.key === c.recommend;
    const b = el("button", { class: "decide choice-btn" + (rec ? " rec" : ""), type: "button" }, `${ch.key}) ${ch.label}`,
      rec ? el("span", { class: "tag" }, "recommended") : null);
    if (risky && ch.key === risky.on) {
      riskyButton = b;
      b.disabled = !wordTyped();
    }
    b.addEventListener("click", () => decide(a, withWords({ decision: ch.key }), card, "decide", key));
    card.append(el("div", { class: "choice" }, b,
      ch.effect ? el("div", {}, ch.effect) : null,
      ch.pros ? el("div", { class: "muted" }, "Pros: ", ch.pros) : null,
      ch.cons ? el("div", { class: "muted" }, "Cons: ", ch.cons) : null));
  }
  if (c.why) card.append(el("p", { class: "muted" }, "Why recommended: ", c.why));
  if (c.after_press) card.append(el("p", { class: "muted" }, "After you press: ", c.after_press));
  if (a.action_text) card.append(el("p", {}, el("b", {}, a.action_text)));
  else if (c.action)
    card.append(el("p", {}, el("b", {}, `If you choose ${c.action.on}: zm acks ${c.action.count} mails as you.`),
      " Any other answer does nothing."));
  const st = c.owner_step;
  if (st && /^https:\/\//.test(st.link) && st.domain)
    card.append(el("p", {}, el("a", { href: st.link, target: "_blank", rel: "noopener noreferrer" }, `Open ${st.domain}`)));
  if (risky)
    card.append(el("label", {}, `Type ${risky.confirm_word} to choose ${risky.on.toUpperCase()}:`, word));
  card.append(text);
  if (a.for) {
    asContact = el("input", { type: "checkbox", checked: draft.asContact });
    asContact.addEventListener("change", () => drafts.set(key, Object.assign(draft, { asContact: asContact.checked })));
    card.append(el("label", { class: "muted" }, asContact, ` ${a.for.contact || "The contact"} said this, not me`));
  }
  const own = el("button", { class: "decide", type: "button" }, "Answer in my own words");
  own.addEventListener("click", () => {
    if (!text.value.trim()) return text.focus();
    decide(a, withWords({ decision: "other" }), card, "decide", key);
  });
  const needMore = el("button", { class: "decide", type: "button" }, "Need more context");
  needMore.addEventListener("click", () => {
    if (!text.value.trim()) return text.focus(); // the note says what is missing
    decide(a, withWords({ decision: "needs_context" }), card, "decide", key);
  });
  card.append(el("div", { class: "row" }, own, needMore));
  return card;
}

function workCard(entry, reader) {
  const w = entry.work;
  const step = workAction(w, reader).kind === "step";
  return el(
    "section",
    { class: "card work" },
    el("div", { class: "muted" }, entry.project_slug || "", " · ", step ? "a step only you can run" : "a decision nobody wrote options for"),
    el("div", { class: "subj" }, w.id || ""),
    el("h2", {}, w.title || "Work item"),
    el("p", { class: "muted" }, step ? "Do it, then close the item." : "Ask its agent to file it as a card."),
  );
}

// Most urgent first: a deadline (soonest), then priority (0 first), then
// oldest (zm SPEC 16 "Inbox"). A group sorts by its most urgent ask.
const due = (a) => (a.by ? Date.parse(a.by) : Infinity);
function urgency(x, y) {
  return due(x) - due(y) || (x.priority ?? 2) - (y.priority ?? 2) || Date.parse(x.created || 0) - Date.parse(y.created || 0);
}
const lead = (g) => [g.primary, ...g.duplicates].map((d) => d.ask).sort(urgency)[0];
const byUrgency = (groups) => groups.sort((x, y) => urgency(lead(x), lead(y)));

// "P1 · by Fri 3:00 PM" for an ask that is more than routine, else "".
function urgent(a) {
  const when = a.by ? "by " + new Date(a.by).toLocaleString([], { weekday: "short", hour: "numeric", minute: "2-digit" }) : "";
  const p = (a.priority ?? 2) < 2 ? "P" + a.priority : "";
  return [p, when].filter(Boolean).map((t) => " · " + t).join("");
}

function askCard(g) {
  const more = g.duplicates.length ? ` · asked ${g.duplicates.length + 1} times` : "";
  const a = g.primary.ask;
  if (isPullRequestAsk(a)) return prCard(g.primary, more);
  // The group's draft, under whichever of its asks it was typed (the first
  // ask may have been answered elsewhere, or a newer one leads the group now).
  const keys = [g.primary, ...g.duplicates].map((d) => groupId(d.ask));
  return a.card ? noteCard(g.primary, more, keys.find((k) => drafts.has(k)) ?? keys[0]) : null;
}

let folds = new Set(); // this render's fold keys
// A collapsed section that stays open across refreshes once opened.
function fold(key, cls, summary) {
  folds.add(key);
  const d = el("details", { class: cls, open: openFolds.has(key) }, el("summary", {}, summary));
  d.addEventListener("toggle", () => (d.open ? openFolds.add(key) : openFolds.delete(key)));
  return d;
}

// One collapsed section titled `title`, holding one collapsed group per key
// (the one with the most urgent ask first, then the biggest), each its cards
// most urgent first.
function folded(out, entries, keyOf, title) {
  if (!entries.length) return;
  const by = new Map();
  for (const d of entries) {
    const k = keyOf(d);
    if (!by.has(k)) by.set(k, []);
    by.get(k).push(d);
  }
  const groups = [...by]
    .map(([k, list]) => [k, byUrgency(groupAsks(list))])
    .sort((x, y) => urgency(lead(x[1][0]), lead(y[1][0])) || y[1].length - x[1].length);
  const total = groups.reduce((n, [, gs]) => n + gs.length, 0);
  const sec = fold(title, "fold", `${title} (${total})`);
  for (const [k, gs] of groups) {
    const sub = fold(title + "\u0000" + k, "fold sub", `${k} (${gs.length})`);
    for (const g of gs) {
      const card = askCard(g);
      if (card) sub.append(card);
    }
    sec.append(sub);
  }
  out.push(sec);
}

// Phone alerts (zm SPEC 16 "Phone alerts"): this daemon buzzes this device for
// its person's new decisions and news, whether or not any Mac is awake.
const b64url = (s) => {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
};

async function post(path, body) {
  const r = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "Potluck-Protocol": "potluck/1" },
    body: JSON.stringify(body),
  });
  const j = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error((j.error && j.error.message) || r.statusText);
  return j;
}

async function alertsState() {
  if (!("serviceWorker" in navigator) || !("PushManager" in window)) return { can: false };
  const reg = await navigator.serviceWorker.register("/v0/approvals/sw.js");
  const sub = await reg.pushManager.getSubscription();
  return { can: true, reg, sub };
}

async function setAlerts(on, button) {
  msg.textContent = "";
  button.disabled = true;
  try {
    const st = await alertsState();
    if (!st.can) {
      msg.textContent = "This browser can't show alerts. On iPhone, add this page to the Home Screen and open it from there.";
      return;
    }
    if (on) {
      if ((await Notification.requestPermission()) !== "granted") {
        msg.textContent = "Alerts are blocked for this page in the phone's settings.";
        return;
      }
      const r = await fetch("/v0/approvals/push-key", { credentials: "same-origin", cache: "no-store" });
      const j = await r.json();
      if (!r.ok) throw new Error((j.error && j.error.message) || r.statusText);
      const sub = st.sub || (await st.reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64url(j.public_key) }));
      await post("/v0/approvals/push", sub.toJSON());
    } else if (st.sub) {
      await post("/v0/approvals/push/off", st.sub.toJSON());
      await st.sub.unsubscribe();
    }
  } catch (e) {
    msg.textContent = "Alerts: " + String(e.message || e);
  } finally {
    button.disabled = false;
    drawAlerts();
  }
}

async function drawAlerts() {
  const slot = document.getElementById("alerts");
  if (!slot) return;
  let st = { can: false };
  try {
    st = await alertsState();
  } catch {
    st = { can: false };
  }
  if (!st.can) {
    slot.replaceChildren(el("p", { class: "muted" }, "Alerts: add this page to the Home Screen to get them."));
    return;
  }
  const on = Boolean(st.sub);
  const b = el("button", { class: "decide", type: "button" }, on ? "Turn off alerts" : "Turn on alerts");
  b.addEventListener("click", () => setAlerts(!on, b));
  slot.replaceChildren(el("div", { class: "row" }, b, el("span", { class: "muted" }, on ? " Alerts are on for this phone." : " Get a buzz for each new decision or news for you.")));
}

export function render(v) {
  let focus = null;
  for (const [id, t] of boxes) if (t === document.activeElement) focus = { id, start: t.selectionStart, end: t.selectionEnd };
  boxes = new Map();
  folds = new Set();
  const reader = [v.identity].filter(Boolean);
  who.replaceChildren(
    el("b", {}, v.identity || "?"),
    v.github ? ` · GitHub @${v.github}` : "",
    ` · this device: ${v.node_name || v.node}`,
  );
  const all = v.decisions || [];
  const open = all.filter((d) => d.kind === "ask" && d.ask && d.ask.status === "open");
  const answered = all.filter((d) => d.kind === "ask" && d.ask && d.ask.status !== "open");
  const work = all.filter((d) => d.kind === "work" && d.work);
  // Yours first. An outside party's decision (an ask `for` a program's
  // contact) and one aimed at someone else fold away, one section per
  // program or person, so they never bury yours.
  const mine = new Set(asksForReader(open, reader));
  const yours = open.filter((d) => mine.has(d) && !d.ask.for);
  const others = open.filter((d) => !mine.has(d));
  const outside = open.filter((d) => mine.has(d) && d.ask.for);
  const groups = byUrgency(groupAsks(yours));
  const out = [el("h3", {}, groups.length ? `To decide (${groups.length})` : "Nothing for you to decide")];
  for (const g of groups) out.push(askCard(g));
  folded(out, outside, (d) => d.ask.for.group || "A program", "Outside decisions");
  folded(out, others, (d) => "For " + d.owner_name, "For someone else");
  const fyi = v.for_you || [];
  if (fyi.length) {
    out.push(el("h3", {}, `For you (${fyi.length})`));
    for (const m of fyi) out.push(knowCard(m));
  }
  if (work.length) {
    out.push(el("h3", {}, `Waiting on you (${work.length})`));
    for (const w of work) out.push(workCard(w, reader));
  }
  if (answered.length) {
    const list = fold("\u0000answered", undefined, `Answered (${answered.length})`);
    for (const d of answered.slice(0, 30)) {
      const dec = d.ask.decision;
      list.append(el("div", { class: "card done" }, el("div", { class: "muted" }, where(d)), el("div", {}, askHeadline(d.ask)),
        dec ? el("div", { class: "muted" }, "→ ", dec.answer || dec.decision || "", " · ", dec.approver || "") : null));
    }
    out.push(list);
  }
  for (const s of v.skipped || []) out.push(el("p", { class: "muted" }, `${s.source}: ${s.error}`));
  root.replaceChildren(el("div", { id: "alerts" }), ...out);
  const box = focus && boxes.get(focus.id);
  if (box) {
    const t = lastInput; // putting the focus back is not the person's input
    box.focus({ preventScroll: true }); // a refresh never scrolls back to the box
    lastInput = t;
    box.setSelectionRange(focus.start, focus.end);
  }
  // Forget drafts and folds no longer in the view, unless a source failed to
  // load this time (its asks are missing, not answered).
  if (!(v.skipped || []).length) {
    const live = new Set(open.map((d) => groupId(d.ask)));
    for (const k of drafts.keys()) if (!live.has(k)) drafts.delete(k);
    for (const k of openFolds) if (!folds.has(k)) openFolds.delete(k);
  }
  drawAlerts();
}

if (root) {
  document.addEventListener("visibilitychange", () => {
    // Back from the background the box may hold focus with no keyboard: the
    // first refresh runs rather than waiting out the idle minute.
    if (document.visibilityState === "visible") load();
    else lastInput = 0;
  });
  setInterval(() => document.visibilityState === "visible" && load(), 30000);
  load();
}
