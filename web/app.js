// opt-chat's web page: the chat's log, the turn underway as it streams, the
// queue, and an input like Claude Code's. Everything comes from the server
// over one WebSocket; see web.go and hub.go.

import { h, render, Component } from './vendor/preact.js';
import { useState, useEffect, useRef, useLayoutEffect, useMemo } from './vendor/hooks.js';
import htm from './vendor/htm.js';
import { marked } from './vendor/marked.js';
import DOMPurify from './vendor/purify.js';
import hljs from './vendor/highlight.js';

const html = htm.bind(h);

// ---------------------------------------------------------------- state

const S = {
  conn: 'connecting', // open, closed (reconnecting), denied (no key)
  msgs: [], // the log, contiguous by i, oldest first
  total: 0,
  older: false, // asking for older messages
  turn: { phase: '' },
  turnChars: 0, // characters streamed since the last turn update: tokens are about a fourth
  live: '', // the reply being written
  thinking: '',
  queue: [],
  status: null,
  last: null,
  inputs: [], // the user's past messages, for Up
  notices: [],
  atts: [], // attachments in the input
  paths: [], // files the input names by path
  skipped: new Set(), // of those, the ones not to attach
  chosen: new Set(), // and the hidden ones the user attached
  lightbox: null,
  dragging: false,
  help: false,
  unseen: false, // Opt replied while the tab was in the background
};

// set changes the state and renders it once per frame; in a tab in the
// background, where frames pause, on a timer, so its title still tells
// when Opt is working.
const views = new Set();
let frame = 0;
function set(patch) {
  Object.assign(S, patch);
  if (frame) return;
  const draw = () => { frame = 0; for (const f of views) f(); };
  frame = document.hidden ? setTimeout(draw, 200) : requestAnimationFrame(draw);
}
document.addEventListener('visibilitychange', () => { if (!document.hidden) set({ unseen: false }); });
function useStore() {
  const [, force] = useState(0);
  useEffect(() => {
    const f = () => force(n => n + 1);
    views.add(f);
    return () => views.delete(f);
  }, []);
  return S;
}

// memo renders fn only when a prop changes.
function memo(fn) {
  return class extends Component {
    shouldComponentUpdate(next) {
      for (const k in next) if (next[k] !== this.props[k]) return true;
      for (const k in this.props) if (!(k in next)) return true;
      return false;
    }
    render(props) { return h(fn, props); }
  };
}

// ---------------------------------------------------------------- server

let ws = null, lastI = -1, retries = 0, sendSeq = 0, pathSeq = 0, noticeSeq = 0;
const sending = new Map(); // seq → a send not acknowledged yet: sent again after a reconnect, put back if it fails
const pageKey = crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2) + Date.now();

function connect() {
  ws = new WebSocket(`${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/ws?after=${lastI}`);
  ws.onmessage = e => { try { onUpdate(JSON.parse(e.data)); } catch (err) { console.error(err); } };
  ws.onclose = () => {
    if (S.conn === 'denied') return;
    set({ conn: 'closed' });
    setTimeout(connect, Math.min(5000, 250 * 2 ** retries++));
    if (retries > 1) checkKey();
  };
}

async function checkKey() {
  try {
    const r = await fetch('/', { method: 'HEAD', cache: 'no-store' });
    if (r.status === 401) set({ conn: 'denied' });
  } catch {}
}

function send(o) {
  if (ws && ws.readyState === 1) { ws.send(JSON.stringify(o)); return true; }
  return false;
}

function onUpdate(u) {
  switch (u.t) {
    case 'hello': {
      retries = 0;
      const msgs = u.reset ? u.msgs : S.msgs.concat(u.msgs.filter(m => m.i > lastI));
      if (u.reset) toEnd = true; // pages loaded above are gone: show the end
      lastI = msgs.length ? msgs[msgs.length - 1].i : -1;
      set({
        conn: 'open', msgs, total: u.total, turn: u.turn, turnChars: 0, live: u.turn.text || '', thinking: u.turn.thinking || '',
        queue: u.queue, status: u.status, last: u.last, inputs: mergeInputs(S.inputs, u.inputs), older: false,
      });
      for (const s of sending.values()) send(s.payload); // the connection may have dropped it: the key makes it once
      break;
    }
    case 'msgs': {
      const fresh = u.msgs.filter(m => m.i > lastI);
      if (!fresh.length) break;
      if (fresh[0].i !== lastI + 1) { ws.close(); break; } // a gap: the reconnect catches up
      lastI = fresh[fresh.length - 1].i;
      const patch = { msgs: S.msgs.concat(fresh), total: lastI + 1 };
      if (fresh.some(m => m.kind === 'opt')) { patch.live = ''; if (document.hidden) patch.unseen = true; }
      const typed = fresh.filter(m => m.kind === 'user' && !m.part).map(m => splitMarks(m.text).body).filter(Boolean);
      if (typed.length) patch.inputs = mergeInputs(S.inputs, typed);
      set(patch);
      break;
    }
    case 'delta':
      if (u.k === 'text') set({ live: (u.r ? '' : S.live) + u.s, turnChars: S.turnChars + u.s.length });
      else set({ thinking: (u.r ? '' : S.thinking) + u.s, turnChars: S.turnChars + u.s.length });
      break;
    case 'turn':
      set(u.turn.phase ? { turn: u.turn, turnChars: 0 } : { turn: u.turn, turnChars: 0, live: '', thinking: '' });
      break;
    case 'queue': set({ queue: u.queue }); break;
    case 'thought': set({ thinking: '' }); break;
    case 'status': set({ status: u.status }); break;
    case 'end': set({ last: u.end }); break;
    case 'notice': notice(u.level, u.s); break;
    case 'history': {
      const first = S.msgs.length ? S.msgs[0].i : Infinity;
      set({ msgs: u.msgs.filter(m => m.i < first).concat(S.msgs), older: false });
      break;
    }
    case 'paths': {
      if (u.seq !== pathSeq || pathsDone) break;
      pathsDone = true;
      const off = u.found.filter(p => secret(p.path) && !S.chosen.has(p.path)).map(p => p.path);
      set({ paths: u.found, skipped: new Set([...S.skipped, ...off]) });
      break;
    }
    case 'restore': if (u.note) notice('info', u.note); restore(u.text, u.files); break;
    case 'sent': sending.delete(u.seq); break;
    case 'failed': {
      const s = sending.get(u.seq);
      sending.delete(u.seq);
      notice('warn', u.s);
      if (s) restore(s.text, s.files);
      break;
    }
  }
}

