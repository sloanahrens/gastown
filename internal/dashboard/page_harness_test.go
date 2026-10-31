package dashboard

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// index.html is the town's page: its render* functions decide what an operator
// reads — a word, a colour, whether a marker is hidden. Reading the file as a
// string checks that a function mentions the right names, not that it draws
// anything, so two om reviews of gt-t9w60 and the bead behind this file
// (gt-tq4ee) found page tests that would pass over a page drawing nothing.
//
// pageDOM runs the page's own script in a pure-Go JavaScript engine under the
// DOM stub below: a test calls a render function with the reading the server
// would send and reads back the element it drew. The engine is a library, not
// a tool, so the unit tier stays hermetic — nothing here starts a process,
// sleeps or touches the network, and the clock the page reads is frozen when
// the stub loads and moves only under __advance.

// pageStubJS is the browser the page runs in: the DOM surface index.html's
// script uses, and nothing more. A render function only builds elements, so
// createElement, append, textContent, className, classList, hidden and title
// carry it, and focus is where document.activeElement moves; anything a later
// test needs beyond that is added here with the test that needs it.
//
// Two deliberate refusals keep the stub honest about the page. getElementById
// answers only for the ids index.html's markup carries (__ids, read from the
// file), so a render into an id the markup lost fails the test instead of
// quietly passing against a stub that invents elements. And time is the test's:
// Date.now reads the fake clock (__epoch is the real one, read once when the
// stub loads) and the clock only moves under __advance, so nothing redraws or
// reconnects behind a test that only reads.
const pageStubJS = `
var __byId = {};
__ids.forEach(function (id) { __byId[id] = __el("div"); });
// __sections is the page's own grid, built from the markup: one element per
// section, each carrying the heading the page's fold reads and the note span
// the page draws into. The elements are the same objects getElementById hands
// back, so a test that sets a note through the page reads it back inside the
// heading it sits in.
var __sections = [];
__sectionSpec.forEach(function (sp) {
  var sec = __el("section");
  if (sp.id) { sec.id = sp.id; __byId[sp.id] = sec; }
  sec.dataset.fold = sp.key;
  if (sp.defaultCollapsed) sec.setAttribute("data-default-collapsed", "");
  sec.hidden = !!sp.hidden;
  var h = __el("h2");
  sp.head.forEach(function (part) {
    if (part.id) {
      var s = __byId[part.id] || __el("span");
      if (part.class) s.className = part.class;
      __byId[part.id] = s;
      h.append(s);
    } else if (part.text) {
      h.append(__textNode(part.text));
    }
  });
  sec.append(h);
  __sections.push(sec);
});
function __cls(e, c) { return (" " + (e.className || "") + " ").indexOf(" " + c + " ") >= 0; }
function __walk(e, fn) {
  for (var i = 0; i < (e.children || []).length; i++) { fn(e.children[i]); __walk(e.children[i], fn); }
}
// __rowCount is how many table rows sit under an element: the height a scroll
// range is measured from, since the stub draws nothing.
function __rowCount(e) {
  var n = 0;
  __walk(e, function (c) { if (c.tagName === "TR") n++; });
  return n;
}
// __find answers the two selectors the page's fold asks of an element: a direct
// child by tag (":scope > h2") and the first descendant with a class (".r",
// ".caret", ".foldsum"). Anything else is null, so a selector the stub does not
// model fails the test that needed it instead of answering wrongly.
function __find(e, sel) {
  sel = String(sel).trim();
  if (sel.indexOf(":scope > ") === 0) {
    var tag = sel.slice(9).trim().toLowerCase();
    for (var i = 0; i < (e.children || []).length; i++) {
      var c = e.children[i];
      if (c.tagName && c.tagName.toLowerCase() === tag) return c;
    }
    return null;
  }
  if (sel.charAt(0) === ".") {
    var cls = sel.slice(1), out = null;
    __walk(e, function (c) { if (!out && __cls(c, cls)) out = c; });
    return out;
  }
  return null;
}
function __findAll(e, sel) { var one = __find(e, sel); return one ? [one] : []; }
// __fireOn is the browser delivering one of its own events to an element — a
// click, a keydown — to the listeners addEventListener collected. __fire below
// is the server's events on a stream, which carry JSON in their data field.
function __fireOn(e, type, ev) {
  var ls = (e._listeners && e._listeners[type]) || [];
  for (var i = 0; i < ls.length; i++) ls[i](ev || { type: type, target: e });
}
function __el(tag) {
  var e = {
    tagName: String(tag).toUpperCase(), children: [], _text: "",
    className: "", title: "", hidden: false, tabIndex: 0,
    dataset: {},
    // The Deploys block sets its ch-based column widths through style, so the
    // stub keeps what the page writes there.
    style: null,
    // The stub has no layout, so the scroll range is modelled the way a browser
    // clamps it: a row is 24px tall, the viewport is 200px, and a scrollTop write
    // lands inside [0, scrollHeight - clientHeight]. A write past the end reads
    // back at the end, and a write made before the rows are drawn reads back at 0.
    _scrollTop: 0,
    get scrollHeight() { return __rowCount(this) * 24; },
    get clientHeight() { return 200; },
    get scrollTop() { return this._scrollTop; },
    set scrollTop(v) {
      var max = Math.max(0, this.scrollHeight - this.clientHeight);
      this._scrollTop = Math.min(Math.max(0, Math.round(Number(v) || 0)), max);
    },
    // parentNode is what closest walks: the browser fills it in on append, and
    // the fold is the one handler that asks what a click landed on.
    parentNode: null,
    get childElementCount() { return this.children.length; },
    get firstChild() { return this.children.length ? this.children[0] : null; },
    get textContent() {
      if (this.children.length) {
        var s = "";
        for (var i = 0; i < this.children.length; i++) s += this.children[i].textContent;
        return s;
      }
      return this._text;
    },
    set textContent(v) { this.children = []; this._text = v == null ? "" : String(v); },
    append: function () { for (var i = 0; i < arguments.length; i++) { arguments[i].parentNode = this; this.children.push(arguments[i]); } },
    appendChild: function (n) { n.parentNode = this; this.children.push(n); return n; },
    prepend: function () { for (var i = arguments.length - 1; i >= 0; i--) { arguments[i].parentNode = this; this.children.unshift(arguments[i]); } },
    replaceChildren: function () {
      this.children = [];
      this._text = "";
      for (var i = 0; i < arguments.length; i++) { arguments[i].parentNode = this; this.children.push(arguments[i]); }
    },
    remove: function () {},
    focus: function () { document.activeElement = this; },
    setAttribute: function (k, v) { this["attr_" + k] = v; },
    getAttribute: function (k) { return this["attr_" + k]; },
    // An attribute the page set, or the markup declared on a section the stub
    // built; hasAttribute is how a pane opts out of the fold's default list.
    hasAttribute: function (k) { return Object.prototype.hasOwnProperty.call(this, "attr_" + k); },
    addEventListener: function (type, fn) {
      if (!this._listeners) this._listeners = {};
      (this._listeners[type] = this._listeners[type] || []).push(fn);
    },
    removeEventListener: function (type, fn) {
      if (!this._listeners || !this._listeners[type]) return;
      this._listeners[type] = this._listeners[type].filter(function (f) { return f !== fn; });
    },
    querySelector: function (sel) { return __find(this, sel); },
    querySelectorAll: function (sel) { return __findAll(this, sel); },
    // closest answers a comma-separated list of tag names and classes, which is
    // the shape the fold's "did this click land on a control" question takes.
    // Any other selector is null, so a test that needs one fails rather than
    // matching an ancestor it should not.
    closest: function (sel) {
      var parts = String(sel).split(",").map(function (s) { return s.trim(); }).filter(Boolean);
      var n = this;
      while (n) {
        for (var i = 0; i < parts.length; i++) {
          var p = parts[i];
          if (p.charAt(0) === "." ? __cls(n, p.slice(1)) : n.tagName && n.tagName.toLowerCase() === p.toLowerCase()) return n;
        }
        n = n.parentNode || null;
      }
      return null;
    },
    contains: function () { return false; },
    classList: null
  };
  e.style = {
    _v: {},
    setProperty: function (k, v) { this._v[k] = String(v); }
  };
  e.classList = {
    toggle: function (c, on) {
      var want = on === undefined ? e.className.split(" ").indexOf(c) < 0 : !!on;
      var parts = e.className.split(" ").filter(function (x) { return x && x !== c; });
      if (want) parts.push(c);
      e.className = parts.join(" ");
      return want;
    },
    add: function (c) { if (!e.classList.contains(c)) e.className = (e.className ? e.className + " " : "") + c; },
    remove: function (c) { e.className = e.className.split(" ").filter(function (x) { return x && x !== c; }).join(" "); },
    contains: function (c) { return e.className.split(" ").indexOf(c) >= 0; }
  };
  return e;
}
// __clickSummary is a click on a details element's summary: the browser flips
// open and then fires toggle, which the page hears through ontoggle.
function __clickSummary(d) {
  d.open = !d.open;
  if (typeof d.ontoggle === "function") d.ontoggle({ type: "toggle" });
}
function __textNode(t) { return { nodeType: 3, textContent: t == null ? "" : String(t) }; }
// __click is the browser's click on an element: the element takes the keyboard
// the way a button does, and its handler runs.
function __click(e) {
  if (typeof e.focus === "function") e.focus();
  if (typeof e.onclick === "function") e.onclick({ type: "click", target: e });
}
var document = {
  createElement: function (t) { return __el(t); },
  createElementNS: function (ns, t) { return __el(t); },
  createTextNode: function (t) { return __textNode(t); },
  getElementById: function (id) { return __byId[id] || null; },
  querySelector: function () { return null; },
  // The page asks the document for its sections once, when it folds them
  // ("main section, .grid section"), and for nodes carrying a data attribute as
  // the feed's ages move; the stub answers the first from the markup and the
  // second with nothing, which is the truth for a page no render has drawn.
  querySelectorAll: function (sel) {
    return String(sel).indexOf("section") >= 0 ? __sections.slice() : [];
  },
  addEventListener: function () {},
  title: "", hidden: false, activeElement: null,
  body: __el("body"),
  hasFocus: function () { return true; },
  execCommand: function () { return true; }
};
// __storageThrows is a browser that refuses storage outright — private mode, a
// locked-down policy. A test sets it before the page runs, so the page meets the
// refusal where it reads its first choice.
var __storageThrows = false;
var localStorage = {
  _v: {},
  getItem: function (k) {
    if (__storageThrows) throw new Error("storage is disabled");
    return Object.prototype.hasOwnProperty.call(this._v, k) ? this._v[k] : null;
  },
  setItem: function (k, v) {
    if (__storageThrows) throw new Error("storage is disabled");
    this._v[k] = String(v);
  },
  removeItem: function (k) {
    if (__storageThrows) throw new Error("storage is disabled");
    delete this._v[k];
  }
};
// __fold, __foldKeys, __foldRead and __pressFold are how a test reads and drives
// a section's fold: the section by its key, the header's caret, aria-expanded
// and summary line, and the click or the keydown the browser would deliver.
function __fold(key) {
  for (var i = 0; i < __sections.length; i++) if (__sections[i].dataset.fold === key) return __sections[i];
  return null;
}
function __foldKeys() { return __sections.map(function (s) { return s.dataset.fold; }); }
function __foldRead(key) {
  var sec = __fold(key);
  if (!sec) return null;
  var h = __find(sec, ":scope > h2");
  var caret = h ? __find(h, ".caret") : null;
  var sum = h ? __find(h, ".foldsum") : null;
  var note = h ? __find(h, ".r") : null;
  return {
    collapsed: __cls(sec, "collapsed"),
    aria: h ? String(h.getAttribute("aria-expanded")) : "",
    role: h ? String(h.getAttribute("role") || "") : "",
    caret: caret ? caret.textContent : "",
    summary: sum ? sum.textContent : "",
    summaryHidden: sum ? !!sum.hidden : true,
    noteHidden: note ? !!note.hidden : null,
    title: h ? h.textContent : ""
  };
}
function __pressFold(key, how) {
  var sec = __fold(key), h = sec && __find(sec, ":scope > h2");
  if (!h) throw new Error("no section folds under " + key);
  if (how === "click") { __fireOn(h, "click", { type: "click", target: h }); return; }
  __fireOn(h, "keydown", {
    type: "keydown", target: h,
    key: how === "space" ? " " : "Enter",
    preventDefault: function () {}
  });
}
// __sources is every stream the page has opened, in order: the page replaces a
// stream by closing it and opening a new EventSource, so the list is how many
// streams the page has run through.
var __sources = [];
var __reloads = 0;
var EventSource = function (url) {
  this.url = url;
  this.readyState = EventSource.CONNECTING;
  this.onopen = null;
  this.onerror = null;
  this._listeners = {};
  this.addEventListener = function (type, fn) { (this._listeners[type] = this._listeners[type] || []).push(fn); };
  this.close = function () { this.readyState = EventSource.CLOSED; };
  __sources.push(this);
};
EventSource.CONNECTING = 0;
EventSource.OPEN = 1;
EventSource.CLOSED = 2;
// __open, __error and __fire are the browser and the server talking to one
// stream: the page hears only the events behind them, and a test drives each one
// by hand.
function __open(s) { s.readyState = EventSource.OPEN; if (typeof s.onopen === "function") s.onopen({ type: "open" }); }
function __error(s, state) {
  s.readyState = state;
  if (typeof s.onerror === "function") s.onerror({ type: "error" });
}
function __fire(s, type, data) {
  var ls = s._listeners[type] || [];
  for (var i = 0; i < ls.length; i++) ls[i]({ type: type, data: JSON.stringify(data) });
}
var location = { href: "http://localhost/", reload: function () { __reloads++; } };
// __fetches is every request the page has sent with fetch, in order. The page
// asks the server only when the reader asks for something (a report, a bead),
// so a fold that re-requested a section would show here. The request never
// answers: a test reads what was asked, not what came back.
var __fetches = [];
function fetch(url) { __fetches.push(String(url)); return new Promise(function () {}); }
var navigator = {};
var history = { pushState: function () {}, replaceState: function () {} };
function alert() {}
function addEventListener() {}
function removeEventListener() {}
// The fake clock, and the timers the page schedules against it. Nothing fires
// until __advance moves past a timer's due time, in due order, so a test runs
// the page's redraws and reconnects when it asks for them rather than when they
// fall. An interval reschedules itself by its own period, never at zero, so
// __advance always makes progress; the step count is a runaway guard, not a
// limit a test should reach.
var __epoch = Date.now(), __clock = 0, __timerSeq = 0, __timers = [];
Date.now = function () { return __epoch + __clock; };
function __schedule(fn, ms, every, args) {
  __timerSeq++;
  __timers.push({ id: __timerSeq, fn: fn, at: __clock + (ms > 0 ? ms : 0), every: every ? (ms > 0 ? ms : 10) : 0, args: args });
  return __timerSeq;
}
function setTimeout(fn) { return __schedule(fn, arguments[1], false, [].slice.call(arguments, 2)); }
function setInterval(fn) { return __schedule(fn, arguments[1], true, [].slice.call(arguments, 2)); }
function clearTimeout(id) { __timers = __timers.filter(function (t) { return t.id !== id; }); }
function clearInterval(id) { clearTimeout(id); }
function requestAnimationFrame(fn) { return __schedule(fn, 16, false, []); }
function __advance(ms) {
  var end = __clock + ms;
  for (var step = 0; step < 100000; step++) {
    var next = null;
    for (var i = 0; i < __timers.length; i++) {
      var t = __timers[i];
      if (t.at <= end && (!next || t.at < next.at || (t.at === next.at && t.id < next.id))) next = t;
    }
    if (!next) break;
    __clock = next.at;
    if (next.every) next.at = __clock + next.every; else clearTimeout(next.id);
    next.fn.apply(null, next.args);
  }
  __clock = end;
}
var window = this;
function __snapEl(e) {
  return { text: e.textContent, class: e.className, title: e.title, hidden: !!e.hidden, href: e.href || "", target: e.target || "", rel: e.rel || "", kids: e.children.length };
}
function __snap(id) {
  var e = __byId[id];
  if (!e) return null;
  var s = __snapEl(e);
  s.id = id;
  return s;
}
// __descend walks one element's subtree by child index, so a test reads back a
// row a pane drew rather than only the container it went into. A path that does
// not exist is null, which the test fails on.
function __descend(id) {
  var e = __byId[id];
  for (var i = 1; i < arguments.length; i++) {
    if (!e || !e.children[arguments[i]]) return null;
    e = e.children[arguments[i]];
  }
  return __snapEl(e);
}
`

