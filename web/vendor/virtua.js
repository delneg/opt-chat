// virtua 0.53.3 (MIT, github.com/inokawa/virtua): lib/index.js, its React build, run on preact/compat.
"use client";

import { forwardRef as e, memo as t, useEffect as o, useImperativeHandle as n, useLayoutEffect as r, useMemo as s, useReducer as l, useRef as i } from "./compat.js";

import { jsx as c } from "./jsx-runtime.js";

import { flushSync as f } from "./compat.js";

var a, u = [], {min: d, max: h, abs: g, floor: $} = Math, p = (e, t, o) => d(o, h(t, e)), m = e => e.sort((e, t) => e - t), w = setTimeout, S = clearTimeout, _ = "function" == typeof queueMicrotask ? queueMicrotask : e => {
    Promise.resolve().then(e);
}, v = () => {
    let e;
    return [ new Promise(t => {
        e = t;
    }), e ];
}, z = "undefined" != typeof window, y = e => e.ownerDocument, x = e => e.defaultView, b = () => (null == a && (a = /iP(hone|od|ad)/.test(navigator.userAgent) || "MacIntel" === navigator.platform && navigator.maxTouchPoints > 0), 
a), I = e => h(e.t(), e.o()), R = ({l: e, i: t, u: o, h: n, t: r, $: s, p: l, m: i, S: c}, f = 0) => {
    let a = !!f, u = 1, $ = 0, p = 0, m = 0, w = 0, S = 0, _ = 0, v = 0, z = 0, y = null, x = [ 0, a ? h(f - 1, 0) : -1 ], I = !1;
    const R = /* @__PURE__ */ new Set, k = () => m - p, T = () => k() + S + w, O = e => t(e) - S, M = e => {
        if (2 === z) return !0;
        if (y && 1 === z) return e < y[0];
        const t = k(), n = O(e), r = o(e);
        return 1 !== v && 0 === z ? n + r <= t : n < t && n + r < t + $;
    }, E = e => {
        e && (b() && 0 !== v || y && 1 === z ? S += e : w += e);
    };
    return {
        _: () => {
            R.clear();
        },
        v: () => u,
        l: (t = 200) => {
            if (!I || a) return x;
            let o, n;
            if (_) [o, n] = x; else {
                let r = h(0, T()), s = r + $;
                i() || (t = h(0, t), 1 !== v && (r -= t), 2 !== v && (s += t)), [o, n] = x = e(h(0, r), h(0, s)), 
                y && (o = d(o, y[0]), n = h(n, y[1]));
            }
            return [ h(o, 0), d(n, s() - 1) ];
        },
        I: n,
        i: O,
        u: o,
        R: s,
        k: () => m,
        T,
        O: () => 0 !== v,
        o: () => $,
        M: () => p,
        t: r,
        C: () => (_ = w, w = 0, [ _, 2 === z ]),
        L: (e, t) => {
            const o = [ e, t ];
            return R.add(o), () => {
                R.delete(o);
            };
        },
        A: (t, o) => {
            let s, i, f = 0;
            switch (t) {
              case 1:
                {
                    if (o === m && 0 === z) break;
                    const e = _;
                    _ = 0;
                    const t = o - m, n = g(t);
                    e && n < g(e) + 1 || 0 !== z || (v = t < 0 ? 2 : 1), a && (a = !1), m = o, f = 4;
                    const s = k();
                    s >= -$ && s <= r() && (f += 1, i = n > $);
                    break;
                }

              case 2:
                f = 8, 0 !== v && (s = !0, f += 1), v = 0, z = 0, y = null;
                break;

              case 3:
                {
                    const e = o.filter(([e, t]) => !n(e, t));
                    if (!e.length) break;
                    E(c(e, M, T(), $)), f = 3, i = !0;
                    break;
                }

              case 4:
                $ !== o && ($ || (I = i = !0), $ = o, f = 3);
                break;

              case 5:
                o[1] ? (E(l(o[0], !0)), z = 2, f = 1) : (l(o[0]), f = 1);
                break;

              case 6:
                p = o;
                break;

              case 7:
                z = 1;
                break;

              case 9:
                null != o && (E(o), f = 1);
                break;

              case 8:
                y = e(o, o + $), f = 1;
            }
            f && (u = 1 + (2147483647 & u), s && S && (w += S, S = 0), R.forEach(([e, t]) => {
                f & e && t(i);
            }));
        }
    };
}, k = (e, t, o) => {
    const n = o ? "unshift" : "push";
    for (let o = 0; o < t; o++) e[n](-1);
    return e;
}, T = (e, t, o, n = 0, r = t - 1) => {
    let s = n;
    for (;n <= r; ) {
        const t = $((n + r) / 2);
        e(t) <= o ? (s = t, n = t + 1) : r = t - 1;
    }
    return p(s, 0, t - 1);
}, O = (e, t, o) => {
    let n = o && o[1] || t || 40, r = -1, s = 0, l = !t, i = 0;
    const c = o && o[0], f = c ? k(c.slice(0, d(e, c.length)), h(0, e - c.length)) : k([], e), a = k([], e + 1), u = e => {
        const t = f[e];
        return -1 === t ? n : t;
    }, g = t => {
        if (!e) return 0;
        if (r >= t) return a[t];
        r < 0 && (a[0] = 0, r = 0);
        let o = r, n = a[o];
        for (;o < t; ) n += u(o), a[++o] = n;
        return r = t, n;
    };
    return {
        l: (t, o) => {
            let n, r;
            return i = d(i, e - 1), g(i) <= t ? (r = T(g, e, o, i), n = T(g, e, t, i, r)) : (n = T(g, e, t, void 0, i), 
            r = T(g, e, o, n)), i = n, [ n, r ];
        },
        W: t => T(g, e, t),
        i: g,
        u,
        h: (e, t = -1) => f[e] === t,
        S: (t, o, i, c) => {
            let a = t.reduce((e, [t, n]) => o(t) ? e + (n - u(t)) : e, 0);
            for (const [e, o] of t) s += -1 === f[e] ? o : o - u(e), f[e] = o, r = d(e, r);
            if (l && c && s > c) {
                let t = 0;
                const o = T(g, e, i + a), s = [];
                f.forEach((e, n) => {
                    -1 !== e && (e && s.push(e), n < o && t++);
                }), r = -1, m(s);
                const c = s.length, u = c / 2 | 0, d = c % 2 == 0 ? (s[u - 1] + s[u]) / 2 : s[u], $ = n;
                a += ((n = d) - $) * h(o - t, 0), l = !1;
            }
            return a;
        },
        t: () => g(e),
        $: () => e,
        p: (t, o) => {
            const s = t - e;
            return r = o ? -1 : d(t - 1, r), e = t, s > 0 ? (k(a, s), k(f, s, o), n * s) : (a.splice(s), 
            (o ? f.splice(0, -s) : f.splice(s)).reduce((e, t) => e - (-1 === t ? n : t), 0));
        },
        m: () => l,
        H: () => [ f.slice(), n ]
    };
}, M = e => "number" == typeof e ? e : e.length, E = (e, t) => "number" == typeof e ? t : e[t], C = (e, t, o = 0) => {
    let n = M(e);
    const r = "number" == typeof t, s = "auto" === t;
    let l = (r ? t : 40) + o, i = -1, c = 0, f = s, a = 0, u = 0, g = null;
    const w = [], S = [], _ = [], v = e => s || !!_[e], z = () => {
        w.length || k(w, n);
    }, y = e => {
        const t = w[e];
        return -1 === t ? l : t;
    }, x = e => {
        if (!n) return 0;
        if (!w.length) return e * l;
        if (i >= e) return S[e];
        i < 0 && (S[0] = 0, i = 0);
        let t = i, o = S[t];
        for (;t < e; ) o += y(t), S[++t] = o;
        return i = e, o;
    }, b = (e, t) => {
        w[e] = t, i = d(e, i);
    }, I = e => w.length ? T(x, n, e) : l ? p($(e / l), 0, n - 1) : 0, R = () => d(a, n), O = () => h(n - u, R()), E = e => (w.length ? y(e) : l) - o;
    return {
        l: (e, t) => {
            const r = x(R()), s = x(n) - x(O()), l = e + (r ? r - o : 0);
            return [ I(l), I(h(l, t - (s ? s - o : 0))) ];
        },
        W: I,
        i: x,
        u: E,
        S: (e, t, n, r) => {
            let s = e.reduce((e, [o, n]) => v(o) && t(o) ? e + (n - E(o)) : e, 0);
            for (const [t, n] of e) v(t) && (z(), c += -1 === w[t] ? n : n - E(t), b(t, n + o));
            if (f && r && c > r) {
                let e = 0;
                const t = I(n + s), o = [];
                w.forEach((n, r) => {
                    -1 !== n && (n && o.push(n), r < t && e++);
                }), i = -1, m(o);
                const r = o.length, c = r / 2 | 0, a = r % 2 == 0 ? (o[c - 1] + o[c]) / 2 : o[c], u = l;
                s += ((l = a) - u) * h(t - e, 0), f = !1;
            }
            return s;
        },
        h: (e, t = -1) => {
            const n = w.length ? w[e] : -1;
            return -1 === t ? v(e) && -1 === n : n === t + o;
        },
        t: () => n ? x(n) - o : 0,
        $: () => n,
        p: e => {
            const t = e - n;
            return i = d(e - 1, i), n = e, w.length && (t > 0 ? k(w, t) : w.splice(t)), 0;
        },
        m: () => f,
        q: v,
        B: (e = 0, t = 0) => {
            a = e, u = t;
        },
        P: R,
        V: O,
        Y: (e, t) => "number" == typeof t ? r ? t : null : r || s || "auto" === t ? null : e.map(e => null != e ? e[t] : null),
        X: (e, t) => {
            if (null == e) return;
            if ("number" == typeof e) {
                const n = e + o;
                if (n === l) return;
                const r = I(t) * (n - l);
                return l = n, r;
            }
            if (e === g && n === _.length) return;
            g = e, _.length = n, z();
            const r = I(t), s = x(r);
            let i;
            for (let t = 0; t < n; t++) {
                const n = e[t], r = "number" != typeof n;
                if (!r || !_[t]) {
                    _[t] = r;
                    const e = r ? -1 : n + o;
                    w[t] !== e && (b(t, e), i = !0);
                }
            }
            return i ? x(r) - s : void 0;
        }
    };
}, L = /*#__PURE__*/ new WeakMap, A = (e, t, o) => {
    const n = [];
    for (const r of e) r >= t && r < o && n.push(r);
    return m(n);
}, W = (e, t, o) => {
    if (t >= o) return -1;
    let n = 0, r = e.length;
    for (;n < r; ) {
        const o = n + r >> 1;
        e[o] <= t ? n = o + 1 : r = o;
    }
    return n - 1;
}, H = (e, t, o, n, r) => {
    const s = t.length;
    for (;o < s && t[o] < n; o++) {
        const n = t[o];
        n > r && (e.push(n), r = n);
    }
    return o;
}, q = (e, t, o, n, r, s) => {
    const l = [];
    for (let e = 0; e < o; e++) l.push(e);
    const i = H(l, e, 0, n, o - 1);
    for (let e = n; e <= r; e++) l.push(e);
    H(l, e, i, s, r);
    for (let e = s; e < t; e++) l.push(e);
    return l;
}, B = (e, t) => d(e.rowIndex + (e.rowSpan || 1), t), P = (e, t) => d(e.colIndex + (e.colSpan || 1), t), V = (e, t, o, n, r, s) => {
    const l = t.length, i = n.length;
    let c = "[l0]", f = 0, a = 0, u = 0;
    for (;f < s; ) {
        for (;u < i && n[u] <= f; ) u++;
        const h = a < l ? t[a] : s;
        let g, $;
        if (h === f) {
            g = f + 1;
            const t = o[a];
            $ = null == t ? e.u(f) + "px" : t ? r : "minmax(" + e.u(f) + "px,auto)", a++;
        } else g = d(h, u < i ? n[u] : s), $ = "minmax(" + (e.i(g - 1) + e.u(g - 1) - e.i(f)) + "px,auto)";
        c += " " + $ + " [l" + g + "]", f = g;
    }
    return c;
}, Y = (e, t, o, n, r) => {
    const s = {
        display: "grid",
        gridTemplateRows: "subgrid",
        gridTemplateColumns: "subgrid",
        gridColumn: "1/-1",
        gridRow: "l" + e + "/l" + t
    };
    return null != o && (s.position = "sticky", s[n] = o + "px", s.zIndex = r), s;
}, X = e => {
    let t;
    return {
        J(o) {
            (t || (t = new (x(y(o)).ResizeObserver)(e))).observe(o);
        },
        j(e) {
            t.unobserve(e);
        },
        D() {
            t && t.disconnect();
        }
    };
}, J = (e, t, o, n, r, s, l) => {
    let i, c, f = 0, a = !1, u = !1, d = !1, h = !1;
    const g = Date.now, $ = n ? "scrollLeft" : "scrollTop", p = n ? "left" : "top", m = e => r ? -e : e, v = () => m(o[$]), z = (e, t) => {
        o.scrollTo({
            [p]: m(e),
            behavior: t ? "smooth" : "instant"
        });
    }, y = () => {
        if (a || u) return a = !1, void x();
        d = !1, e.A(2);
    }, x = () => {
        S(c), c = w(y, 150);
    }, I = () => {
        f = g(), d && (h = !0), l && e.A(6, l()), e.A(1, v()), x();
    }, R = t => {
        if (a || !e.O() || t.ctrlKey) return;
        const o = g() - f;
        150 > o && 50 < o && (n ? t.deltaX : t.deltaY) && (a = !0);
    }, k = () => {
        u = !0, d = h = !1;
    }, T = () => {
        u = !1, b() && (d = !0);
    };
    return t.addEventListener("scroll", I), t.addEventListener("wheel", R, {
        passive: !0
    }), t.addEventListener("touchstart", k, {
        passive: !0
    }), t.addEventListener("touchend", T, {
        passive: !0
    }), {
        G: I,
        D: () => {
            t.removeEventListener("scroll", I), t.removeEventListener("wheel", R), t.removeEventListener("touchstart", k), 
            t.removeEventListener("touchend", T), S(c);
        },
        U: () => {
            const [t, n] = e.C();
            if (!t) return;
            h && s && s(), h = !1;
            const r = e.k(), l = r + t, c = e.M() + e.t() - e.o();
            l <= 0 || l >= c || r >= c ? z(l) : o.scrollBy({
                [p]: m(t),
                behavior: "instant"
            }), n && (i && i(), e.o() > e.t() && e.A(1, v()));
        },
        F: (t, o) => {
            let n, r, s;
            i && i();
            const l = i = () => {
                n = !0, S(r), s && s();
            }, c = () => {
                if (!n) {
                    if (e.o() && (S(r), r = w(l, 150)), o) {
                        for (let [t, o] = e.l(0); t <= o; t++) if (e.I(t)) return;
                        l();
                    }
                    e.A(7), z(t(), o);
                }
            };
            o && e.A(8, t()), (() => {
                if (n) return;
                let t;
                s = e.L(2, () => {
                    t || (t = !0, _(() => {
                        t = !1, c();
                    }));
                }), c();
            })();
        }
    };
}, j = (e, t) => {
    let o, n, r = v(), s = !0;
    const l = /* @__PURE__ */ new WeakMap, i = X(r => {
        const i = [];
        for (const {target: c, contentRect: {width: f, height: a}} of r) if (c === o) f || a ? (s || n.G(), 
        s = !0, e.A(4, t ? f : a)) : s = !1; else if (c.offsetParent) {
            const e = l.get(c);
            null != e && i.push([ e, t ? f : a ]);
        }
        i.length && e.A(3, i);
    });
    return {
        K(s, l = s.parentElement) {
            i.J(o = l), n = J(e, l, l, t, t && "rtl" === getComputedStyle(l).direction, () => {
                const e = t ? "overflowX" : "overflowY", o = l.style, n = o[e];
                o[e] = "hidden", w(() => {
                    o[e] = n;
                });
            }), r[1](!0);
        },
        _() {
            i.D(), n && n.D(), r[1](!1), r = v();
        },
        N: (e, t) => (l.set(e, t), i.J(e), () => {
            l.delete(e), i.j(e);
        }),
        async Z(e, t) {
            await r[0] && n.F(e, t);
        },
        ee() {
            n && n.U();
        },
        te: e.M
    };
}, D = (e, t) => {
    let o, n, r, s, l, i = v();
    const c = /* @__PURE__ */ new WeakMap, f = X(n => {
        const r = [];
        for (const {target: e, contentRect: {width: s, height: i}} of n) if (e === o) l && l(); else if (e.offsetParent) {
            const o = c.get(e);
            null != o && r.push([ o, t ? s : i ]);
        }
        r.length && e.A(3, r);
    });
    return {
        K(c) {
            const a = y(c), u = x(a), d = o = a.scrollingElement, h = t && "rtl" === getComputedStyle(d).direction, g = (e, t, o, n, r = 0) => {
                const s = n ? "offsetLeft" : "offsetTop", l = r + (h ? t.clientWidth - e[s] - e.offsetWidth : e[s]), i = e.offsetParent;
                return e !== o && i ? g(i, t, o, n, l) : l;
            }, $ = s = () => g(c, d, a.body, t), p = l = () => {
                e.A(4, d[t ? "clientWidth" : "clientHeight"]);
            };
            u.addEventListener("resize", p), f.J(d), r = () => {
                u.removeEventListener("resize", p);
            }, n = J(e, u, d, t, h, null, $), i[1](!0);
        },
        _() {
            r && r(), f.D(), n && n.D(), i[1](!1), i = v();
        },
        N: (e, t) => (c.set(e, t), f.J(e), () => {
            c.delete(e), f.j(e);
        }),
        async Z(e, t) {
            await i[0] && n.F(e, t);
        },
        ee() {
            n && n.U();
        },
        te: () => s()
    };
}, G = (e, t) => {
    let o, n, r, s = v(), l = !0;
    const i = /* @__PURE__ */ new WeakMap, c = /* @__PURE__ */ new WeakMap, f = X(s => {
        const f = [], a = [];
        for (const {target: u, contentRect: {width: d, height: h}} of s) if (u === o) d || h ? (l || (n.G(), 
        r.G()), l = !0, e.A(4, h), t.A(4, d)) : l = !1; else if (u.offsetParent) {
            const e = i.get(u), t = c.get(u);
            null != e && f.push([ e, h ]), null != t && a.push([ t, d ]);
        }
        f.length && e.A(3, f), a.length && t.A(3, a);
    });
    return {
        async Z(e, t) {
            await s[0] && (e ? r : n).F(t);
        },
        K(l, i = l.parentElement) {
            f.J(o = i);
            const c = (e, t) => J(e, i, i, t, t && "rtl" === getComputedStyle(i).direction, () => {
                const e = t ? "overflowX" : "overflowY", o = i.style, n = o[e];
                o[e] = "hidden", w(() => {
                    o[e] = n;
                });
            });
            n = c(e, !1), r = c(t, !0), s[1](!0);
        },
        _() {
            f.D(), n && n.D(), r && r.D(), s[1](!1), s = v();
        },
        N: (e, t, o) => (null != t && i.set(e, t), null != o && c.set(e, o), f.J(e), () => {
            i.delete(e), c.delete(e), f.j(e);
        }),
        ee() {
            n && n.U(), r && r.U();
        }
    };
}, U = (e, t) => {
    e.Z(() => t);
}, F = (e, t, o) => {
    U(e, o + t.k());
}, K = (e, t, o, {align: n, smooth: r, offset: s = 0} = {}) => {
    if (o = p(o, 0, t.R() - 1), "nearest" === n) {
        const e = t.i(o), r = t.k();
        if (e < r) n = "start"; else {
            if (!(e + t.u(o) > r + t.o())) return;
            n = "end";
        }
    }
    e.Z(() => s + e.te() + t.i(o) + ("end" === n ? t.u(o) - t.o() : "center" === n ? (t.u(o) - t.o()) / 2 : 0), r);
}, N = (e, t, o, n, r, s, l) => {
    const i = t.R(), c = o.P(), f = o.V();
    r = p(r, 0, i - 1);
    const a = A(n, c, f), u = W(a, r, f), g = u < 0 ? -1 : a[u], $ = () => o.i(c) + (g < 0 || g === r ? 0 : o.i(g + 1) - o.i(g)), m = () => o.i(i) - o.i(f);
    if ("nearest" === s) {
        if (r < c || r >= f) return;
        const e = $(), o = t.k() + e, n = t.o() - e - m(), l = t.u(r);
        let i = t.i(r);
        if (g === r) {
            const e = (u + 1 < a.length ? a[u + 1] : f) - 1;
            i = h(i, d(o, t.i(e) + t.u(e) - l));
        }
        if (i < o) s = "start"; else {
            if (!(i + l > o + n)) return;
            s = "end";
        }
    }
    e.Z(l, () => {
        const e = $(), o = t.u(r) - (t.o() - e - m());
        return t.i(r) - e + ("end" === s ? o : "center" === s ? o / 2 : 0);
    });
}, Q = z ? r : o, Z = "current", ee = (e, t) => {
    if (Array.isArray(e)) for (const o of e) ee(o, t); else null == e || "boolean" == typeof e || t.push(e);
}, te = (e, t) => {
    const o = e.key;
    return null != o ? o : "_" + t;
}, oe = e => {
    const t = i(null);
    return t.current || (t.current = e());
}, ne = e => {
    const t = i(e);
    return Q(() => {
        t[Z] = e;
    }, [ e ]), t;
}, re = /*#__PURE__*/ t(({oe: e, ne: t, re: o, se: n, le: r, ie: l, ce: f, fe: a}) => {
    const u = i(null);
    Q(() => t(u[Z], o), [ o ]);
    const d = s(() => {
        const e = {
            contain: "layout style",
            position: r && a ? void 0 : "absolute",
            [f ? "height" : "width"]: "100%",
            [f ? "top" : "left"]: 0,
            [f ? "insetInlineStart" : "top"]: n,
            visibility: !r || a ? void 0 : "hidden"
        };
        return f && (e.display = "inline-flex"), e;
    }, [ n, r, a, f ]);
    /* @__PURE__ */
    return c(l, "string" == typeof l ? {
        ref: u,
        style: d,
        children: e
    } : {
        ref: u,
        style: d,
        index: o,
        children: e
    });
}), se = (e, t) => s(() => {
    if ("function" == typeof e) return [ o => e(t[o], o), t.length ];
    const o = (e => {
        const t = [];
        return ee(e, t), t;
    })(e);
    return [ e => o[e], o.length ];
}, [ e, t ]), le = /*#__PURE__*/ e(({children: e, data: t, bufferSize: o, itemSize: r, shift: s, horizontal: a, keepMounted: u, cache: d, startMargin: h = 0, ssrCount: g, as: $ = "div", item: p = "div", scrollRef: w, onScroll: S, onScrollEnd: v, onResize: z}, y) => {
    "use no memo";
    const [x, b] = se(e, t), k = i(null), T = i(!!g), M = ne(S), E = ne(v), C = ne(z), [L, A, W, H] = oe(() => {
        const e = !!a, t = O(b, r, d), o = R(t, g);
        return [ o, t, j(o, e), e ];
    });
    b !== L.R() && L.A(5, [ b, s ]), h !== L.M() && L.A(6, h);
    const [q, B] = l(L.v, void 0, L.v), P = L.O(), V = L.t(), Y = [], X = e => {
        const t = x(e);
        /* @__PURE__ */
        return c(re, {
            ne: W.N,
            re: e,
            se: L.i(e),
            le: L.I(e),
            ie: p,
            oe: t,
            ce: H,
            fe: T[Z]
        }, te(t, e));
    };
    if (Q(() => {
        T[Z] = !1, L.L(1, e => {
            e ? f(B) : B();
        }), L.L(4, () => {
            M.current && M.current(L.k());
        }), L.L(8, () => {
            E.current && E.current();
        }), L.L(2, () => {
            C.current && C.current();
        });
        const e = k[Z];
        return w ? _(() => {
            w.current && W.K(e, w[Z]);
        }) : W.K(e), () => {
            L._(), W._();
        };
    }, []), Q(() => {
        W.ee();
    }, [ q ]), n(y, () => ({
        get cache() {
            return A.H();
        },
        get scrollOffset() {
            return L.k();
        },
        get scrollSize() {
            return I(L);
        },
        get viewportSize() {
            return L.o();
        },
        findItemIndex: e => A.W(e - L.M()),
        getItemOffset: L.i,
        getItemSize: L.u,
        scrollToIndex: (e, t) => K(W, L, e, t),
        scrollTo: e => U(W, e),
        scrollBy: e => F(W, L, e)
    }), []), u) {
        const e = new Set(u);
        for (let [t, n] = L.l(o); t <= n; t++) e.add(t);
        m([ ...e ]).forEach(e => {
            e < b && Y.push(X(e));
        });
    } else for (let [e, t] = L.l(o); e <= t; e++) Y.push(X(e));
    /* @__PURE__ */
    return c($, {
        ref: k,
        style: {
            contain: "size style",
            overflowAnchor: "none",
            flex: "none",
            position: "relative",
            width: H ? V : "100%",
            height: H ? "100%" : V,
            pointerEvents: P ? "none" : void 0
        },
        children: Y
    });
}), ie = /*#__PURE__*/ e(({children: e, data: t, bufferSize: o, itemSize: n, shift: r, horizontal: s, keepMounted: l, cache: i, ssrCount: f, item: a, onScroll: u, onScrollEnd: d, onResize: h, style: g, ...$}, p) => /* @__PURE__ */ c("div", {
    ...$,
    style: {
        display: "block",
        [s ? "overflowX" : "overflowY"]: "auto",
        contain: "strict",
        width: "100%",
        height: "100%",
        ...g
    },
    children: /* @__PURE__ */ c(le, {
        ref: p,
        data: t,
        bufferSize: o,
        itemSize: n,
        shift: r,
        horizontal: s,
        keepMounted: l,
        cache: i,
        ssrCount: f,
        item: a,
        onScroll: u,
        onScrollEnd: d,
        onResize: h,
        children: e
    })
})), ce = /*#__PURE__*/ e(({children: e, data: t, bufferSize: o, itemSize: r, shift: s, horizontal: a, cache: u, ssrCount: d, as: h = "div", item: g = "div", onScroll: $, onScrollEnd: p, onResize: m}, w) => {
    "use no memo";
    const [S, _] = se(e, t), v = i(null), z = ne($), y = ne(p), x = ne(m), b = i(!!d), [I, k, T, M] = oe(() => {
        const e = !!a, t = O(_, r, u), o = R(t, d);
        return [ o, t, D(o, e), e ];
    });
    _ !== I.R() && I.A(5, [ _, s ]);
    const [E, C] = l(I.v, void 0, I.v), L = I.O(), A = I.t(), W = [];
    Q(() => (b[Z] = !1, I.L(1, e => {
        e ? f(C) : C();
    }), I.L(4, () => {
        z.current && z.current();
    }), I.L(8, () => {
        y.current && y.current();
    }), I.L(2, () => {
        x.current && x.current();
    }), T.K(v[Z]), () => {
        I._(), T._();
    }), []), Q(() => {
        T.ee();
    }, [ E ]), n(w, () => ({
        get cache() {
            return k.H();
        },
        get scrollOffset() {
            return I.k();
        },
        get viewportSize() {
            return I.o();
        },
        findItemIndex: e => k.W(e - I.M()),
        getItemOffset: I.i,
        getItemSize: I.u,
        scrollToIndex: (e, t) => K(T, I, e, t)
    }), []);
    for (let [e, t] = I.l(o); e <= t; e++) {
        const t = S(e);
        W.push(/* @__PURE__ */ c(re, {
            ne: T.N,
            re: e,
            se: I.i(e),
            le: I.I(e),
            ie: g,
            oe: t,
            ce: M,
            fe: b[Z]
        }, te(t, e)));
    }
    /* @__PURE__ */
    return c(h, {
        ref: v,
        style: {
            contain: "size style",
            overflowAnchor: "none",
            flex: "none",
            position: "relative",
            width: M ? A : "100%",
            height: M ? "100%" : A,
            pointerEvents: L ? "none" : void 0
        },
        children: W
    });
}), fe = /*#__PURE__*/ t(({ae: {ue: e, de: t, he: o, ge: n, $e: r, pe: s, me: l, we: f}, oe: a, Se: u, _e: d, ve: h, ne: g}) => {
    const $ = i(null);
    return Q(() => {
        if (null != n || null != r) return g($[Z], n, r);
    }, [ n, r, g ]), /* @__PURE__ */ c("div", {
        ref: $,
        role: s,
        "aria-colindex": e + 1,
        "aria-rowspan": t,
        "aria-colspan": o,
        "aria-sort": l,
        style: f,
        children: a(u, d, {
            rowIndex: h,
            colIndex: e
        })
    });
}), ae = /*#__PURE__*/ t(({ae: {ze: e, ye: t, we: o}, oe: n, Se: r, xe: s, ne: l}) => /* @__PURE__ */ c("div", {
    role: "row",
    "aria-rowindex": e + 1,
    style: o,
    children: t.map(t => /* @__PURE__ */ c(fe, {
        ae: t,
        oe: n,
        Se: r,
        _e: E(s, t.ue),
        ve: e,
        ne: l
    }, t.ue))
})), ue = ({ae: {be: e, we: t}, oe: o, Ie: n, xe: r, ne: s}) => /* @__PURE__ */ c("div", {
    role: "rowgroup",
    style: t,
    children: e.map(e => /* @__PURE__ */ c(ae, {
        ae: e,
        oe: o,
        Se: E(n, e.ze),
        xe: r,
        ne: s
    }, e.ze))
}), de = /*#__PURE__*/ e(({children: e, rows: t, cols: o, rowHeight: r, colWidth: a, headerRows: g, sectionRows: $, footerRows: w, headerCols: S, footerCols: v, spans: z, keepMounted: y, bufferSize: x, gap: b = 0, ariaSort: k, onVerticalScroll: T, onHorizontalScroll: O, onScrollEnd: H, onResize: X, style: J, ...j}, D) => {
    "use no memo";
    const U = i(null), F = ne(T), K = ne(O), ee = ne(H), te = ne(X), [re, se, le, ie, ce] = oe(() => {
        const e = C(t, r, b), n = C(o, a, b), s = R(e), l = R(n);
        return [ s, l, e, n, G(s, l) ];
    }), fe = s(() => le.Y(t, r), [ t, r ]), de = s(() => ie.Y(o, a), [ o, a ]), he = s(() => ((e = u) => {
        const t = /* @__PURE__ */ new Map;
        for (const o of e) {
            const e = o.rowIndex + (o.rowSpan || 1);
            for (let n = o.rowIndex; n < e; n++) {
                const e = t.get(n);
                e ? e.push(o) : t.set(n, [ o ]);
            }
        }
        return t;
    })(z), [ z ]);
    le.B(g, w), ie.B(S, v);
    const ge = M(t), $e = M(o);
    ge !== re.R() && re.A(5, [ ge ]), $e !== se.R() && se.A(5, [ $e ]), re.A(9, le.X(fe, re.T())), 
    se.A(9, ie.X(de, se.T()));
    const pe = () => re.v() + se.v(), [me, we] = l(pe, void 0, pe), Se = re.O() || se.O(), _e = re.i(0), ve = se.i(0), ze = re.R(), ye = se.R(), xe = ((e, t, [o, n], [r, s], l, i = u, c = u, f) => {
        const a = e.$(), g = t.$(), $ = e.P(), w = e.V(), S = t.P(), _ = t.V();
        o = p(o, $, w), n = d(n, w - 1), r = p(r, S, _), s = d(s, _ - 1);
        const v = A(i, $, w), z = h(S - 1, 0), y = [], x = [], b = [];
        for (const e of c) e.rowIndex < a && e.colIndex < g && (b.push(e), y.push(e.rowIndex));
        const I = b.length, R = W(v, o, w), k = R < 0 ? -1 : v[R];
        k >= 0 && y.push(k);
        const T = q(m(y), a, $, o, n, w);
        for (let e = 0; e < T.length; e++) for (const t of l.get(T[e]) || u) {
            const {rowIndex: l, colIndex: i} = t, c = B(t, a), f = P(t, g);
            if ((e < 1 || T[e - 1] < l) && i < g && (c - l > 1 || f - i > 1)) {
                let e = (l < $ || h(l, o) < d(c, n + 1) || w < c || k >= l && k < c) && (i < S || h(i, r) < d(f, s + 1) || _ < f);
                for (let t = 0; !e && t < I; t++) {
                    const o = b[t];
                    e = o.rowIndex >= l && o.rowIndex < c && o.colIndex >= i && o.colIndex < f;
                }
                e && b.push(t);
            }
        }
        for (const {rowIndex: e, colIndex: t} of b) {
            const o = W(v, e, w);
            y.push(e), o >= 0 && y.push(v[o]), x.push(t);
        }
        m(y), m(x);
        const O = q(y, a, $, o, n, w), M = q(x, g, S, r, s, _), E = O.length, C = M.length, H = /* @__PURE__ */ new Map, X = [], J = [];
        for (const e of b) {
            const t = B(e, a), o = P(e, g);
            X.push(t), J.push(o);
            const n = M.indexOf(e.colIndex);
            for (let r = O.indexOf(e.rowIndex); r < E && O[r] < t; r++) {
                const t = O[r] * g;
                for (let r = n; r < C && M[r] < o; r++) H.set(t + M[r], e);
            }
        }
        const j = [];
        for (const e of M) j.push(!t.q(e) && null);
        const D = [], G = e.i($), U = [];
        let F = U;
        const K = L.get(e), N = /* @__PURE__ */ new Map;
        for (const l of O) {
            const i = l < $, c = l >= w, u = W(v, l, w), d = i ? 0 : c ? w : u < 0 ? -1 : v[u];
            if (l === d) {
                const e = i ? $ : c ? a : u + 1 < v.length ? v[u + 1] : w;
                X.push(e);
                const t = [];
                U.push({
                    Re: i ? -1 : c ? -2 : -3 - d,
                    be: t,
                    we: Y(d, e, i || c ? 0 : null, c ? "bottom" : "top", 4)
                }), F = t;
            } else d < 0 && (F = U);
            const p = u >= 0 && l === d, m = i || c || l === k || l >= o && l <= n, y = p ? G : null, x = l * g, b = K && K.get(l), I = b && b.ye, R = [];
            let T = 0, O = !e.q(l) && null, E = l + 1, L = !1;
            for (let e = 0; e < C; e++) {
                const o = M[e], n = o < S, c = o >= _, u = n || c || o >= r && o <= s, d = H.get(x + o);
                if (d ? d.rowIndex === l && d.colIndex === o : m && u) {
                    const r = d ? B(d, a) : l + 1, s = d ? P(d, g) : o + 1, u = r - l, $ = s - o, m = u > 1, w = $ > 1, S = m || w;
                    let _, v;
                    E = h(E, r);
                    const y = n ? t.i(o) : null, x = c ? t.i(g) - t.i(s) : null;
                    !1 !== O || m || (_ = l, O = !0), !1 !== j[e] || w || (v = o, j[e] = !0);
                    const b = i ? "columnheader" : o === z && (n || p) ? "rowheader" : "cell", k = S ? u : void 0, M = S ? $ : void 0, C = "cell" !== b && f && f.rowIndex === l && f.colIndex === o ? f.order : void 0;
                    let A;
                    if (I) {
                        const e = I.length;
                        for (;T < e && I[T].ue < o; ) T++;
                        T < e && I[T].ue === o && (A = I[T]);
                    }
                    if (!A || A.de !== k || A.he !== M || A.ke !== y || A.Te !== x || A.ge !== _ || A.$e !== v || A.pe !== b || A.me !== C) {
                        let e = "1/l" + o;
                        S && (e += "/l" + r + "/l" + s);
                        const t = {
                            contain: "layout style",
                            display: "grid",
                            gridArea: e
                        };
                        m && (t.height = "0px", t.minHeight = "100%", t.zIndex = 1), w && (t.width = "0px", 
                        t.minWidth = "100%"), (n || c) && (t[n ? "insetInlineStart" : "insetInlineEnd"] = (n ? y : x) + "px", 
                        t.position = "sticky", t.zIndex = 2), A = {
                            ue: o,
                            de: k,
                            he: M,
                            ge: _,
                            $e: v,
                            pe: b,
                            me: C,
                            we: t,
                            ke: y,
                            Te: x
                        }, L = !0;
                    }
                    R.push(A);
                }
            }
            if (D.push(O), R.length) {
                const e = b && !L && b.ye.length === R.length && b.Oe === y ? b : {
                    ze: l,
                    ye: R,
                    we: Y(l, E, y, "top", 3),
                    Oe: y
                };
                N.set(l, e), F.push(e);
            }
        }
        return L.set(e, N), {
            Me: V(e, O, D, m(X), "max-content", a),
            Ee: V(t, M, j, m(J), "minmax(max-content,auto)", g),
            Ce: U
        };
    })(le, ie, re.l(x), se.l(x), he, $, y, k), be = ne(e => ((e, t, o, n, r, s = u, {rowIndex: l, colIndex: i, rowAlign: c, colAlign: f}) => {
        null != l && N(e, t, n, s, l, c, !1), null != i && N(e, o, r, u, i, f, !0);
    })(ce, re, se, le, ie, $, e));
    return Q(() => {
        const e = e => {
            e ? f(we) : we();
        };
        re.L(1, e), se.L(1, e);
        let t = !1;
        re.L(4, () => {
            t = !0, F.current && F.current(re.k());
        }), se.L(4, () => {
            t = !0, K.current && K.current(se.k());
        });
        const o = () => {
            !t || re.O() || se.O() || (t = !1, ee.current && ee.current());
        };
        let n;
        re.L(8, o), se.L(8, o);
        const r = () => {
            n || (n = !0, _(() => {
                n = !1, te.current && te.current();
            }));
        };
        return re.L(2, r), se.L(2, r), ce.K(U[Z]), () => {
            re._(), se._(), ce._();
        };
    }, []), Q(() => {
        ce.ee();
    }, [ me ]), n(D, () => ({
        get verticalScrollOffset() {
            return re.k();
        },
        get horizontalScrollOffset() {
            return se.k();
        },
        get scrollHeight() {
            return I(re);
        },
        get scrollWidth() {
            return I(se);
        },
        get viewportHeight() {
            return re.o();
        },
        get viewportWidth() {
            return se.o();
        },
        findRowIndex: le.W,
        findColIndex: ie.W,
        getRowOffset: re.i,
        getColOffset: se.i,
        getRowSize: re.u,
        getColSize: se.u,
        scrollToIndex: e => be[Z](e),
        scrollTo: ({vertical: e, horizontal: t}) => ((e, t, o) => {
            null != t && e.Z(!1, () => t), null != o && e.Z(!0, () => o);
        })(ce, e, t),
        scrollBy: ({vertical: e, horizontal: t}) => ((e, t, o, n, r) => {
            if (null != n) {
                const o = n + t.k();
                e.Z(!1, () => o);
            }
            if (null != r) {
                const t = r + o.k();
                e.Z(!0, () => t);
            }
        })(ce, re, se, e, t)
    }), []), /* @__PURE__ */ c("div", {
        role: "table",
        "aria-rowcount": ze,
        "aria-colcount": ye,
        ...j,
        style: {
            overflow: "auto",
            contain: "strict",
            width: "100%",
            height: "100%",
            ...J
        },
        children: /* @__PURE__ */ c("div", {
            ref: U,
            style: {
                contain: "size style",
                overflowAnchor: "none",
                flex: "none",
                display: "grid",
                gridTemplateRows: xe.Me,
                gridTemplateColumns: xe.Ee,
                gap: b,
                marginTop: _e,
                marginInlineStart: ve,
                height: I(re) - _e,
                pointerEvents: Se ? "none" : void 0
            },
            children: xe.Ce.map(n => "be" in n ? /* @__PURE__ */ c(ue, {
                ae: n,
                oe: e,
                Ie: t,
                xe: o,
                ne: ce.N
            }, n.Re) : /* @__PURE__ */ c(ae, {
                ae: n,
                oe: e,
                Se: E(t, n.ze),
                xe: o,
                ne: ce.N
            }, n.ze))
        })
    });
}), he = (e, t) => t ? `calc(${100 * e}% + ${t}px)` : 100 * e + "%", ge = /*#__PURE__*/ t(({oe: e, ne: t, re: o, se: n, Le: r, Ae: l, le: f}) => {
    const a = i(null);
    Q(() => t(a[Z], o), [ o ]);
    const u = s(() => ({
        contain: "layout style",
        position: "absolute",
        top: n,
        insetInlineStart: r,
        width: l,
        visibility: f ? "hidden" : void 0
    }), [ n, r, l, f ]);
    /* @__PURE__ */
    return c("div", {
        ref: a,
        style: u,
        children: e
    });
}), $e = /*#__PURE__*/ e(({children: e, data: t, lanes: o, gap: r, itemSize: s, bufferSize: a, keepMounted: u, cache: g, onScroll: $, onScrollEnd: p, onResize: w, style: S, ..._}, v) => {
    "use no memo";
    const z = i(null), y = ne($), x = ne(p), b = ne(w), [O, M, E] = oe(() => {
        const e = ((e, t, o = 0, n, r) => {
            let s = h(t, 1), l = r && r[1] || n || 40, i = -1, c = n ? void 0 : /* @__PURE__ */ new Set, f = !n;
            const a = r && r[0], u = a ? k(a.slice(0, d(e, a.length)), h(0, e - a.length)) : k([], e), g = [], $ = [], p = [], w = e => {
                const t = u[e];
                return -1 === t ? l : t;
            }, S = t => {
                if (t = d(t, e - 1), i >= t) return;
                const n = [], r = [];
                for (let e = 0; e < s; e++) n.push(0), r.push(-1);
                for (let e = i, t = s; e >= 0 && t; e--) {
                    const o = $[e];
                    -1 === r[o] && (r[o] = e, n[o] = g[e] + w(e), t--);
                }
                for (;i < t; ) {
                    const e = ++i;
                    let t = 0, l = 1 / 0, c = 1 / 0;
                    for (let e = 0; e < s; e++) {
                        const s = n[e] + (r[e] >= 0 ? o : 0);
                        (s < l || s === l && r[e] < c) && (t = e, l = s, c = r[e]);
                    }
                    g[e] = l, $[e] = t, n[t] = l + w(e), r[t] = e, p[e] = h(e ? p[e - 1] : 0, n[t]);
                }
            }, _ = t => t >= e ? z() : (S(t), g[t]), v = e => (S(e), p[e]), z = () => e ? v(e - 1) : 0, y = t => {
                if (!e) return 0;
                const o = T(v, e, t);
                return v(o) <= t ? o + 1 : o;
            };
            return {
                l: (t, o) => {
                    const n = T(_, e, o);
                    return [ d(y(t), n), n ];
                },
                i: _,
                u: w,
                h: (e, t = -1) => u[e] === t,
                S: (t, n, r, a) => {
                    let h = y(r);
                    for (;h > 0 && !n(h - 1); ) h--;
                    for (;h < e && n(h); ) h++;
                    const g = _(h);
                    for (const [e, o] of t) u[e] = o, c && c.add(e), i = d(e - 1, i);
                    let $ = _(h) - g;
                    if (c && a) {
                        const e = [];
                        let t = 0;
                        if (c.forEach(n => {
                            const r = u[n];
                            r > 0 && (e.push(r), t += r + o);
                        }), t > a * s) {
                            m(e);
                            const t = e.length, o = t / 2 | 0, n = t % 2 == 0 ? (e[o - 1] + e[o]) / 2 : e[o], s = y(r + $), a = _(s);
                            l = n, i = -1, $ += _(s) - a, c = void 0, f = !1;
                        }
                    }
                    return $;
                },
                t: z,
                $: () => e,
                p: t => {
                    const o = t - e;
                    return i = d(t - 1, i), e = t, o > 0 ? k(u, o) : u.splice(o), 0;
                },
                X: (e, t = 0, r) => {
                    if ((e = h(e, 1)) === s && t === o) return;
                    const l = y(r), f = _(l);
                    return s = e, o = t, i = -1, c = n ? void 0 : /* @__PURE__ */ new Set, _(l) - f;
                },
                m: () => f,
                H: () => [ u.slice(), l ],
                We: () => s,
                He: () => o,
                qe: e => (S(e), $[e])
            };
        })(t.length, o, r, s, g), n = R(e);
        return [ n, e, j(n, !1) ];
    });
    t.length !== O.R() && O.A(5, [ t.length ]), O.A(9, M.X(o, r, O.T()));
    const [C, L] = l(O.v, void 0, O.v), A = O.O(), W = O.t();
    Q(() => (O.L(1, e => {
        e ? f(L) : L();
    }), O.L(4, () => {
        y.current && y.current(O.k());
    }), O.L(8, () => {
        x.current && x.current();
    }), O.L(2, () => {
        b.current && b.current();
    }), E.K(z[Z]), () => {
        O._(), E._();
    }), []), Q(() => {
        E.ee();
    }, [ C ]), n(v, () => ({
        get cache() {
            return M.H();
        },
        get scrollOffset() {
            return O.k();
        },
        get scrollSize() {
            return I(O);
        },
        get viewportSize() {
            return O.o();
        },
        getItemOffset: O.i,
        getItemSize: O.u,
        scrollToIndex: (e, t) => K(E, O, e, t),
        scrollTo: e => U(E, e),
        scrollBy: e => F(E, O, e)
    }), []);
    const H = [], q = M.We(), B = M.He(), P = he(1 / q, B / q - B), V = o => {
        const n = e(t[o], o), r = M.qe(o);
        /* @__PURE__ */
        return c(ge, {
            ne: E.N,
            re: o,
            se: O.i(o),
            Le: he(r / q, r * B / q),
            Ae: P,
            le: O.I(o),
            oe: n
        }, te(n, o));
    };
    if (u) {
        const e = new Set(u);
        for (let [t, o] = O.l(a); t <= o; t++) e.add(t);
        m([ ...e ]).forEach(e => {
            e < t.length && H.push(V(e));
        });
    } else for (let [e, t] = O.l(a); e <= t; e++) H.push(V(e));
    /* @__PURE__ */
    return c("div", {
        ..._,
        style: {
            display: "block",
            overflowY: "auto",
            contain: "strict",
            width: "100%",
            height: "100%",
            ...S
        },
        children: /* @__PURE__ */ c("div", {
            ref: z,
            style: {
                contain: "size style",
                overflowAnchor: "none",
                flex: "none",
                position: "relative",
                width: "100%",
                height: W,
                pointerEvents: A ? "none" : void 0
            },
            children: H
        })
    });
});

export { de as VGrid, ie as VList, $e as VMasonry, le as Virtualizer, ce as WindowVirtualizer };