function notice(level, s) {
  set({ notices: S.notices.concat({ id: ++noticeSeq, after: lastI, level, s }).slice(-50) });
}

function mergeInputs(a, b) {
  const out = a.filter(x => !b.includes(x)).concat(b);
  return out.slice(-200);
}

function loadOlder() {
  if (S.older || !S.msgs.length || S.msgs[0].i === 0) return;
  if (send({ t: 'history', before: S.msgs[0].i })) set({ older: true });
}

// ---------------------------------------------------------------- input

let inputEl = null, histPos = -1, histList = [], draft = '', pathTimer = 0, uploadSeq = 0;

function autosize() {
  if (!inputEl) return;
  inputEl.style.height = 'auto';
  inputEl.style.height = inputEl.scrollHeight + 'px';
}

function setInput(text) {
  inputEl.value = text;
  autosize();
  inputEl.focus();
  inputEl.setSelectionRange(text.length, text.length);
  lookForPaths();
}

function restore(text, files) {
  const cur = inputEl.value.trim();
  setInput(text + (cur ? '\n\n' + cur : ''));
  const back = (files || []).map(f => ({ ...f, id: ++uploadSeq, status: 'done', url: kindOf(f) === 'image' ? '/' + f.name : null, promise: Promise.resolve() }));
  set({ atts: back.concat(S.atts) });
}

// secret is a path in a hidden folder or to a hidden file (~/.ssh, ~/.zshrc):
// naming one does not send it, only the chip's + does.
function secret(p) { return /\/\.[^/]/.test(p); }

let pathsDone = true; // S.paths are the input's as it is

