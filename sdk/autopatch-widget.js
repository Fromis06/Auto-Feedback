(function () {
  if (typeof window === 'undefined' || typeof document === 'undefined' || window.__AUTOPATCH_INIT__) return;
  window.__AUTOPATCH_INIT__ = true;

  var script = document.currentScript;
  var cfg = Object.assign({
    endpoint: (script && script.dataset && script.dataset.endpoint) || '/api/v1/feedback',
    label: (script && script.dataset && script.dataset.label) || 'Report Bug',
    accent: (script && script.dataset && script.dataset.accent) || '#2563eb',
    position: (script && script.dataset && script.dataset.position) || 'bottom-right'
  }, window.AUTOPATCH_CONFIG || {});

  var breadcrumbs = [], consoleErrors = [], networkLogs = [];
  var pushRing = function (arr, item, max) { arr.push(item); if (arr.length > max) arr.shift(); };
  var now = function () { return new Date().toISOString(); };

  document.addEventListener('click', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('button,a,[role="button"],input[type="submit"]') : e.target;
    if (!el || (el.closest && el.closest('#ap-widget-root'))) return;
    pushRing(breadcrumbs, {
      type: 'click',
      target: (el.tagName || '').toLowerCase() + (el.id ? '#' + el.id : ''),
      id: el.id || '',
      text: (el.innerText || el.value || '').trim().slice(0, 80),
      aria_label: el.getAttribute ? (el.getAttribute('aria-label') || '') : '',
      path: location.pathname + location.search,
      timestamp: now()
    }, 30);
  }, true);

  var recordNav = function () {
    pushRing(breadcrumbs, { type: 'navigation', path: location.pathname + location.search + location.hash, timestamp: now() }, 30);
  };
  window.addEventListener('popstate', recordNav);
  window.addEventListener('hashchange', recordNav);
  ['pushState', 'replaceState'].forEach(function (fn) {
    if (window.history && typeof window.history[fn] === 'function') {
      var orig = window.history[fn];
      window.history[fn] = function () {
        var res = orig.apply(this, arguments);
        recordNav();
        return res;
      };
    }
  });

  window.addEventListener('error', function (e) {
    pushRing(consoleErrors, {
      message: e.message || 'Script error',
      source: (e.filename || '') + (e.lineno ? ':' + e.lineno : ''),
      timestamp: now()
    }, 20);
  });
  window.addEventListener('unhandledrejection', function (e) {
    var r = e.reason;
    pushRing(consoleErrors, {
      message: (r && r.message) ? r.message : String(r || 'Unhandled promise rejection'),
      source: 'unhandledrejection',
      timestamp: now()
    }, 20);
  });

  var origFetch = typeof window.fetch === 'function' ? window.fetch.bind(window) : null;
  if (origFetch) {
    window.fetch = function (input, init) {
      var method = ((init && init.method) || (input && input.method) || 'GET').toUpperCase();
      var rawUrl = typeof input === 'string' ? input : (input && (input.url || input.href)) || '';
      var cleanUrl = rawUrl.replace(/\/\/[^/@]+@/, '//');
      var t0 = Date.now();
      return origFetch(input, init).then(function (res) {
        if (res.status >= 400) {
          pushRing(networkLogs, { method: method, url: cleanUrl, status: res.status, duration_ms: Date.now() - t0, timestamp: now() }, 10);
        }
        return res;
      }, function (err) {
        pushRing(networkLogs, { method: method, url: cleanUrl, status: 0, error: err && err.message ? err.message : 'Network error', duration_ms: Date.now() - t0, timestamp: now() }, 10);
        throw err;
      });
    };
  }

  function mount() {
    var root = document.createElement('div');
    root.id = 'ap-widget-root';
    var side = cfg.position === 'bottom-left' ? 'left:20px;' : 'right:20px;';
    var accent = /^#[0-9a-fA-F]{3,8}$|^[a-zA-Z]+$/.test(cfg.accent) ? cfg.accent : '#2563eb';
    root.innerHTML =
      '<button id="ap-btn" type="button" style="position:fixed;bottom:20px;' + side + 'z-index:2147483646;background:' + accent + ';color:#fff;border:none;border-radius:999px;padding:10px 16px;font:600 13px/1.2 system-ui,sans-serif;cursor:pointer;box-shadow:0 4px 14px rgba(0,0,0,.22)"></button>' +
      '<div id="ap-modal" style="display:none;position:fixed;inset:0;z-index:2147483647;background:rgba(15,23,42,.5);align-items:center;justify-content:center;font:14px/1.4 system-ui,sans-serif">' +
      '<div style="background:#fff;color:#0f172a;width:min(420px,92vw);border-radius:12px;padding:18px;box-shadow:0 16px 40px rgba(0,0,0,.25)">' +
      '<div id="ap-title" style="font-weight:700;font-size:15px;margin-bottom:8px"></div>' +
      '<textarea id="ap-msg" rows="4" placeholder="Describe what went wrong..." style="width:100%;box-sizing:border-box;padding:8px;border:1px solid #cbd5e1;border-radius:8px;resize:vertical;font:inherit"></textarea>' +
      '<div id="ap-status" style="min-height:18px;font-size:12px;margin:6px 0;color:#475569"></div>' +
      '<div style="display:flex;justify-content:flex-end;gap:8px">' +
      '<button id="ap-cancel" type="button" style="padding:7px 12px;border:1px solid #cbd5e1;background:#fff;border-radius:8px;cursor:pointer">Cancel</button>' +
      '<button id="ap-send" type="button" style="padding:7px 14px;border:none;background:' + accent + ';color:#fff;border-radius:8px;font-weight:600;cursor:pointer">Submit</button>' +
      '</div></div></div>';
    document.body.appendChild(root);
    document.getElementById('ap-btn').textContent = cfg.label;
    document.getElementById('ap-title').textContent = cfg.label;

    var modal = document.getElementById('ap-modal'), msg = document.getElementById('ap-msg'), status = document.getElementById('ap-status'), send = document.getElementById('ap-send');
    var toggle = function (open) { modal.style.display = open ? 'flex' : 'none'; if (open) { status.textContent = ''; msg.focus(); } };
    document.getElementById('ap-btn').onclick = function () { toggle(true); };
    document.getElementById('ap-cancel').onclick = function () { toggle(false); };
    send.onclick = function () {
      var text = msg.value.trim();
      if (!text) { status.textContent = 'Please describe the issue.'; return; }
      send.disabled = true; status.textContent = 'Sending...';
      var payload = {
        user_message: text,
        app_commit_sha: window.__APP_COMMIT_SHA__ || '',
        breadcrumbs: breadcrumbs.slice(),
        console_errors: consoleErrors.slice(),
        network_logs: networkLogs.slice(),
        device_meta: { user_agent: navigator.userAgent, viewport: window.innerWidth + 'x' + window.innerHeight, platform: navigator.platform || '', url: location.href }
      };
      (origFetch || window.fetch)(cfg.endpoint, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) })
        .then(function (r) {
          if (!r.ok) throw new Error('HTTP ' + r.status);
          msg.value = ''; status.textContent = 'Submitted! Thank you.';
          setTimeout(function () { toggle(false); send.disabled = false; }, 900);
        })
        .catch(function (err) { status.textContent = 'Failed: ' + (err.message || 'network error'); send.disabled = false; });
    };
  }
  if (document.body) mount(); else document.addEventListener('DOMContentLoaded', mount);
})();
