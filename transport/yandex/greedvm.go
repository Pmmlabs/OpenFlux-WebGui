package yandex

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"

	"openflux/utils"
)

// Выполнение greed.js в JS-раннере (goja) с заглушками браузера.
//
// Отпечаток greed.js шифруется сессионным ключом Яндекса: сервер
// расшифровывает rdata и валидирует значения. Статический шаблон
// зашифрован ключом чужой сессии — сервер видит мусор и эскалирует
// капчу до силуэтной. Единственный надёжный способ получить валидный
// rdata — выполнить настоящий greed.js, чтобы он сам собрал отпечаток
// и зашифровал его правильным ключом.

// greedStubJS — окружение браузера для greed.js. Значения сняты с
// headless Chrome 141 (macOS), того же профиля, что и весь остальной
// fingerprint-контекст клиента (UA, TLS, metrika).
const greedStubJS = `
var navigator = {
  userAgent: '` + browserUserAgent + `',
  platform: 'MacIntel',
  language: 'ru-RU',
  languages: ['ru-RU', 'ru', 'en-US', 'en'],
  hardwareConcurrency: 12,
  deviceMemory: 8,
  maxTouchPoints: 0,
  vendor: 'Google Inc.',
  productSub: '20030107',
  cookieEnabled: true,
  doNotTrack: null,
  plugins: { length: 5, 0: { name: 'PDF Viewer', filename: 'internal-pdf-viewer', description: '' }, 1: { name: 'Chrome PDF Viewer', filename: 'internal-pdf-viewer', description: '' }, 2: { name: 'Chromium PDF Viewer', filename: 'internal-pdf-viewer', description: '' }, 3: { name: 'Microsoft Edge PDF Viewer', filename: 'internal-pdf-viewer', description: '' }, 4: { name: 'WebKit built-in PDF', filename: 'internal-pdf-viewer', description: '' }, item: function(i) { return this[i]; }, namedItem: function(n) { return null; }, refresh: function() {} },
  mimeTypes: { length: 2, 0: { type: 'application/pdf', suffixes: 'pdf', description: '', enabledPlugin: null }, 1: { type: 'text/pdf', suffixes: 'pdf', description: '', enabledPlugin: null }, item: function(i) { return this[i]; }, namedItem: function(n) { return null; } },
  javaEnabled: function() { return false; },
};
var screen = {
  width: 800, height: 600, availWidth: 800, availHeight: 600,
  colorDepth: 24, pixelDepth: 24,
  orientation: { type: 'landscape-primary', angle: 0 },
};
function make2D() {
  return new Proxy({}, {
    get: function(t, p) {
      if (p === 'measureText') return function() { return { width: 10 + Math.random() * 5 }; };
      if (p === 'isPointInPath') return function() { return false; };
      if (p === 'getImageData') return function() { return { data: new Uint8ClampedArray(4) }; };
      if (p === 'createLinearGradient' || p === 'createRadialGradient') return function() { return { addColorStop: function() {} }; };
      if (p === 'createPattern') return function() { return {}; };
      if (typeof p === 'string') return function() {};
      return undefined;
    },
    set: function() { return true; },
  });
}
var GL_PARAMS = {
  37445: 'Apple',
  37446: 'Apple GPU',
  7936: 'WebKit',
  7937: 'WebKit WebGL',
  7938: 'WebGL 1.0',
};
var canvasProto = {
  width: 300, height: 150,
  getContext: function(kind) {
    if (kind === '2d') return make2D();
    return {
      getParameter: function(p) { return GL_PARAMS[p] !== undefined ? GL_PARAMS[p] : 'WebGL GLSL ES 1.0 (1.0)'; },
      getExtension: function(n) { return n === 'WEBGL_debug_renderer_info' ? { UNMASKED_VENDOR_WEBGL: 37445, UNMASKED_RENDERER_WEBGL: 37446 } : null; },
      getSupportedExtensions: function() { return ['EXT_texture_filter_anisotropic', 'OES_texture_float', 'OES_element_index_uint']; },
      getContextAttributes: function() { return { alpha: true, antialias: true, depth: true, stencil: false }; },
    };
  },
  toDataURL: function() { return 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUg' + btoa(String(Math.random())).slice(0, 40) + '=='; },
};
var document = {
  cookie: '',
  documentElement: { style: {}, clientWidth: 1280, clientHeight: 713, getBoundingClientRect: function() { return { top: 0, left: 0, right: 1280, bottom: 713 }; } },
  body: { appendChild: function() {}, removeChild: function() {}, style: {} },
  createElement: function(tag) {
    if (tag === 'canvas') {
      var c = {};
      for (var k in canvasProto) c[k] = canvasProto[k];
      return c;
    }
    return { style: {}, appendChild: function() {}, setAttribute: function() {}, getElementsByTagName: function() { return []; }, getContext: canvasProto.getContext, toDataURL: canvasProto.toDataURL };
  },
  getElementsByTagName: function() { return [{ appendChild: function() {} }]; },
  addEventListener: function() {}, removeEventListener: function() {},
  visibilityState: 'visible', hidden: false,
};
var window = this;
window.navigator = navigator;
window.screen = screen;
window.document = document;
window.location = { href: 'https://docs.yandex.ru/showcaptcha?cc=1', protocol: 'https:', host: 'docs.yandex.ru', hostname: 'docs.yandex.ru', origin: 'https://docs.yandex.ru' };
window.devicePixelRatio = 1;
window.innerWidth = 1280; window.innerHeight = 713;
window.outerWidth = 1280; window.outerHeight = 800;
window.screenX = 0; window.screenY = 0; window.screenLeft = 0; window.screenTop = 0;
window.history = { length: 7 };
window.performance = { now: function() { return Date.now() % 100000; }, timing: { navigationStart: Date.now() - 3000 } };
window.localStorage = { getItem: function() { return null; }, setItem: function() {}, removeItem: function() {} };
window.sessionStorage = { getItem: function() { return null; }, setItem: function() {}, removeItem: function() {} };
window.indexedDB = {};
window.addEventListener = function() {}; window.removeEventListener = function() {};
window.requestAnimationFrame = function(cb) { setTimeout(cb, 16); };
window.atob = function(s) { return __atob(s); };
window.btoa = function(s) { return __btoa(s); };
window.crypto = {
  getRandomValues: function(a) { for (var i = 0; i < a.length; i++) a[i] = Math.floor(Math.random() * 256); return a; },
  subtle: {},
};
window.Intl = {
  DateTimeFormat: function() { this.resolvedOptions = function() { return { locale: 'en-US', calendar: 'gregory', numberingSystem: 'latn', timeZone: 'Europe/Moscow', year: 'numeric', month: 'numeric', day: 'numeric' }; }; this.format = function(d) { return (d.getMonth() + 1) + '/' + d.getDate() + '/' + d.getFullYear(); }; },
  NumberFormat: function() { this.resolvedOptions = function() { return { locale: 'en-US', numberingSystem: 'latn', style: 'decimal', minimumIntegerDigits: 1, minimumFractionDigits: 0, maximumFractionDigits: 3, useGrouping: 'auto', notation: 'standard', signDisplay: 'auto', roundingIncrement: 1, roundingMode: 'halfExpand', roundingPriority: 'auto', trailingZeroDisplay: 'auto' }; }; this.format = function(n) { return String(n); }; },
  Collator: function() { this.resolvedOptions = function() { return { locale: 'en-US', usage: 'sort', sensitivity: 'variant', ignorePunctuation: false, collation: 'default', numeric: false, caseFirst: 'false' }; }; this.compare = function(a, b) { return a < b ? -1 : a > b ? 1 : 0; }; },
};
window.Intl[Symbol.toStringTag] = 'Intl';
window.Intl.DateTimeFormat.prototype[Symbol.toStringTag] = 'Intl.DateTimeFormat';
window.Intl.NumberFormat.prototype[Symbol.toStringTag] = 'Intl.NumberFormat';
window.Intl.Collator.prototype[Symbol.toStringTag] = 'Intl.Collator';
// Date.prototype в формате V8/ICU (goja выдаёт "(MSK)" и "09/18/2023, 04:20:00").
(function() {
  var TZ_NAME = 'Moscow Standard Time';
  var TZ_OFFSET = -180; // как getTimezoneOffset: запад > 0
  var DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  function pad(n) { return n < 10 ? '0' + n : '' + n; }
  function off() { var m = -TZ_OFFSET, sign = m < 0 ? '-' : '+'; m = Math.abs(m); return 'GMT' + sign + pad(Math.floor(m / 60)) + pad(m % 60); }
  function loc(d) { return new Date(d.getTime() + TZ_OFFSET * 60000); }
  function h12(h) { var x = h % 12; return x === 0 ? 12 : x; }
  Date.prototype.toString = function() { var l = loc(this); return DAYS[l.getUTCDay()] + ' ' + MONTHS[l.getUTCMonth()] + ' ' + l.getUTCDate() + ' ' + l.getUTCFullYear() + ' ' + pad(l.getUTCHours()) + ':' + pad(l.getUTCMinutes()) + ':' + pad(l.getUTCSeconds()) + ' ' + off() + ' (' + TZ_NAME + ')'; };
  Date.prototype.toTimeString = function() { var l = loc(this); return pad(l.getUTCHours()) + ':' + pad(l.getUTCMinutes()) + ':' + pad(l.getUTCSeconds()) + ' ' + off() + ' (' + TZ_NAME + ')'; };
  Date.prototype.toDateString = function() { var l = loc(this); return DAYS[l.getUTCDay()] + ' ' + MONTHS[l.getUTCMonth()] + ' ' + l.getUTCDate() + ' ' + l.getUTCFullYear(); };
  Date.prototype.toLocaleString = function() { var l = loc(this); return (l.getUTCMonth() + 1) + '/' + l.getUTCDate() + '/' + l.getUTCFullYear() + ', ' + h12(l.getUTCHours()) + ':' + pad(l.getUTCMinutes()) + ':' + pad(l.getUTCSeconds()) + ' ' + (l.getUTCHours() < 12 ? 'AM' : 'PM'); };
  Date.prototype.toLocaleDateString = function() { var l = loc(this); return (l.getUTCMonth() + 1) + '/' + l.getUTCDate() + '/' + l.getUTCFullYear(); };
  Date.prototype.toLocaleTimeString = function() { var l = loc(this); return h12(l.getUTCHours()) + ':' + pad(l.getUTCMinutes()) + ':' + pad(l.getUTCSeconds()) + ' ' + (l.getUTCHours() < 12 ? 'AM' : 'PM'); };
})();
window.TextEncoder = function() { this.encode = function(s) { var out = []; for (var i = 0; i < s.length; i++) { var c = s.charCodeAt(i); if (c < 128) out.push(c); else { out.push(c & 63 | 128); out.push(c >> 6 | 192); } } return new Uint8Array(out); }; };
window.TextDecoder = function() { this.decode = function(b) { var out = ''; for (var i = 0; i < b.length; i++) out += String.fromCharCode(b[i]); return out; }; };
window.chrome = undefined;
window.opera = undefined;
window.callPhantom = undefined;
window._phantom = undefined;
window.__nightmare = undefined;
window.Buffer = undefined;
window.global = undefined;
window.process = undefined;
window.require = undefined;
window.module = undefined;
window.exports = undefined;
window.self = window;
window.top = window;
window.globalThis = window;
`