function lookForPaths() {
  clearTimeout(pathTimer);
  const text = inputEl.value;
  if (!/(^|[\s'"(\[`]|\]\()(\/|~\/)/.test(text)) { pathSeq++; pathsDone = true; if (S.paths.length) set({ paths: [] }); return; }
  pathsDone = false;
  pathTimer = setTimeout(() => send({ t: 'paths', text, seq: ++pathSeq }), 250);
}

const commands = [
  ['/model', 'show or set the model for turns'],
  ['/effort', 'show or set their effort'],
  ['/agents', 'list the subagents at work'],
  ['/help', 'list the commands'],
];

function suggestions(text) {
  if (!/^\/\w*$/.test(text)) return [];
  return commands.filter(([c]) => c.startsWith(text) && c !== text);
}

let submitting = false; // waiting for uploads: a second Enter sends nothing more

async function submit(now) {
  if (submitting) return;
  if (!inputEl.value.trim() && !S.atts.length) {
    if (now && S.queue.length) send({ t: 'now' });
    return;
  }
  if (S.conn !== 'open') { notice('warn', 'Not connected: your message stays in the input until opt-chat is back.'); return; }
  if (S.atts.some(a => a.status === 'uploading')) {
    submitting = true;
    try { await Promise.all(S.atts.map(a => a.promise)); } finally { submitting = false; }
  }
  if (S.atts.some(a => a.status === 'error')) { notice('warn', 'An attachment failed: take it off (×) and send again.'); return; }
  const text = inputEl.value; // as it is now: typing went on while the uploads finished
  if (!text.trim() && !S.atts.length) return;
  const files = S.atts.map(({ name, orig, kind, size }) => ({ name, orig, kind, size }));
  // Sent before the paths in it were looked for: the server looks.
  const paths = pathsDone ? S.paths.map(p => p.path).filter(p => !S.skipped.has(p)) : null;
  const seq = ++sendSeq;
  const payload = { t: 'send', text, files, paths, now, seq, key: pageKey + ':' + seq };
  if (!send(payload)) return;
  sending.set(seq, { payload, text, files: S.atts });
  if (text.trim() && !text.startsWith('/')) set({ inputs: mergeInputs(S.inputs, [text.trim()]) });
  if (inputEl.value === text) setInput('');
  histPos = -1;
  set({ atts: [], paths: [], skipped: new Set(), chosen: new Set() });
}

function escape() {
  if (S.lightbox) { set({ lightbox: null }); return; }
  if (S.help) set({ help: false });
  if (S.turn.phase) send({ t: 'stop' });
}

function caretFirstLine(el) { return !el.value.slice(0, el.selectionStart).includes('\n'); }
function caretLastLine(el) { return !el.value.slice(el.selectionEnd).includes('\n'); }

let composedAt = -1e9; // Safari sends the Enter that ends a composition after compositionend

function onKey(e) {
  const el = e.target;
  if (e.isComposing || e.keyCode === 229) return;
  if (e.key === 'Enter' && e.timeStamp - composedAt < 100) { e.preventDefault(); return; }
  if (e.key === '?' && !el.value) { e.preventDefault(); set({ help: !S.help }); return; }
  const sugg = suggestions(el.value);
  if (e.key === 'Tab' && sugg.length) {
    e.preventDefault();
    setInput(sugg[0][0] + ' ');
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.altKey) {
    e.preventDefault();
    submit(e.metaKey || e.ctrlKey);
    return;
  }
  if (e.key === 'Escape') { e.preventDefault(); escape(); return; }
  // An entry recalled and left as it is walks on either way; once edited, the
  // arrows move in it first.
  const recalled = histPos >= 0 && el.value === histList[histPos];
  if (e.key === 'ArrowUp' && !e.shiftKey && (recalled || caretFirstLine(el))) {
    if (!el.value && S.queue.length && histPos < 0) { // edit the last queued, as Claude Code does
      e.preventDefault();
      send({ t: 'pull', id: S.queue[S.queue.length - 1].id });
      return;
    }
    if (histPos < 0) { draft = el.value; histList = S.inputs.slice(); } // as it is now: more come in as Opt works
    const next = histPos < 0 ? histList.length - 1 : histPos - 1;
    if (next >= 0 && next < histList.length) {
      e.preventDefault();
      histPos = next;
      setInput(histList[next]);
      el.setSelectionRange(0, 0);
    }
    return;
  }
  if (e.key === 'ArrowDown' && !e.shiftKey && histPos >= 0 && (recalled || caretLastLine(el))) {
    e.preventDefault();
    histPos++;
    if (histPos >= histList.length) { histPos = -1; setInput(draft); } else setInput(histList[histPos]);
  }
}

// ---------------------------------------------------------------- attachments

function kindOf(f) {
  if (f.kind) return f.kind;
  return /\.(png|jpe?g|gif|webp)$/i.test(f.name || f.orig || '') ? 'image' : 'file';
}

function addFiles(list) {
  for (const file of list) upload(file);
}

function upload(file) {
  const id = ++uploadSeq;
  let orig = file.name || 'file';
  if (/^image\.(png|jpe?g|gif|webp)$/i.test(orig)) orig = `pasted-${new Date().toISOString().replace(/[:.]/g, '-')}.${orig.split('.').pop()}`;
  const att = {
    id, orig, size: file.size, kind: file.type.startsWith('image/') ? 'image' : 'file', status: 'uploading', progress: 0,
    url: file.type.startsWith('image/') ? URL.createObjectURL(file) : null,
  };
  const update = patch => set({ atts: S.atts.map(a => a.id === id ? Object.assign(a, patch) : a) });
  const blob = att.url;
  att.promise = new Promise(resolve => {
    const xhr = att.xhr = new XMLHttpRequest();
    xhr.onloadend = () => { if (blob) URL.revokeObjectURL(blob); resolve(); };
    xhr.onabort = () => update({ status: 'error', error: 'taken off' });
    xhr.open('POST', '/upload');
    xhr.setRequestHeader('X-Name', encodeURIComponent(orig));
    xhr.upload.onprogress = e => e.lengthComputable && update({ progress: e.loaded / e.total });
    xhr.onload = () => {
      if (xhr.status === 200) {
        const r = JSON.parse(xhr.responseText);
        update({ status: 'done', name: r.name, kind: r.kind, size: r.size, orig: r.orig, url: r.kind === 'image' ? '/' + r.name : null });
      } else update({ status: 'error', error: xhr.responseText.trim() || `upload failed (${xhr.status})` });
    };
    xhr.onerror = () => update({ status: 'error', error: 'upload failed' });
    xhr.send(file);
  });
  set({ atts: S.atts.concat(att) });
}

// onPaste attaches what ⌘V or Ctrl+V pastes as files (a screenshot copied
// with ⌃⌘⇧4, an image copied from a page, a file copied in Finder), as
// Claude Code does; text goes into the input, as ever.
function onPaste(e) {
  const cd = e.clipboardData;
  if (!cd) return;
  let files = [...cd.files];
  if (!files.length) files = [...cd.items].filter(i => i.kind === 'file').map(i => i.getAsFile()).filter(Boolean);
  // Cells copied from Excel, Numbers or Word come as rich text with a
  // picture of themselves: the text wins. (An image copied from a page or a
  // screenshot has no rich text.)
  const rich = cd.getData('text/plain') && cd.types.includes('text/rtf') && files.every(f => f.type.startsWith('image/'));
  if (files.length && !rich) {
    e.preventDefault();
    addFiles(files);
    inputEl?.focus();
    return;
  }
  if (e.target !== inputEl) { // pasted elsewhere on the page: into the input
    const text = cd.getData('text/plain');
    if (text) { e.preventDefault(); inputEl.focus(); document.execCommand('insertText', false, text); }
  }
}

function onDrop(e) {
  e.preventDefault();
  set({ dragging: false });
  dragDepth = 0;
  const files = [];
  let dirs = 0;
  if (e.dataTransfer.items) {
    for (const it of e.dataTransfer.items) {
      if (it.kind !== 'file') continue;
      if (it.webkitGetAsEntry?.()?.isDirectory) { dirs++; continue; }
      const f = it.getAsFile();
      if (f) files.push(f);
    }
  } else files.push(...e.dataTransfer.files);
  if (dirs) notice('warn', 'A folder can\'t be attached: type its path in the message instead.');
  addFiles(files);
  inputEl?.focus();
}

let dragDepth = 0;
function hasFiles(e) { return [...(e.dataTransfer?.types || [])].includes('Files'); }

// ---------------------------------------------------------------- formatting

marked.use({
  gfm: true,
  breaks: true, // a line break shows, as in Claude Code
  renderer: {
    code({ text, lang }) {
      // Only a named language is highlighted, as Claude Code does: a guess
      // costs much and would change the colours when the reply is done.
      const language = (lang || '').split(/\s/)[0];
      let body;
      try {
        body = language && hljs.getLanguage(language) ? hljs.highlight(text, { language }).value : esc(text);
      } catch { body = esc(text); }
      return `<div class="code"><span class="copy" role="button" tabindex="0">copy</span><pre><code class="hljs">${body}</code></pre></div>`;
    },
  },
});

DOMPurify.addHook('afterSanitizeAttributes', node => {
  if (node.tagName === 'A') { node.setAttribute('target', '_blank'); node.setAttribute('rel', 'noopener noreferrer'); }
});

// What a reply may not hold: anything that could restyle or overlay the
// page, or pass for its controls. (Our own copy control is a span.)
const PURE = {
  FORBID_TAGS: ['style', 'form', 'input', 'button', 'textarea', 'select', 'dialog', 'template'],
  FORBID_ATTR: ['style', 'id'],
  ADD_ATTR: ['role'],
};
const pure = s => DOMPurify.sanitize(s, PURE);

const mdCache = new Map();
function md(text) {
  let out = mdCache.get(text);
  if (out === undefined) {
    out = pure(marked.parse(text));
    if (text.length < 100000) {
      mdCache.set(text, out);
      if (mdCache.size > 2000) mdCache.delete(mdCache.keys().next().value);
    }
  }
  return out;
}

// mdLive renders the reply being written: each block once, when done, so a
// frame costs the block underway, not the whole reply again.
const blockCache = new Map();
function mdLive(text) {
  const tokens = marked.lexer(text);
  let out = '';
  tokens.forEach((t, j) => {
    let h = blockCache.get(t.raw);
    if (h === undefined) {
      const one = [t];
      one.links = tokens.links;
      h = pure(marked.parser(one));
      if (j < tokens.length - 1) blockCache.set(t.raw, h); // the last may still grow
    }
    out += h;
  });
  if (blockCache.size > 500) blockCache.clear();
  return out;
}

function esc(s) { return s.replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]); }
function unesc(s) { return s.replace(/&(amp|lt|gt|quot|#34|#39);/g, (_, e) => ({ amp: '&', lt: '<', gt: '>', quot: '"', '#34': '"', '#39': '\'' })[e]); }

const MARK = /\n*<attachment name="([^"]*)" path="([^"]*)" size="([^"]*)"(?:\/>|>\n([\s\S]*?)\n<\/attachment>)/g;
function splitMarks(text) {
  const marks = [];
  const body = text.replace(MARK, (_, name, path, size, content) => {
    marks.push({ name: unesc(name), path: unesc(path), size, content });
    return '';
  });
  return { body: body.trim(), marks };
}

function fileURL(path) {
  const base = path.split('/').pop();
  return /^[0-9a-f]{32}(\.[a-z0-9]+)?$/.test(base) ? '/files/' + base : null;
}

// count is "1 line", "2 lines": n with its noun.
function count(n, noun) { return `${(n || 0).toLocaleString()} ${noun}${n === 1 ? '' : 's'}`; }

function icon(kind) { return { pdf: '📕', text: '📄', image: '🖼' }[kind] || '📎'; }

function size(n) {
  if (n < 1000) return n + ' B';
  if (n < 1e6) return (n / 1e3).toFixed(1) + ' KB';
  if (n < 1e9) return (n / 1e6).toFixed(1) + ' MB';
  return (n / 1e9).toFixed(1) + ' GB';
}

function k(n) { return n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n || 0); }

// tilde shortens a path: relative to where Opt's tools run, as Claude Code
// shows it, or else from ~.
function tilde(p) {
  const cwd = S.status?.cwd, home = S.status?.dir?.match(/^\/(Users|home)\/[^/]+/)?.[0];
  if (cwd && cwd !== home && p?.startsWith(cwd + '/')) return p.slice(cwd.length + 1);
  return home && p?.startsWith(home + '/') ? '~' + p.slice(home.length) : p;
}

function when(d) {
  const t = new Date(d);
  return isNaN(t) ? '' : t.toLocaleString(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

function day(d) {
  const t = new Date(d);
  return isNaN(t) ? '' : t.toLocaleDateString(undefined, { weekday: 'short', year: 'numeric', month: 'short', day: 'numeric' });
}

function elapsed(ms) {
  const s = Math.max(0, Math.floor(ms / 1000));
  return s < 60 ? s + 's' : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
}

// ---------------------------------------------------------------- the log

// items turns the log into what the page shows: a message split for length
// back in one piece, each tool call with its result, and a line where the
// day changes.
function items(msgs) {
  const joined = [];
  for (const m of msgs) {
    const p = joined[joined.length - 1];
    // part marks the parts after the first; logs from before it had none
    if (p && p.kind === m.kind && p.last + 1 === m.i && (m.part > 0 || (p.date === m.date && p.text.length >= 25000))) {
      joined[joined.length - 1] = { ...p, text: p.text + m.text, images: (p.images || []).concat(m.images || []), last: m.i };
      continue;
    }
    joined.push(m.last === undefined ? Object.assign(m, { last: m.i }) : m);
  }
  const byRef = new Map(), open = [], echoOf = new Map(), paired = new Set();
  for (const m of joined) {
    if (m.kind === 'tool') {
      if (m.ref) byRef.set(m.ref, m); else open.push(m);
    } else if (m.kind === 'opt') {
      open.length = 0; // unanswered calls before a reply were cut short
    } else if (m.kind === 'echo') {
      const t = m.ref ? byRef.get(m.ref) : open.shift();
      if (t && !echoOf.has(t.i)) { echoOf.set(t.i, m); paired.add(m.i); }
    }
  }
  const out = [];
  let lastDay = '';
  for (const m of joined) {
    if (paired.has(m.i)) continue;
    const d = day(m.date);
    if (d && d !== lastDay) { out.push({ sep: d, key: 'd' + m.i, i: m.i }); lastDay = d; }
    out.push({ m, echo: echoOf.get(m.i), key: m.i });
  }
  return out;
}

// Log is the chat. A tool call with no result is running when the turn
// underway made it (from on), else it was cut short.
const Log = memo(function Log({ msgs, phase, from, notices }) {
  const list = useMemo(() => items(msgs), [msgs]);
  const notes = useMemo(() => notices.slice().sort((a, b) => a.after - b.after), [notices]);
  const out = [];
  let j = 0;
  const flush = before => { // a notice shows after the message it followed, wherever that one shows
    for (; j < notes.length && notes[j].after < before; j++) out.push(html`<${Notice} key=${'n' + notes[j].id} n=${notes[j]}/>`);
  };
  for (const it of list) {
    flush(it.sep ? it.i : it.m.i);
    if (it.sep) { out.push(html`<div class="sep" key=${it.key}><span class="lbl">${it.sep}</span></div>`); continue; }
    const state = it.m.kind !== 'tool' ? '' : it.echo ? 'done' : phase && it.m.i >= from ? 'run' : 'cut';
    out.push(html`<${MsgView} key=${it.key} m=${it.m} echo=${it.echo} state=${state}/>`);
  }
  flush(Infinity);
  return out;
});

function Notice({ n }) {
  return html`<div class=${'notice ' + n.level}><span>${n.level === 'warn' ? '!' : '·'}</span><span>${n.s}</span></div>`;
}

const MsgView = memo(({ m, echo, state }) => {
  switch (m.kind) {
    case 'user': return html`<${UserMsg} m=${m}/>`;
    case 'opt': {
      const cut = m.text.endsWith(' [interrupted]'); // a reply stopped midway, as Turn.Run logs it
      return html`<div class="msg opt" title=${when(m.date)}><span class="mark">●</span><div class="body"><${Markdown} text=${cut ? m.text.slice(0, -14) : m.text}/>
        ${cut && html`<div class="result"><span>  ⎿</span><span class="cut-note">Interrupted</span></div>`}</div></div>`;
    }
    case 'tool': return html`<${ToolMsg} m=${m} echo=${echo} state=${state}/>`;
    case 'echo': return html`<${ToolMsg} echo=${m} state="done"/>`;
    case 'work': return html`<${WorkMsg} m=${m}/>`;
    default: return html`<${NoteMsg} m=${m}/>`;
  }
});

function Markdown({ text, live }) {
  return html`<div class="md" dangerouslySetInnerHTML=${{ __html: live ? mdLive(text) : md(text) }}/>`;
}

function UserMsg({ m }) {
  const { body, marks } = useMemo(() => splitMarks(m.text), [m.text]);
  const [open, setOpen] = useState(false);
  const lines = body.split('\n');
  const long = lines.length > 20; // a big paste shows folded, as in Claude Code
  return html`<div class="msg user" title=${when(m.date)}>
    <span class="mark">></span>
    <div class="body">
      ${body && html`<div class="text">${long && !open ? lines.slice(0, 12).join('\n') : body}</div>`}
      ${long && html`<button class="more" onClick=${() => setOpen(!open)}>${open ? '▴ fold' : `… +${lines.length - 12} lines (click to expand)`}</button>`}
      <${Attached} marks=${marks} images=${m.images}/>
    </div>
  </div>`;
}

// Attached shows a message's attachments: images as thumbnails, other
// files as chips; a text file's chip opens its text.
function Attached({ marks = [], images = [] }) {
  const [open, setOpen] = useState(-1);
  if (!marks.length && !images.length) return null;
  const shown = new Set();
  const parts = marks.map((mk, j) => {
    const url = fileURL(mk.path);
    const base = mk.path.split('/').pop();
    if (url && images.some(n => n.endsWith('/' + base)) && /\.(png|jpe?g|gif|webp)$/.test(base)) {
      shown.add(base);
      return html`<img class="thumb" src=${url} alt=${mk.name} title=${mk.name + ' · ' + mk.size} loading="lazy" role="button" tabindex="0" onClick=${() => set({ lightbox: url })}/>`;
    }
    if (url && images.some(n => n.endsWith('/' + base))) shown.add(base);
    if (mk.content !== undefined) {
      return html`<button class="chip" title=${mk.path} onClick=${() => setOpen(open === j ? -1 : j)}>📄 <span class="name">${mk.name}</span> <span class="size">${mk.size}</span> ${open === j ? '▴' : '▾'}</button>`;
    }
    return html`<a class="chip" href=${url || undefined} target="_blank" rel="noopener" title=${mk.path}>${/\.pdf$/i.test(mk.name) ? '📕' : '📎'} <span class="name">${mk.name}</span> <span class="size">${mk.size}</span></a>`;
  });
  for (const n of images) {
    const base = n.split('/').pop();
    if (shown.has(base)) continue;
    const url = '/files/' + base;
    parts.push(/\.pdf$/.test(base)
      ? html`<a class="chip" href=${url} target="_blank" rel="noopener">📕 <span class="name">${base}</span></a>`
      : html`<img class="thumb" src=${url} alt="" loading="lazy" role="button" tabindex="0" onClick=${() => set({ lightbox: url })}/>`);
  }
  return html`<div class="atts">${parts}</div>
    ${open >= 0 && marks[open] && html`<div class="attach-text"><pre>${marks[open].content}</pre></div>`}`;
}

function WorkMsg({ m }) {
  const match = m.text.match(/^\[([^\]]+)\]\s*/);
  const who = match ? match[1] : 'subagent';
  const text = match ? m.text.slice(match[0].length) : m.text;
  return html`<div class="msg work" title=${when(m.date)}><span class="mark">◆</span><div class="body">
    <div class="who">[${who}] report</div><${Markdown} text=${text}/></div></div>`;
}

function NoteMsg({ m }) {
  if (!m.text.includes('\n') && / \(imported\)$/.test(m.text)) {
    return html`<div class="sep" title=${m.text}><span class="lbl">${m.text.replace(/ \(imported\)$/, '')} · imported</span></div>`;
  }
  return html`<div class="msg note" title=${when(m.date)}><span class="mark">※</span><div class="body"><div class="text">${m.text}</div>
    <${Attached} images=${m.images}/></div></div>`;
}

// ---------------------------------------------------------------- tool calls

function parseTool(text) {
  const sp = text.indexOf(' ');
  const name = sp < 0 ? text : text.slice(0, sp);
  const raw = sp < 0 ? '' : text.slice(sp + 1);
  let input = null;
  try { input = JSON.parse(raw); } catch {}
  return { name, input, raw };
}

function oneLine(s, n) {
  s = String(s ?? '').replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n) + '…' : s;
}

function toolArgs(name, x, raw) {
  if (!x || typeof x !== 'object') return oneLine(raw, 140);
  switch (name) {
    case 'Bash': return oneLine(x.command, 140);
    case 'Read': case 'Write': case 'Edit': case 'MultiEdit': case 'NotebookEdit':
      return tilde(x.file_path || x.notebook_path || '') + (x.offset ? `:${x.offset}` : '');
    case 'Glob': case 'Grep': return oneLine(x.pattern, 100) + (x.path ? ` in ${tilde(x.path)}` : '');
    case 'WebSearch': return oneLine(x.query, 140);
    case 'WebFetch': return oneLine(x.url, 140);
    case 'zoom': return typeof x.id === 'string' ? `"${x.id}"` : `${x.id}${x.n ? '+' + x.n : ''}`;
    case 'date': return String(x.id);
    case 'spawn': case 'tell': return `[${x.name}] ${oneLine(x.task || x.message, 100)}`;
    default: return oneLine(JSON.stringify(x), 140);
  }
}

function ToolInput({ name, x, raw }) {
  if (!x) return html`<pre>${raw}</pre>`;
  if (name === 'Bash') return html`<pre>${x.command}</pre>`;
  if (name === 'Edit' && typeof x.old_string === 'string') {
    return html`<pre class="diff">${x.old_string.split('\n').map(l => html`<div class="del">- ${l}</div>`)}${x.new_string.split('\n').map(l => html`<div class="add">+ ${l}</div>`)}</pre>`;
  }
  if (name === 'Write' && typeof x.content === 'string') return html`<pre>${x.content}</pre>`;
  return html`<pre>${JSON.stringify(x, null, 2)}</pre>`;
}

// brief is the one line Claude Code shows for a tool whose output says
// little at a glance: a file read, written or edited.
function brief(name, x, lines, echo) {
  switch (name) {
    case 'Read': return echo.images?.length ? 'Read an image' : `Read ${count(lines.length, 'line')}`;
    case 'Write': return typeof x?.content === 'string' ? `Wrote ${count(x.content.replace(/\n$/, '').split('\n').length, 'line')}` : '';
    case 'Edit': {
      if (typeof x?.old_string !== 'string' || typeof x.new_string !== 'string') return '';
      const [add, del] = changed(x.old_string, x.new_string);
      return `Updated with ${count(add, 'addition')} and ${count(del, 'removal')}${x.replace_all ? ', everywhere' : ''}`;
    }
  }
  return '';
}

// changed counts the lines added and removed between a and b, past the
// lines they start and end with alike.
function changed(a, b) {
  const x = a.split('\n'), y = b.split('\n');
  let p = 0, q = 0;
  while (p < x.length && p < y.length && x[p] === y[p]) p++;
  while (q < x.length - p && q < y.length - p && x[x.length - 1 - q] === y[y.length - 1 - q]) q++;
  return [y.length - p - q, x.length - p - q];
}

function ToolMsg({ m, echo, state }) {
  const [open, setOpen] = useState(false);
  const tool = m ? parseTool(m.text) : null;
  const out = echo ? echo.text : '';
  const lines = out ? out.replace(/\n+$/, '').split('\n') : [];
  const failed = /^(<tool_use_error>|Error\b|error:)/i.test(out.trim());
  const cls = 'msg tool ' + (state === 'run' ? 'run' : state === 'cut' || failed ? 'cut' : '');
  const toggle = () => { if (!getSelection().toString()) setOpen(!open); }; // not when selecting its text
  const sum = echo && !failed && tool ? brief(tool.name, tool.input, lines, echo) : '';
  return html`<div class=${cls} title=${when((m || echo).date)}>
    <span class="mark">●</span>
    <div class="body">
      ${tool && html`<div class="head" role="button" tabindex="0" aria-expanded=${open} onClick=${toggle}><span class="name">${tool.name}</span><span class="args">(${toolArgs(tool.name, tool.input, tool.raw)})</span></div>`}
      ${echo && !open && sum && html`<div class="result" role="button" tabindex=${tool ? -1 : 0} onClick=${toggle}><span>  ⎿</span><span class="lines">${sum} <span class="more">(click to expand)</span></span></div>`}
      ${echo && !open && !sum && html`<div class="result" role="button" tabindex=${tool ? -1 : 0} onClick=${toggle}><span>  ⎿</span><span class="lines">${lines.slice(0, 3).map(l => oneLine(l, 160)).join('\n') || '(no output)'}${lines.length > 3 && html`<span class="more">${'\n'}… +${lines.length - 3} lines (click to expand)</span>`}</span></div>`}
      ${state === 'cut' && !echo && html`<div class="result"><span>  ⎿</span><span class="cut-note">Interrupted</span></div>`}
      ${open && html`<div class="full">
        ${tool && html`<div class="label">input</div><${ToolInput} name=${tool.name} x=${tool.input} raw=${tool.raw}/>`}
        ${echo && html`<div class="label">output</div><pre onClick=${toggle}>${out || '(no output)'}</pre>`}
      </div>`}
      ${echo && echo.images && echo.images.length > 0 && html`<${Attached} images=${echo.images}/>`}
    </div>
  </div>`;
}

// ---------------------------------------------------------------- the page

const glyphs = ['·', '✢', '✳', '✶', '✻', '✽', '✻', '✶', '✳', '✢'];

function Spinner({ turn, chars, queued }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => tick(n => n + 1), 120);
    return () => clearInterval(t);
  }, []);
  const g = glyphs[Math.floor(Date.now() / 120) % glyphs.length];
  let verb = 'Thinking';
  switch (turn.phase) {
    case 'settling': verb = turn.backlog ? `Waiting for ${turn.backlog} message summar${turn.backlog === 1 ? 'y' : 'ies'}` : 'Getting ready'; break;
    case 'reading': verb = 'Reading memory'; break;
    case 'writing': verb = 'Writing'; break;
    case 'tool': verb = turn.tool ? `Running ${turn.tool}` : 'Calling a tool'; break;
  }
  const meta = [elapsed(Date.now() - (turn.since || Date.now()))];
  const tokens = (turn.tokens || 0) + Math.round(chars / 4);
  if (tokens) meta.push(`↓ ${k(tokens)} tokens`);
  if (turn.phase === 'settling' && turn.written) meta.push(`${count(turn.written, 'line')} written`);
  meta.push(html`<button title="stop the turn" onClick=${() => send({ t: 'stop' })}>esc to interrupt</button>`);
  if (queued || turn.phase === 'settling') meta.push(html`<button title="stop the turn and send the queue now" onClick=${() => send({ t: 'now' })}>⌘⏎ send now</button>`);
  return html`<div class="spinner"><span><span class="glyph">${g}</span>${verb}…</span><span class="meta">(${meta.map((x, j) => html`${j ? ' · ' : ''}${x}`)})</span>
    ${turn.partial > 0 && html`<span class="meta">memory partial: ${count(turn.partial, 'message')} not summarized yet</span>`}</div>`;
}