// pageIDRE matches one id attribute. The stub answers getElementById from this
// set, so a page that draws into an id the markup does not carry fails.
var pageIDRE = regexp.MustCompile(`\bid="([^"]+)"`)

// pageScript is index.html's inline script, the text the browser runs.
func pageScript(t *testing.T) string {
	t.Helper()
	page := string(indexHTML)
	start := strings.Index(page, "<script>")
	end := strings.LastIndex(page, "</script>")
	if start < 0 || end < start {
		t.Fatal("index.html has no inline script")
	}
	return page[start+len("<script>") : end]
}

// foldSectionSpec is one section of index.html's grid as the stub builds it:
// the key its fold is saved under, its id, the attributes the markup carries,
// and the heading's text and spans in order. It is read from the markup, so a
// section added to the page without a key shows up in the fold tests as one
// that cannot fold rather than being left out.
type foldSectionSpec struct {
	Key              string         `json:"key"`
	ID               string         `json:"id,omitempty"`
	DefaultCollapsed bool           `json:"defaultCollapsed,omitempty"`
	Hidden           bool           `json:"hidden,omitempty"`
	Head             []foldHeadPart `json:"head"`
}

// foldHeadPart is one run of a section heading: the text between its spans, or
// a span with the class and id the markup gives it.
type foldHeadPart struct {
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`
	Class string `json:"class,omitempty"`
}

var (
	pageAttrRE    = regexp.MustCompile(`([A-Za-z_:][-A-Za-z0-9_:.]*)="([^"]*)"`)
	pageSpanRE    = regexp.MustCompile(`<span([^>]*)>([^<]*)</span>`)
	pageBareHidRE = regexp.MustCompile(`(^|\s)hidden(\s|>|$)`)
)