// runGreedJS выполняет greed.js в goja и возвращает результат
// PGreed.safeGet() — JSON-строку отпечатка (уже зашифрован сессионным
// ключом), с исходным порядком полей. pageURL подставляется в
// location.href заглушки.
// greedTimer — отложенный вызов setTimeout/setInterval: колбэк
// исполняется не синхронно (иначе Promise.race в коллекторах greed.js
// разрешается таймаутом раньше, чем проба API), а в цикле событий
// runGreedJS после основного скрипта.
type greedTimer struct {
	fn  goja.Callable
	due time.Time
}

// setGreedVMHelpers устанавливает Go-хелперы для VM: atob/btoa
// (binary-safe), setTimeout/clearTimeout/setInterval/clearInterval —
// отложенно через очередь таймеров (drainGreedTimers).
func setGreedVMHelpers(vm *goja.Runtime, timers *[]greedTimer) {
	vm.Set("__atob", func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		runes := make([]rune, len(raw))
		for i, b := range raw {
			runes[i] = rune(b)
		}
		return vm.ToValue(string(runes))
	})
	vm.Set("__btoa", func(call goja.FunctionCall) goja.Value {
		s := call.Argument(0).String()
		buf := make([]byte, 0, len(s))
		for _, r := range s {
			buf = append(buf, byte(r&0xff))
		}
		return vm.ToValue(base64.StdEncoding.EncodeToString(buf))
	})
	schedule := func(call goja.FunctionCall) {
		if fn, ok := goja.AssertFunction(call.Argument(0)); ok {
			delay := 0.0
			if arg := call.Argument(1); arg != goja.Undefined() && arg != goja.Null() {
				delay = arg.ToFloat()
			}
			*timers = append(*timers, greedTimer{
				fn:  fn,
				due: time.Now().Add(time.Duration(delay * float64(time.Millisecond))),
			})
		}
	}
	vm.Set("setTimeout", func(call goja.FunctionCall) goja.Value {
		schedule(call)
		return vm.ToValue(len(*timers))
	})
	vm.Set("setInterval", func(call goja.FunctionCall) goja.Value {
		schedule(call)
		return vm.ToValue(len(*timers))
	})
	vm.Set("clearTimeout", func(goja.FunctionCall) goja.Value { return goja.Undefined() })
	vm.Set("clearInterval", func(goja.FunctionCall) goja.Value { return goja.Undefined() })
}