function Queue({ queue }) {
  const [editing, setEditing] = useState(null);
  if (!queue.length) return null;
  return html`<div class="queue">
    ${queue.map(q => html`<div class="item" key=${q.id}>
      <span>></span>
      ${editing === q.id
        ? html`<textarea rows="2" ref=${el => el && !el.dataset.ready && (el.dataset.ready = '1', el.value = q.text, el.focus())}
            onCompositionEnd=${e => { composedAt = e.timeStamp; }}
            onKeyDown=${e => {
              if (e.isComposing || e.keyCode === 229 || e.timeStamp - composedAt < 100) return;
              if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send({ t: 'edit', id: q.id, text: e.target.value }); setEditing(null); inputEl?.focus(); }
              if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); setEditing(null); inputEl?.focus(); }
            }}/>`
        : html`<span class="text">${q.text || '(attachments)'}${q.files?.length ? html` <span class="size">+ ${q.files.join(', ')}</span>` : ''}</span>`}
      <span class="acts">
        <button title="edit" onClick=${() => setEditing(q.id)}>edit</button>
        <button title="take it out" onClick=${() => send({ t: 'remove', id: q.id })}>remove</button>
        <button class="now" title="stop the turn and send the queue now" onClick=${() => send({ t: 'now' })}>send now</button>
      </span>
    </div>`)}
    <div class="hint">queued: goes to Opt at its next tool call or turn · ↑ to edit · ⌘⏎ send now</div>
  </div>`;
}

