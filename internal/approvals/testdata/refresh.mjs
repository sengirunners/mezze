// Runs web/app.js (copied next to this file by TestDecisionsRefreshKeepsWhatYouType)
// against a small fake DOM: types into a card's "your own words" box, lets the
// 30 s refresh fire, and checks the words, the focus and caret, a ticked box and
// the open folds all survive; a draft follows its group when the first
// ask is answered elsewhere, a box focused with no input for a minute stops
// holding refreshes off, and drafts and folds no longer in the view are
// forgotten. Exits 1 with the first failure.

class Text {
  constructor(data) {
    this.data = String(data);
    this.parent = null;
  }
  get textContent() {
    return this.data;
  }
}

class Elem {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.parent = null;
    this.listeners = {};
    this.attrs = {};
    this.className = "";
    this.value = "";
    this.selectionStart = 0;
    this.selectionEnd = 0;
  }
  get isConnected() {
    let n = this;
    while (n.parent) n = n.parent;
    return n === document.body;
  }
  get textContent() {
    return this.children.map((c) => c.textContent).join("");
  }
  set textContent(v) {
    this.replaceChildren(String(v));
  }
  append(...kids) {
    for (const k of kids) {
      const n = typeof k === "string" ? new Text(k) : k;
      if (n.parent) n.parent.children = n.parent.children.filter((c) => c !== n);
      n.parent = this;
      this.children.push(n);
    }
  }
  replaceChildren(...kids) {
    for (const c of this.children) c.parent = null;
    this.children = [];
    this.append(...kids);
    if (!document.activeElement.isConnected) document.activeElement = document.body; // as a browser does
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
  }
  addEventListener(type, f) {
    (this.listeners[type] ||= []).push(f);
  }
  dispatch(type) {
    for (const f of this.listeners[type] || []) f({ type, target: this });
  }
  click() {
    if (!this.disabled) this.dispatch("click");
  }
  *walk() {
    for (const c of this.children) {
      if (c instanceof Elem) {
        yield c;
        yield* c.walk();
      }
    }
  }
  querySelectorAll(tag) {
    return [...this.walk()].filter((n) => n.tagName === tag.toUpperCase());
  }
  focus(opts) {
    this.focusOpts = opts;
    if (!this.isConnected || document.activeElement === this) return;
    document.activeElement = this;
    this.dispatch("focus"); // as a browser does
  }
  blur() {
    if (document.activeElement === this) document.activeElement = document.body;
  }
  setSelectionRange(start, end) {
    this.selectionStart = start;
    this.selectionEnd = end;
  }
}

// The clock, moved on by hand for the "no input for a minute" fallback.
const realNow = Date.now;
let skew = 0;
Date.now = () => realNow() + skew;

const body = new Elem("body");
const docListeners = {};
globalThis.document = {
  body,
  activeElement: body,
  visibilityState: "visible",
  createElement: (tag) => new Elem(tag),
  createTextNode: (s) => new Text(s),
  getElementById: (id) => [body, ...body.walk()].find((n) => n.id === id) || null,
  addEventListener: (type, f) => (docListeners[type] ||= []).push(f),
};
for (const id of ["who", "msg", "decisions"]) {
  const n = new Elem(id === "decisions" ? "main" : "p");
  n.id = id;
  body.append(n);
}
const root = document.getElementById("decisions");
const msg = document.getElementById("msg");

const fail = (why) => {
  console.error("FAIL: " + why);
  process.exit(1);
};
const check = (ok, why) => ok || fail(why);

// The server: one card for the reader, one card for a program's contact
// (folded under "Outside decisions"), one answered ask.
const card = (id, extra = {}) => ({
  kind: "ask",
  project_slug: "zm",
  ask: {
    id,
    status: "open",
    asked_by: "dev-01",
    created: "2026-11-07T10:00:00Z",
    card: { question: `Which way for ${id}?`, choices: [{ key: "a", label: `A ${id}` }, { key: "b", label: `B ${id}` }], recommend: "a" },
    ...extra,
  },
});
const view = {
  identity: "ivo",
  node: "nPHONE",
  node_name: "ivo-phone",
  decisions: [
    card("ask-a"),
    card("ask-c", { for: { contact: "Pat", group: "Acme" } }),
    { kind: "ask", project_slug: "zm", ask: { id: "ask-old", status: "answered", card: { title: "Old one", question: "Old?", choices: [] } } },
  ],
};
let reads = 0;
let readReply = null; // a broken answer for the next read, else the view
let decideReply = { ok: true, body: {} };
const posts = [];
globalThis.fetch = async (url, opts = {}) => {
  if (url === "/v0/approvals/device") {
    reads++;
    const r = readReply;
    readReply = null;
    return r || { ok: true, statusText: "OK", json: async () => structuredClone(view) };
  }
  posts.push({ url, body: JSON.parse(opts.body) });
  const r = decideReply;
  return { ok: r.ok, statusText: r.ok ? "OK" : "Internal Server Error", json: async () => r.body };
};
let tick = null;
globalThis.setInterval = (f, ms) => {
  check(ms === 30000, `refresh every ${ms} ms, want 30000`);
  tick = f;
};
const settle = async () => {
  for (let i = 0; i < 20; i++) await new Promise((r) => setTimeout(r, 0));
};