// pageSections is the page's grid as the stub builds it: every section in
// index.html's .grid, with the heading it folds under.
func pageSections(t *testing.T) []foldSectionSpec {
	t.Helper()

	page := string(indexHTML)
	start := strings.Index(page, `<div class="grid">`)
	if start < 0 {
		t.Fatal("index.html has no .grid holding the sections")
	}
	grid := page[start:]
	if end := strings.Index(grid, "<script>"); end >= 0 {
		grid = grid[:end]
	}
	var out []foldSectionSpec
	for i := 0; ; {
		at := strings.Index(grid[i:], "<section")
		if at < 0 {
			break
		}
		at += i
		open := strings.Index(grid[at:], ">")
		if open < 0 {
			t.Fatalf("index.html's section at offset %d has no end of tag", at)
		}
		tag := grid[at : at+open+1]
		body := grid[at+open+1:]
		next := strings.Index(body, "<section")
		if next < 0 {
			next = len(body)
		}
		var sp foldSectionSpec
		for _, m := range pageAttrRE.FindAllStringSubmatch(tag, -1) {
			switch m[1] {
			case "id":
				sp.ID = m[2]
			case "data-fold":
				sp.Key = m[2]
			}
		}
		sp.DefaultCollapsed = strings.Contains(tag, "data-default-collapsed")
		sp.Hidden = pageBareHidRE.MatchString(tag)
		sp.Head = pageHeadParts(t, body[:next])
		out = append(out, sp)
		i = at + open + 1 + next
	}
	return out
}