function takeOff(a) {
  if (a.xhr && a.xhr.readyState < 4) a.xhr.abort(); // not uploaded for nothing
  set({ atts: S.atts.filter(x => x.id !== a.id) });
  inputEl?.focus();
}

function Chips() {
  const { atts, paths, skipped } = S;
  if (!atts.length && !paths.length) return null;
  const attach = p => { const s = new Set(S.skipped); s.delete(p); set({ skipped: s, chosen: new Set([...S.chosen, p]) }); inputEl?.focus(); };
  const leave = p => { set({ skipped: new Set([...S.skipped, p]) }); inputEl?.focus(); };
  return html`<div class="atts">
    ${atts.map(a => html`<span class=${'chip' + (a.status === 'error' ? ' err' : '')} key=${a.id} title=${a.error || a.orig}>
      ${a.url ? html`<img src=${a.url} alt=""/>` : icon(kindOf(a))}
      <span class="name">${a.orig}</span>
      ${a.status === 'uploading' ? html`<span class="bar"><i style=${{ width: Math.round((a.progress || 0) * 100) + '%' }}/></span>` : html`<span class="size">${a.status === 'error' ? a.error : size(a.size)}</span>`}
      <button class="x" title="remove" onClick=${() => takeOff(a)}>×</button>
    </span>`)}
    ${paths.map(p => skipped.has(p.path)
      ? html`<span class="chip path off" key=${p.path} title=${p.path + ': not attached'}>
          ↳ <span class="name">${p.name}</span><span class="size">not attached</span>
          <button class="x" title="attach it" onClick=${() => attach(p.path)}>+</button>
        </span>`
      : html`<span class="chip path" key=${p.path} title=${'attaches ' + p.path}>
          ↳ <span class="name">${p.name}</span><span class="size">${size(p.size)}</span>
          <button class="x" title="leave it as text" onClick=${() => leave(p.path)}>×</button>
        </span>`)}
  </div>`;
}