// drainGreedTimers — цикл событий: ждёт результат из VM, исполняя
// наступившие таймеры (после каждого батча — микро-джобы промисов
// дрейнятся goja в конце RunString).
func drainGreedTimers(vm *goja.Runtime, timers *[]greedTimer, resultCh <-chan string, errCh <-chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case jsonStr := <-resultCh:
			return jsonStr, nil
		case err := <-errCh:
			return "", err
		default:
		}
		if len(*timers) == 0 {
			select {
			case jsonStr := <-resultCh:
				return jsonStr, nil
			case err := <-errCh:
				return "", err
			case <-time.After(time.Until(deadline)):
				return "", fmt.Errorf("greed vm: timeout after %v", timeout)
			}
		}
		next := (*timers)[0].due
		for _, t := range *timers {
			if t.due.Before(next) {
				next = t.due
			}
		}
		if wait := time.Until(next); wait > 0 {
			select {
			case jsonStr := <-resultCh:
				return jsonStr, nil
			case err := <-errCh:
				return "", err
			case <-time.After(wait):
			}
		}
		var due, rest []greedTimer
		now := time.Now()
		for _, t := range *timers {
			if !t.due.After(now) {
				due = append(due, t)
			} else {
				rest = append(rest, t)
			}
		}
		*timers = rest
		for _, t := range due {
			if _, err := t.fn(goja.Undefined()); err != nil {
				return "", fmt.Errorf("greed timer: %w", err)
			}
		} // дренируем микро-джобы промисов, порождённые таймерами
		if _, err := vm.RunString("void 0;"); err != nil {
			return "", err
		}
	}
}