const cardOf = (id) => [...root.walk()].find((n) => n.tagName === "SECTION" && n.textContent.includes(`Which way for ${id}?`));
const boxOf = (id) => cardOf(id)?.querySelectorAll("textarea")[0];
const buttonOf = (id, label) => cardOf(id)?.querySelectorAll("button").find((b) => b.textContent.startsWith(label));
const foldsOpen = () => root.querySelectorAll("details").filter((d) => d.open).map((d) => d.children[0].textContent);

const app = await import(process.argv[2]);
await settle();
check(tick, "app.js set no refresh");
check(reads === 1 && boxOf("ask-a"), "first load did not draw ask-a's card");

// 1. Ivo types mid-sentence; the 30 s refresh fires; a new ask came in.
const words = "I want the second one, but only if";
const box = boxOf("ask-a");
box.focus();
box.value = words;
box.setSelectionRange(17, 17);
box.dispatch("input");
view.decisions.push(card("ask-b"));
tick();
await settle();
check(box.isConnected, "the refresh rebuilt the card under the person typing");
check(document.activeElement === box, "the refresh took the focus from the box");
check(box.value === words, `the refresh changed the typed words to ${JSON.stringify(box.value)}`);

// 2. A render while typing (a tap's own refresh) keeps words, focus and caret.
app.render(structuredClone(view));
const again = boxOf("ask-a");
check(again && again !== box, "render did not rebuild the card");
check(again.value === words, `after a render the box holds ${JSON.stringify(again.value)}, want the typed words`);
check(document.activeElement === again, "after a render the box lost the focus");
check(again.selectionStart === 17 && again.selectionEnd === 17, `caret at ${again.selectionStart}-${again.selectionEnd}, want 17`);
check(cardOf("ask-b"), "render did not draw the new ask");

// 3. Away from the box: open the folds, tick "the contact said this", refresh.
again.blur();
for (const d of root.querySelectorAll("details")) {
  d.open = true;
  d.dispatch("toggle");
}
const opened = foldsOpen();
check(opened.length === 3, `opened folds ${JSON.stringify(opened)}, want 3`);
const said = cardOf("ask-c").querySelectorAll("input")[0];
said.checked = true;
said.dispatch("change");
const before = reads;
tick();
await settle();
check(reads === before + 1, "the refresh did not run once the person left the box");
check(boxOf("ask-a") !== again && boxOf("ask-a").value === words, "a refresh lost the unsent words");
check(cardOf("ask-c").querySelectorAll("input")[0].checked, "a refresh cleared 'the contact said this'");
check(JSON.stringify(foldsOpen()) === JSON.stringify(opened), `folds open after refresh ${JSON.stringify(foldsOpen())}, want ${JSON.stringify(opened)}`);

// 4. A refused answer keeps the words and shows why.
decideReply = { ok: false, body: { error: { message: "zm is busy" } } };
buttonOf("ask-a", "Answer in my own words").click();
await settle();
check(posts.at(-1).body.text === words, `sent ${JSON.stringify(posts.at(-1).body)}, want the typed words`);
check(msg.textContent === "zm is busy", `message ${JSON.stringify(msg.textContent)}, want the refusal`);
check(boxOf("ask-a").value === words, "a refused answer lost the words");

// 5. A taken answer clears its draft (the card shows empty if it is still listed).
decideReply = { ok: true, body: {} };
buttonOf("ask-a", "Answer in my own words").click();
await settle();
check(msg.textContent === "", `message ${JSON.stringify(msg.textContent)} after a taken answer`);
check(boxOf("ask-a").value === "", "a taken answer left its words in the box");