// pageHeadParts is one section's heading as the stub lays it out: the text
// between its spans and each span with its class and id, which is where the
// page finds the note it copies into a closed section's summary.
func pageHeadParts(t *testing.T, body string) []foldHeadPart {
	t.Helper()

	open := strings.Index(body, "<h2>")
	if open < 0 {
		return nil
	}
	inner := body[open+len("<h2>"):]
	end := strings.Index(inner, "</h2>")
	if end < 0 {
		t.Fatalf("index.html's section heading has no end: %q", body)
	}
	inner = inner[:end]
	var out []foldHeadPart
	for inner != "" {
		loc := pageSpanRE.FindStringSubmatchIndex(inner)
		if loc == nil {
			out = append(out, foldHeadPart{Text: inner})
			break
		}
		if text := inner[:loc[0]]; text != "" {
			out = append(out, foldHeadPart{Text: text})
		}
		p := foldHeadPart{Text: inner[loc[4]:loc[5]]}
		for _, m := range pageAttrRE.FindAllStringSubmatch(inner[loc[2]:loc[3]], -1) {
			switch m[1] {
			case "id":
				p.ID = m[2]
			case "class":
				p.Class = m[2]
			}
		}
		out = append(out, p)
		inner = inner[loc[1]:]
	}
	return out
}