func runGreedJS(greedSrc, pageURL string, timeout time.Duration) (string, error) {
	vm := goja.New()
	var timers []greedTimer
	setGreedVMHelpers(vm, &timers)

	stub := greedStubJS
	if pageURL != "" {
		stub = strings.Replace(stub,
			"https://docs.yandex.ru/showcaptcha?cc=1", pageURL, 1)
	}
	if _, err := vm.RunString(stub); err != nil {
		return "", fmt.Errorf("greed stub: %w", err)
	}
	if _, err := vm.RunString(greedSrc); err != nil {
		return "", fmt.Errorf("greed.js: %w", err)
	}

	// safeGet() возвращает Promise — разворачиваем через then.
	// ВАЖНО: JSON.stringify выполняем внутри VM — Go-маршалинг map
	// сортирует ключи алфавитно, а порядок полей (суффиксы ;N —
	// индексы) должен сохраняться как у greed.js.
	resultCh := make(chan string, 1)
	errCh := make(chan error, 1)
	vm.Set("__resolve", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		resultCh <- call.Argument(0).String()
		return goja.Undefined()
	}))
	vm.Set("__reject", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		errCh <- fmt.Errorf("safeGet rejected: %v", call.Argument(0))
		return goja.Undefined()
	}))
	vm.Set("__emit", vm.ToValue(func(call goja.FunctionCall) goja.Value {
		resultCh <- call.Argument(0).String()
		return goja.Undefined()
	}))
	_, err := vm.RunString(`
		if (!window.PGreed) { __reject('PGreed not found'); }
		else {
			var p = window.PGreed.safeGet();
			if (p && typeof p.then === 'function') {
				p.then(function(v) { __emit(JSON.stringify(v)); }, __reject);
			}
			else { __emit(JSON.stringify(p)); }
		}
	`)
	if err != nil {
		return "", fmt.Errorf("safeGet call: %w", err)
	}

	return drainGreedTimers(vm, &timers, resultCh, errCh, timeout)
}