function Footer() {
  const s = S.status, l = S.last;
  const right = [];
  if (s) {
    right.push(s.effort ? `${s.model} ${s.effort}` : s.model);
    right.push(count(s.msgs, 'message'), `view ${count(s.viewLines, 'line')}, ${Math.round(s.viewBytes / 1000)} KB`);
    if (s.backlog || s.merges) right.push(html`<span class="busy" title=${count(s.running, 'compaction') + ' running'}>summarizing ${[s.backlog && count(s.backlog, 'message'), s.merges && count(s.merges, 'merge')].filter(Boolean).join(' + ')}</span>`);
    if (s.written) right.push(`${count(s.written, 'line')} written`);
    if (s.failing) right.push(html`<span class="conn bad">${s.failing} failing, retrying</span>`);
    if (s.agents?.length) right.push(`agents: ${s.agents.map(a => '[' + a + ']').join(' ')}`);
  }
  if (l) right.push(`last turn ↓${k(l.turn.output_tokens)}, ${k(l.turn.cache_read_input_tokens + l.view.cache_read_input_tokens)} cached`);
  const conn = { open: ['ok', '● connected'], connecting: ['', '○ connecting'], closed: ['bad', '● reconnecting'], denied: ['bad', '● no key'] }[S.conn];
  return html`<div class="footer"><div class="segs">
    <span class="seg"><button onClick=${() => set({ help: !S.help })}>? for shortcuts</button></span>
    <span class=${'seg conn ' + conn[0]}>${conn[1]}</span>
    ${right.map(x => html`<span class="seg">${x}</span>`)}
  </div></div>`;
}