// pageIDs is every element id index.html's markup declares, in the order the
// file declares them.
func pageIDs() []string {
	var ids []string
	for _, m := range pageIDRE.FindAllStringSubmatch(string(indexHTML), -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// pageDOM is one run of the page: its script executed in a fresh engine over
// the stub DOM, so tests hold independent pages.
type pageDOM struct {
	vm *goja.Runtime
}

// newPageDOM loads index.html's script over an empty browser: no storage, so
// the page reads none of the choices a previous load left behind.
func newPageDOM(t *testing.T) *pageDOM {
	t.Helper()

	return newPageDOMStoring(t, nil)
}

// newPageDOMStoring loads index.html's script over a browser that already holds
// storage — the page loaded a second time, which is the reload a choice kept in
// localStorage has to survive. Every value is written before the page's script
// reads it, so what the page gets back is what the browser would hold. A page
// that cannot run is a page the harness does not describe, so a stub missing
// what a new function uses stops the test rather than silently checking less of
// the page.
func newPageDOMStoring(t *testing.T, stored map[string]string) *pageDOM {
	t.Helper()

	return newPage(t, stored, "")
}

// newPageDOMNoStorage loads the page over a browser that refuses storage — what
// private mode and a locked-down policy do — before the page reads its first
// choice, so a page that cannot survive the refusal fails here.
func newPageDOMNoStorage(t *testing.T) *pageDOM {
	t.Helper()

	return newPage(t, nil, "__storageThrows = true;")
}

// newPage runs one page: the stub browser with the markup's ids and sections,
// whatever storage the caller says the browser holds, anything the caller needs
// true before the page starts (setUp), then the page's own script.
func newPage(t *testing.T, stored map[string]string, setUp string) *pageDOM {
	t.Helper()

	vm := goja.New()
	if err := vm.Set("__ids", pageIDs()); err != nil {
		t.Fatalf("handing the page's ids to the stub: %v", err)
	}
	specs, err := json.Marshal(pageSections(t))
	if err != nil {
		t.Fatalf("reading the page's sections: %v", err)
	}
	if _, err := vm.RunString("var __sectionSpec = " + string(specs) + ";"); err != nil {
		t.Fatalf("handing the page's sections to the stub: %v", err)
	}
	if _, err := vm.RunString(pageStubJS); err != nil {
		t.Fatalf("the DOM stub did not load: %v", err)
	}
	for k, v := range stored {
		if _, err := vm.RunString("localStorage.setItem(" + strconv.Quote(k) + ", " + strconv.Quote(v) + ")"); err != nil {
			t.Fatalf("handing the page its %s: %v", k, err)
		}
	}
	if setUp != "" {
		if _, err := vm.RunString(setUp); err != nil {
			t.Fatalf("setting the page's browser up: %v", err)
		}
	}
	if _, err := vm.RunString(pageScript(t)); err != nil {
		t.Fatalf("index.html's script did not run: %v", err)
	}
	return &pageDOM{vm: vm}
}

// storage is everything the page has written, the way the browser would hand it
// to the next load.
func (p *pageDOM) storage(t *testing.T) map[string]string {
	t.Helper()

	v, err := p.vm.RunString("JSON.stringify(localStorage._v)")
	if err != nil {
		t.Fatalf("reading the page's storage: %v", err)
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(v.String()), &out); err != nil {
		t.Fatalf("reading the page's storage: %v", err)
	}
	return out
}

// call runs one of the page's own functions over a single reading, the state
// or row the server would send it. A name the page no longer declares fails
// here: a test naming a function that is gone checks nothing.
func (p *pageDOM) call(t *testing.T, fn string, arg any) {
	t.Helper()

	f, ok := goja.AssertFunction(p.vm.Get(fn))
	if !ok {
		t.Fatalf("index.html has no function %s to run", fn)
	}
	if _, err := f(goja.Undefined(), p.vm.ToValue(arg)); err != nil {
		t.Fatalf("%s(%v): %v", fn, arg, err)
	}
}

// drawn is what one element holds after a render: the text it reads, the class
// it wears, its title, whether it is hidden, where a link goes, and how many
// children it holds.
type drawn struct {
	Text   string `json:"text"`
	Class  string `json:"class"`
	Title  string `json:"title"`
	Hidden bool   `json:"hidden"`
	Href   string `json:"href"`
	Target string `json:"target"`
	Rel    string `json:"rel"`
	Kids   int    `json:"kids"`
}

// element reads back one element of the page by id. An id the markup does not
// carry is a fatal, not an empty reading: the page would draw into nothing.
func (p *pageDOM) element(t *testing.T, id string) drawn {
	t.Helper()

	v, err := p.vm.RunString("JSON.stringify(__snap(" + strconv.Quote(id) + "))")
	if err != nil {
		t.Fatalf("reading #%s: %v", id, err)
	}
	js := v.String()
	if js == "null" {
		t.Fatalf("index.html's markup has no #%s", id)
	}
	var d drawn
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		t.Fatalf("reading #%s: %v", id, err)
	}
	return d
}

// cellWithText reads back the first leaf under the element id whose text is
// exactly text: the cell a render drew, with the title it carries. Matching the
// whole text keeps a cell from answering for a longer one that merely starts
// with it, where "docker-sbx" would also read the k8s seat's cell. A render
// that drew no such cell fails here rather than reading as an empty string.
func (p *pageDOM) cellWithText(t *testing.T, id, text string) drawn {
	t.Helper()

	v, err := p.vm.RunString(`JSON.stringify((() => {
		let out = null;
		(function walk(n) {
			for (const c of (n.children || [])) {
				if (!out && c.children && c.children.length === 0 && String(c.textContent) === ` + quoteJS(text) + `) {
					out = {text: c.textContent, class: c.className, title: c.title};
				}
				walk(c);
			}
		})(__byId[` + quoteJS(id) + `]);
		return out;
	})())`)
	if err != nil {
		t.Fatalf("reading #%s for %q: %v", id, text, err)
	}
	js := v.String()
	if js == "null" {
		t.Fatalf("#%s draws no cell holding %q", id, text)
	}
	var d drawn
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		t.Fatalf("reading #%s for %q: %v", id, text, err)
	}
	return d
}