// buildGreedRdata — загружает greed.js с cookies текущей сессии,
// выполняет в JS-ранчере и возвращает rdata = base64(JSON(safeGet())).
func buildGreedRdata(client *http.Client, jar http.CookieJar, pageURL, userAgent string) (string, error) {
	req, err := http.NewRequest("GET", "https://docs.yandex.ru/captchapgrd", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Set("Referer", pageURL)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("greed fetch: %w", err)
	}
	greed, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return "", err
	}
	if len(greed) < 100000 {
		return "", fmt.Errorf("greed fetch: suspicious size %d", len(greed))
	}

	t0 := time.Now()
	fpJSON, err := runGreedJS(string(greed), pageURL, 30*time.Second)
	if err != nil {
		return "", err
	}
	utils.Debugf("[GREED] vm ok: %d bytes, %v", len(fpJSON), time.Since(t0))
	return base64.StdEncoding.EncodeToString([]byte(fpJSON)), nil
}

var rePicSeeds = regexp.MustCompile(`pic:\[(\d+),(\d+)\]`)

// encodePicassoField — picasso в структуре настоящего Chrome: два
// canvas-прогона с seeds из SSR (pic:[N,N]). Сервер не сверяет сами
// хэши (проверено живым тестом), важна структура.
func encodePicassoField(html string) string {
	var seed1, seed2 int
	if m := rePicSeeds.FindStringSubmatch(html); len(m) == 3 {
		seed1, _ = strconv.Atoi(m[1])
		seed2, _ = strconv.Atoi(m[2])
	}
	randHex := func() string {
		b := make([]byte, 16)
		for i := range b {
			b[i] = byte(rand.Intn(256))
		}
		return hex.EncodeToString(b)
	}
	pic := []map[string]interface{}{
		{"seed": seed1, "rounds": 5, "width": 300, "height": 300, "fontSizeFactor": 1.5, "maxShadowBlur": 50, "result": randHex(), "calcTime": 10 + rand.Intn(15)},
		{"seed": seed2, "rounds": 10, "width": 300, "height": 300, "fontSizeFactor": 2, "maxShadowBlur": 70, "result": randHex(), "calcTime": 8 + rand.Intn(15)},
	}
	raw, _ := json.Marshal(pic)
	return base64.StdEncoding.EncodeToString(raw)
}