function Help() {
  const rows = [
    ['Enter', 'send; while Opt works, queue'],
    ['Shift+Enter', 'new line'],
    ['⌘/Ctrl+Enter', 'send now: stop the turn and send'],
    ['Esc', 'stop the turn; queued messages come back here'],
    ['↑ / ↓', 'your earlier messages; ↑ on an empty input edits the last queued'],
    ['drop · paste · 📎', 'attach files; a path you type (/… or ~/…) attaches its file'],
    ['/model /effort', 'switch the model or effort for turns'],
    ['/agents /help', 'subagents at work; the commands'],
  ];
  return html`<div class="help">${rows.map(([a, b]) => html`<kbd>${a}</kbd><span>${b}</span>`)}</div>`;
}

function Input() {
  const ref = useRef();
  const pick = useRef();
  const [, tick] = useState(0);
  useEffect(() => {
    inputEl = ref.current;
    inputEl.focus();
  }, []);
  const sugg = inputEl ? suggestions(inputEl.value) : [];
  return html`
    ${sugg.length > 0 && html`<div class="cmds">${sugg.map(([c, d], j) => html`<div class=${'cmd' + (j ? '' : ' sel')}>${c}  ${d}${j ? '' : '  (tab)'}</div>`)}</div>`}
    <div class="box">
      <${Chips}/>
      <span class="gt">></span>
      <textarea ref=${ref} rows="1" spellcheck="false" aria-label="Message Opt" placeholder=${S.turn.phase ? 'Queue a message for Opt…' : 'Message Opt'}
        onInput=${() => { histPos = -1; autosize(); lookForPaths(); tick(n => n + 1); }}
        onKeyDown=${onKey}
        onCompositionEnd=${e => { composedAt = e.timeStamp; }}
        onPaste=${onPaste}/>
      <span class="tools">
        <button title="attach files" onClick=${() => pick.current.click()}>📎</button>
        <input type="file" multiple hidden ref=${pick} onChange=${e => { addFiles([...e.target.files]); e.target.value = ''; inputEl.focus(); }}/>
      </span>
    </div>`;
}