// descend reads back one element of the page by walking child indices from an
// id — the row a pane drew under its container, where element reads only the
// container. A path the pane did not draw fails here rather than returning a
// zero reading.
func (p *pageDOM) descend(t *testing.T, id string, path ...int) drawn {
	t.Helper()

	args := []string{strconv.Quote(id)}
	for _, i := range path {
		args = append(args, strconv.Itoa(i))
	}
	v, err := p.vm.RunString("JSON.stringify(__descend(" + strings.Join(args, ",") + "))")
	if err != nil {
		t.Fatalf("reading #%s%v: %v", id, path, err)
	}
	js := v.String()
	if js == "null" {
		t.Fatalf("#%s has no element at %v", id, path)
	}
	var d drawn
	if err := json.Unmarshal([]byte(js), &d); err != nil {
		t.Fatalf("reading #%s%v: %v", id, path, err)
	}
	return d
}

// A scroll position the stub keeps is one a browser could hold: a write past the
// end of the rows reads back at the end of the range, a write before the top
// reads back at 0, and an element with nothing to scroll stays at 0. A test that
// scrolls a rebuilt table would otherwise pass on a position no reader can reach
// (gt-tjlu6).
func TestPageHarnessKeepsAScrollWithinTheRangeItsRowsAllow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		rows  int
		write int
		want  int
	}{
		{"past the end of thirty rows", 30, 9999, 30*24 - 200},
		{"inside the range", 30, 240, 240},
		{"before the top", 30, -5, 0},
		{"nothing to scroll", 3, 240, 0},
	}
	p := newPageDOM(t)
	for _, c := range cases {
		v, err := p.vm.RunString(`(function () {
  var box = document.createElement("div");
  for (var i = 0; i < ` + strconv.Itoa(c.rows) + `; i++) box.append(document.createElement("tr"));
  box.scrollTop = ` + strconv.Itoa(c.write) + `;
  return box.scrollTop;
})()`)
		if err != nil {
			t.Fatalf("%s: scrolling a stub box: %v", c.name, err)
		}
		if got := int(v.ToInteger()); got != c.want {
			t.Errorf("%s: scrollTop after writing %d = %d, want %d", c.name, c.write, got, c.want)
		}
	}
}
