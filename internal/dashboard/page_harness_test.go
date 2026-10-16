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
// sleeps, reads a clock or touches the network.

// pageStubJS is the browser the page runs in: the DOM surface index.html's
// script uses, and nothing more. A render function only builds elements, so
// createElement, append, textContent, className, classList, hidden and title
// carry it; anything a later test needs beyond that is added here with the
// test that needs it.
//
// Two deliberate refusals keep the stub honest about the page. getElementById
// answers only for the ids index.html's markup carries (__ids, read from the
// file), so a render into an id the markup lost fails the test instead of
// quietly passing against a stub that invents elements. And no timer ever
// fires: a test drives each redraw itself, so nothing renders behind it.
const pageStubJS = `
var __byId = {};
__ids.forEach(function (id) { __byId[id] = __el("div"); });
function __el(tag) {
  var e = {
    tagName: String(tag).toUpperCase(), children: [], _text: "",
    className: "", title: "", hidden: false, tabIndex: 0,
    dataset: {}, style: {},
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
function __textNode(t) { return { nodeType: 3, textContent: t == null ? "" : String(t) }; }
var document = {
  createElement: function (t) { return __el(t); },
  createElementNS: function (ns, t) { return __el(t); },
  createTextNode: function (t) { return __textNode(t); },
  getElementById: function (id) { return __byId[id] || null; },
  querySelector: function () { return null; },
  querySelectorAll: function () { return []; },
  addEventListener: function () {},
  title: "", hidden: false,
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
var EventSource = function (url) {
  this.url = url;
  this.onopen = null;
  this.onerror = null;
  this.addEventListener = function () {};
  this.close = function () {};
};
var location = { href: "http://localhost/", reload: function () {} };
var navigator = {};
var history = { pushState: function () {}, replaceState: function () {} };
function alert() {}
function addEventListener() {}
function removeEventListener() {}
function setInterval() { return 1; }
function setTimeout() { return 1; }
function clearInterval() {}
function clearTimeout() {}
function requestAnimationFrame() { return 1; }
var window = this;
function __snap(id) {
  var e = __byId[id];
  if (!e) return null;
  return { id: id, text: e.textContent, class: e.className, title: e.title, hidden: !!e.hidden, kids: e.children.length };
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

// newPageDOM loads index.html's script, failing the test if the page throws.
// A page that cannot run is a page the harness does not describe, so a stub
// missing what a new function uses stops the test rather than silently
// checking less of the page.
func newPageDOM(t *testing.T) *pageDOM {
	t.Helper()

	vm := goja.New()
	if err := vm.Set("__ids", pageIDs()); err != nil {
		t.Fatalf("handing the page's ids to the stub: %v", err)
	}
	if _, err := vm.RunString(pageStubJS); err != nil {
		t.Fatalf("the DOM stub did not load: %v", err)
	}
	if _, err := vm.RunString(pageScript(t)); err != nil {
		t.Fatalf("index.html's script did not run: %v", err)
	}
	return &pageDOM{vm: vm}
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
// it wears, its title, whether it is hidden, and how many children it holds.
type drawn struct {
	Text   string `json:"text"`
	Class  string `json:"class"`
	Title  string `json:"title"`
	Hidden bool   `json:"hidden"`
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