let toEnd = false; // scroll to the end at the next render

function Scroller() {
  const ref = useRef(), inner = useRef();
  const stick = useRef(true), was = useRef({ height: 0, first: -1 });
  const [newer, setNewer] = useState(false);
  const toBottom = () => { const el = ref.current; el.scrollTop = el.scrollHeight; };
  useLayoutEffect(() => {
    const el = ref.current;
    const first = S.msgs.length ? S.msgs[0].i : -1;
    if (toEnd) { toEnd = false; stick.current = true; setNewer(false); el.scrollTop = el.scrollHeight; }
    else if (was.current.first >= 0 && first >= 0 && first < was.current.first) el.scrollTop += el.scrollHeight - was.current.height; // older messages came in above
    else if (stick.current) el.scrollTop = el.scrollHeight;
    else if (el.scrollHeight > was.current.height + 4) setNewer(true);
    was.current = { height: el.scrollHeight, first };
  });
  useEffect(() => {
    const ro = new ResizeObserver(() => { if (stick.current) toBottom(); was.current.height = ref.current.scrollHeight; });
    ro.observe(inner.current);
    ro.observe(ref.current); // the input growing makes the log shorter
    return () => ro.disconnect();
  }, []);
  const onScroll = () => {
    const el = ref.current;
    stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (stick.current) setNewer(false);
    if (el.scrollTop < 600) loadOlder();
  };
  const first = S.msgs.length ? S.msgs[0].i : 0;
  return html`<div class="scroll" ref=${ref} onScroll=${onScroll}>
    <div class="column log" ref=${inner}>
      <div class="edge">${S.conn === 'connecting' ? 'Connecting…' : first > 0 ? (S.older ? 'Loading older messages…' : '↑ older messages') : S.msgs.length ? 'The beginning of the chat' : 'An empty chat: say something.'}</div>
      <${Log} msgs=${S.msgs} phase=${S.turn.phase} from=${S.turn.from || 0} notices=${S.notices}/>
      ${S.thinking && (S.turn.phase === 'thinking') && html`<div class="thinking"><div>${S.thinking}</div></div>`}
      ${S.live && html`<div class="msg opt live"><span class="mark">●</span><div class="body"><${Markdown} text=${S.live} live=${true}/></div></div>`}
    </div>
    ${newer && html`<div class="newer"><button onClick=${() => { stick.current = true; setNewer(false); toBottom(); }}>↓ new messages</button></div>`}
  </div>`;
}

function App() {
  useStore();
  useEffect(() => {
    document.title = (S.turn.phase ? '✻ ' : S.unseen ? '● ' : '') + 'opt-chat';
  });
  return html`<div class="shell">
    ${S.conn === 'denied' && html`<div class="banner">This page has no key to the chat: open the address opt-chat printed in the terminal.</div>`}
    ${S.conn === 'closed' && html`<div class="banner">Lost opt-chat: reconnecting…</div>`}
    <${Scroller}/>
    <div class="dock"><div class="column">
      ${S.turn.phase && html`<${Spinner} turn=${S.turn} chars=${S.turnChars} queued=${S.queue.length > 0}/>`}
      <${Queue} queue=${S.queue}/>
      <${Input}/>
      ${S.help && html`<${Help}/>`}
      <${Footer}/>
    </div></div>
    ${S.dragging && html`<div class="drop"><div>Drop to attach</div></div>`}
    ${S.lightbox && html`<div class="lightbox" role="dialog" aria-label="image; Esc or a click closes it" onClick=${() => set({ lightbox: null })}><img src=${S.lightbox} alt=""/></div>`}
  </div>`;
}

// ---------------------------------------------------------------- wiring

addEventListener('dragenter', e => { if (hasFiles(e)) { e.preventDefault(); dragDepth++; set({ dragging: true }); } });
addEventListener('dragover', e => { if (hasFiles(e) || e.target.tagName !== 'TEXTAREA') e.preventDefault(); });
addEventListener('dragleave', e => { if (hasFiles(e) && --dragDepth <= 0) { dragDepth = 0; set({ dragging: false }); } });
addEventListener('drop', e => {
  if (hasFiles(e)) { onDrop(e); return; }
  if (e.target.tagName === 'TEXTAREA') return; // text dropped on an input goes in, as ever
  e.preventDefault(); // a link dropped elsewhere would take the tab away from the chat: into the input
  const text = e.dataTransfer.getData('text/plain');
  if (text && inputEl) { inputEl.focus(); document.execCommand('insertText', false, text); }
});
addEventListener('keydown', e => {
  if ((e.key === 'Enter' || e.key === ' ') && e.target.getAttribute?.('role') === 'button' && e.target.tagName !== 'BUTTON') {
    e.preventDefault(); // a card, a thumbnail or a copy, from the keyboard
    e.target.click();
    return;
  }
  if (e.key === 'Escape' && e.target !== inputEl && e.target.tagName !== 'TEXTAREA') { escape(); return; }
  if (e.target === inputEl || e.target.tagName === 'TEXTAREA' || e.target.tagName === 'INPUT') return;
  if (e.metaKey || e.ctrlKey || e.altKey || e.key.length !== 1) return;
  if (getSelection().toString()) return;
  inputEl?.focus(); // type anywhere: it goes to the input
});
addEventListener('paste', e => {
  if (e.target === inputEl || (e.target.tagName === 'TEXTAREA' || e.target.tagName === 'INPUT')) return;
  onPaste(e);
});
document.addEventListener('click', e => {
  const b = e.target.closest('.copy');
  if (!b) return;
  const code = b.parentElement.querySelector('code');
  navigator.clipboard.writeText(code ? code.textContent : '').then(() => { b.textContent = 'copied'; setTimeout(() => (b.textContent = 'copy'), 1200); });
});

render(html`<${App}/>`, document.getElementById('app'));
connect();
