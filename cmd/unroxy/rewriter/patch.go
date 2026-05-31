package rewriter

var MonitorPatch = `
(function(){
var p = location.pathname.split("/")[1];
if (!p) return;
function r(u) {
  if (typeof u !== "string") return u;
  if (u[0] === "/") return "/" + p + u;
  if (u.indexOf("://") > 0) {
    var m = u.match(/^https?:\/\/([^\/]+)(\/.*)?$/);
    if (m) return "/" + m[1] + (m[2] || "/");
  }
  return u;
}
window.fetch = new Proxy(window.fetch, { apply: function(t, _, a) {
  return t.apply(_, [r(a[0]), a[1]]);
}});
var _xo = XMLHttpRequest.prototype.open;
XMLHttpRequest.prototype.open = function(m, u) {
  return _xo.call(this, m, r(u), arguments[2], arguments[3]);
};
window.Worker = new Proxy(Worker, { construct: function(t, a) {
  return new t(r(a[0]), a[1]);
}});
window.WebSocket = new Proxy(WebSocket, { construct: function(t, a) {
  return new t(r(a[0]), a[1]);
}});
window.EventSource = new Proxy(EventSource, { construct: function(t, a) {
  return new t(r(a[0]));
}});
window.open = new Proxy(window.open, { apply: function(t, _, a) {
  return t.apply(_, [r(a[0])].concat(Array.from(a).slice(1)));
}});
var _la = Location.prototype.assign;
Location.prototype.assign = function(u) { return _la.call(this, r(u)); };
var _lr = Location.prototype.replace;
Location.prototype.replace = function(u) { return _lr.call(this, r(u)); };
(function() {
  var _o = Object.getOwnPropertyDescriptor(Location.prototype, "href");
  Object.defineProperty(Location.prototype, "href", {
    get: function() { return _o.get.call(this); },
    set: function(v) { this.assign(r(v)); },
    configurable: true, enumerable: true
  });
})();
var _hp = History.prototype.pushState;
History.prototype.pushState = function(s, t, u) {
  return _hp.call(this, s, t, u ? r(u) : u);
};
var _hr = History.prototype.replaceState;
History.prototype.replaceState = function(s, t, u) {
  return _hr.call(this, s, t, u ? r(u) : u);
};
var _sb = navigator.sendBeacon.bind(navigator);
navigator.sendBeacon = function(u, d) { return _sb(r(u), d); };
var _sw = navigator.serviceWorker.register.bind(navigator.serviceWorker);
navigator.serviceWorker.register = function(u, o) { return _sw(r(u), o); };
})();
`
