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
function __el(tag) {
  var e = {
    tagName: String(tag).toUpperCase(), children: [], _text: "",
    className: "", title: "", hidden: false, tabIndex: 0,
    dataset: {},
    // The Deploys block sets its ch-based column widths through style, so the
    // stub keeps what the page writes there.
    style: null,
    scrollTop: 0, scrollHeight: 0, clientHeight: 0,
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
    append: function () { for (var i = 0; i < arguments.length; i++) this.children.push(arguments[i]); },
    appendChild: function (n) { this.children.push(n); return n; },
    prepend: function () { for (var i = arguments.length - 1; i >= 0; i--) this.children.unshift(arguments[i]); },
    replaceChildren: function () {
      this.children = [];
      this._text = "";
      for (var i = 0; i < arguments.length; i++) this.children.push(arguments[i]);
    },
    remove: function () {},
    focus: function () { document.activeElement = this; },
    setAttribute: function (k, v) { this["attr_" + k] = v; },
    getAttribute: function (k) { return this["attr_" + k]; },
    addEventListener: function () {},
    removeEventListener: function () {},
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    closest: function () { return null; },
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
  querySelectorAll: function () { return []; },
  addEventListener: function () {},
  title: "", hidden: false, activeElement: null,
  body: __el("body"),
  hasFocus: function () { return true; },
  execCommand: function () { return true; }
};
var localStorage = {
  _v: {},
  getItem: function (k) { return Object.prototype.hasOwnProperty.call(this._v, k) ? this._v[k] : null; },
  setItem: function (k, v) { this._v[k] = String(v); },
  removeItem: function (k) { delete this._v[k]; }
};
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

	vm := goja.New()
	if err := vm.Set("__ids", pageIDs()); err != nil {
		t.Fatalf("handing the page's ids to the stub: %v", err)
	}
	if _, err := vm.RunString(pageStubJS); err != nil {
		t.Fatalf("the DOM stub did not load: %v", err)
	}
	for k, v := range stored {
		if _, err := vm.RunString("localStorage.setItem(" + strconv.Quote(k) + ", " + strconv.Quote(v) + ")"); err != nil {
			t.Fatalf("handing the page its %s: %v", k, err)
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
