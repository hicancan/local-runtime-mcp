package jsrt

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"
)

// installWebGlobals installs lightweight, zero-dependency standard JS/Web globals
// (atob, btoa, setTimeout, clearTimeout, URL, URLSearchParams) onto the session VM.
func (v *sessionVM) installWebGlobals() {
	// atob / btoa
	_ = v.vm.Set("atob", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(v.vm.NewTypeError("atob requires a base64 string"))
		}
		raw := strings.TrimSpace(call.Arguments[0].String())
		b, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(raw)
			if err != nil {
				panic(v.vm.NewTypeError("atob: invalid base64 input: " + err.Error()))
			}
		}
		return v.vm.ToValue(string(b))
	})
	_ = v.vm.Set("btoa", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(v.vm.NewTypeError("btoa requires a string"))
		}
		return v.vm.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Arguments[0].String())))
	})

	// setTimeout / clearTimeout bound to VM event loop & active cell lifetime.
	var timerSeq atomic.Int64
	var timerMu sync.Mutex
	timers := map[int64]*time.Timer{}

	_ = v.vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(v.vm.NewTypeError("setTimeout requires a callback function"))
		}
		fn, ok := goja.AssertFunction(call.Arguments[0])
		if !ok {
			panic(v.vm.NewTypeError("setTimeout callback must be a function"))
		}
		var ms int64
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			ms = call.Arguments[1].ToInteger()
		}
		if ms < 0 {
			ms = 0
		}
		if ms > 300_000 {
			ms = 300_000
		}
		var extraArgs []goja.Value
		if len(call.Arguments) > 2 {
			extraArgs = append([]goja.Value(nil), call.Arguments[2:]...)
		}
		id := timerSeq.Add(1)
		cell := v.currentCell()
		t := time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
			timerMu.Lock()
			delete(timers, id)
			timerMu.Unlock()
			if cell != nil && cell.ctx.Err() != nil {
				return
			}
			v.post(func() {
				prev := v.activeCell
				v.activeCell = cell
				defer func() { v.activeCell = prev }()
				_, _ = fn(goja.Undefined(), extraArgs...)
			})
		})
		timerMu.Lock()
		timers[id] = t
		timerMu.Unlock()
		return v.vm.ToValue(id)
	})
	_ = v.vm.Set("clearTimeout", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 || goja.IsUndefined(call.Arguments[0]) || goja.IsNull(call.Arguments[0]) {
			return goja.Undefined()
		}
		id := call.Arguments[0].ToInteger()
		timerMu.Lock()
		if t := timers[id]; t != nil {
			t.Stop()
			delete(timers, id)
		}
		timerMu.Unlock()
		return goja.Undefined()
	})

	// Go-backed URL parser helper + JS URL / URLSearchParams constructors.
	_ = v.vm.Set("__parseURL", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(v.vm.NewTypeError("URL requires a string argument"))
		}
		raw := call.Arguments[0].String()
		var parsed *url.URL
		var err error
		if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) && !goja.IsNull(call.Arguments[1]) {
			baseURL, berr := url.Parse(call.Arguments[1].String())
			if berr != nil || baseURL.Scheme == "" {
				panic(v.vm.NewTypeError("Invalid base URL: " + call.Arguments[1].String()))
			}
			rel, rerr := url.Parse(raw)
			if rerr != nil {
				panic(v.vm.NewTypeError("Invalid URL: " + raw))
			}
			parsed = baseURL.ResolveReference(rel)
		} else {
			parsed, err = url.Parse(raw)
			if err != nil || parsed.Scheme == "" {
				panic(v.vm.NewTypeError("Invalid URL: " + raw))
			}
		}
		proto := parsed.Scheme + ":"
		origin := ""
		if parsed.Host != "" {
			origin = proto + "//" + parsed.Host
		}
		pathname := parsed.Path
		if pathname == "" && parsed.Host != "" {
			pathname = "/"
		}
		search := ""
		if parsed.RawQuery != "" {
			search = "?" + parsed.RawQuery
		}
		hash := ""
		if parsed.Fragment != "" {
			hash = "#" + parsed.Fragment
		}
		return v.vm.ToValue(map[string]any{
			"href":     fmt.Sprintf("%s//%s%s%s%s", proto, parsed.Host, pathname, search, hash),
			"protocol": proto,
			"origin":   origin,
			"host":     parsed.Host,
			"hostname": parsed.Hostname(),
			"port":     parsed.Port(),
			"pathname": pathname,
			"search":   search,
			"hash":     hash,
		})
	})

	_, _ = v.vm.RunString(`
(() => {
const _parseURL = globalThis.__parseURL;
delete globalThis.__parseURL;
class URLSearchParams {
  constructor(init) {
    this._entries = [];
    if (!init) return;
    if (typeof init === 'string') {
      const s = init.startsWith('?') ? init.slice(1) : init;
      if (!s) return;
      for (const part of s.split('&')) {
        if (!part) continue;
        const eq = part.indexOf('=');
        const k = eq >= 0 ? part.slice(0, eq) : part;
        const val = eq >= 0 ? part.slice(eq + 1) : '';
        this._entries.push([decodeURIComponent(k.replace(/\+/g, ' ')), decodeURIComponent(val.replace(/\+/g, ' '))]);
      }
    } else if (Array.isArray(init)) {
      for (const pair of init) {
        if (Array.isArray(pair) && pair.length >= 2) this._entries.push([String(pair[0]), String(pair[1])]);
      }
    } else if (typeof init === 'object') {
      for (const [k, val] of Object.entries(init)) this._entries.push([String(k), String(val)]);
    }
  }
  append(k, val) { this._entries.push([String(k), String(val)]); }
  delete(k) { const key = String(k); this._entries = this._entries.filter(([ek]) => ek !== key); }
  get(k) { const key = String(k); const f = this._entries.find(([ek]) => ek === key); return f ? f[1] : null; }
  getAll(k) { const key = String(k); return this._entries.filter(([ek]) => ek === key).map(([, v]) => v); }
  has(k) { const key = String(k); return this._entries.some(([ek]) => ek === key); }
  set(k, val) {
    const key = String(k), s = String(val);
    let replaced = false;
    const next = [];
    for (const entry of this._entries) {
      if (entry[0] === key) {
        if (!replaced) { next.push([key, s]); replaced = true; }
      } else {
        next.push(entry);
      }
    }
    if (!replaced) next.push([key, s]);
    this._entries = next;
  }
  entries() { return this._entries[Symbol.iterator](); }
  keys() { return this._entries.map(([k]) => k)[Symbol.iterator](); }
  values() { return this._entries.map(([, v]) => v)[Symbol.iterator](); }
  forEach(cb, thisArg) { for (const [k, val] of this._entries) cb.call(thisArg, val, k, this); }
  [Symbol.iterator]() { return this.entries(); }
  toString() {
    return this._entries.map(([k, val]) => encodeURIComponent(k) + '=' + encodeURIComponent(val)).join('&');
  }
}
class URL {
  constructor(input, base) {
    const p = _parseURL(String(input), base === undefined ? undefined : String(base));
    this.protocol = p.protocol;
    this.origin = p.origin;
    this.host = p.host;
    this.hostname = p.hostname;
    this.port = p.port;
    this.pathname = p.pathname;
    this.hash = p.hash;
    this.searchParams = new URLSearchParams(p.search);
  }
  get search() {
    const s = this.searchParams.toString();
    return s ? '?' + s : '';
  }
  set search(val) {
    this.searchParams = new URLSearchParams(String(val || ''));
  }
  get href() {
    return this.protocol + '//' + this.host + this.pathname + this.search + this.hash;
  }
  toString() { return this.href; }
  toJSON() { return this.href; }
}
globalThis.URLSearchParams = URLSearchParams;
globalThis.URL = URL;
})();
`)
}