// 6. A draft lives with its group: the first ask is answered on another
// device and the ask joined to it (same_as) becomes the card.
const first = card("ask-g1");
view.decisions.push(first, card("ask-g2", { same_as: "ask-g1", card: first.ask.card }));
tick();
await settle();
check(cardOf("ask-g1")?.textContent.includes("asked 2 times"), "the joined asks did not draw as one card");
const groupWords = "keep it, but tell them why";
let g = boxOf("ask-g1");
g.focus();
g.value = groupWords;
g.setSelectionRange(5, 5);
g.dispatch("input");
first.ask.status = "answered";
app.render(structuredClone(view));
g = boxOf("ask-g1");
check(g && !cardOf("ask-g1").textContent.includes("asked 2 times"), "the joined ask did not take over the card");
check(g.value === groupWords, `after the first ask was answered elsewhere the box holds ${JSON.stringify(g.value)}, want the typed words`);
check(document.activeElement === g && g.selectionStart === 5, "the joined ask's card did not get the focus and caret back");
decideReply = { ok: true, body: {} };
buttonOf("ask-g1", "Answer in my own words").click();
await settle();
check(posts.at(-1).url === "/v0/asks/ask-g2/decide" && posts.at(-1).body.text === groupWords, `sent ${JSON.stringify(posts.at(-1))}, want ask-g2 with the typed words`);
check(boxOf("ask-g1").value === "", "a taken answer left the group's words in the box");

// 7. Focus with no input for a minute (an iPhone back from the background,
// no keyboard): the refresh runs anyway and puts the focus and caret back.
const idle = boxOf("ask-b");
idle.focus();
idle.value = "half a thought";
idle.setSelectionRange(4, 4);
idle.dispatch("input");
let n = reads;
skew += 59000;
tick();
await settle();
check(reads === n && boxOf("ask-b") === idle, "the refresh ran within a minute of the last keystroke");
skew += 2000;
tick();
await settle();
const back = boxOf("ask-b");
check(reads === n + 1 && back !== idle, "no refresh after a minute with focus and no input");
check(back.value === "half a thought" && document.activeElement === back && back.selectionStart === 4, "the idle refresh lost the words, focus or caret");
check(back.focusOpts?.preventScroll === true, "putting the focus back can scroll the page to the box");
tick();
await settle();
check(reads === n + 2, "the focus a render put back counted as input: refreshes held off again");
// Back from the background (no idle minute waited out): the first refresh runs.
back.dispatch("input");
const hide = (state) => {
  document.visibilityState = state;
  for (const f of docListeners.visibilitychange || []) f();
};
hide("hidden");
hide("visible");
await settle();
check(reads === n + 3 && boxOf("ask-b") !== back && document.activeElement === boxOf("ask-b"), "coming back to the page did not refresh under a box with focus");
boxOf("ask-b").blur();
skew += 61000;

// An unreadable answer is an error, never an empty view that forgets drafts.
readReply = { ok: true, statusText: "OK", json: async () => JSON.parse("<html>") };
n = reads;
tick();
await settle();
check(reads === n + 1 && boxOf("ask-b")?.value === "half a thought", "an unreadable answer emptied the page");
check(msg.textContent !== "", "an unreadable answer showed no error");
tick();
await settle();
check(boxOf("ask-b").value === "half a thought", "an unreadable answer forgot a draft");

// 8. Drafts and folds no longer in the view are forgotten, but not while a
// source failed to load (its asks are missing, not answered).
const [askB] = view.decisions.splice(view.decisions.findIndex((d) => d.ask.id === "ask-b"), 1);
view.skipped = [{ source: "work", error: "timed out" }];
tick();
await settle();
check(!cardOf("ask-b"), "ask-b still drawn after it left the view");
view.decisions.push(askB);
delete view.skipped;
tick();
await settle();
check(boxOf("ask-b").value === "half a thought", "a draft was forgotten while a source failed to load");
view.decisions.splice(view.decisions.indexOf(askB), 1);
const [askC] = view.decisions.splice(view.decisions.findIndex((d) => d.ask.id === "ask-c"), 1);
tick();
await settle();
view.decisions.push(askB, askC);
tick();
await settle();
check(boxOf("ask-b").value === "", `a draft for an ask that left the view came back: ${JSON.stringify(boxOf("ask-b").value)}`);
check(!foldsOpen().some((f) => f.startsWith("Outside decisions")), `folds for asks that left the view came back open: ${JSON.stringify(foldsOpen())}`);

console.log("ok");
process.exit(0);
