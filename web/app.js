// opt-chat's web page: the chat's log, the turn underway as it streams, the
// queue, and an input like Claude Code's. Everything comes from the server
// over one WebSocket; see web.go and hub.go.

import { h, render, Component } from './vendor/preact.js';
import { useState, useEffect, useRef, useLayoutEffect, useMemo } from './vendor/hooks.js';
import htm from './vendor/htm.js';
import { marked } from './vendor/marked.js';
import DOMPurify from './vendor/purify.js';
import hljs from './vendor/highlight.js';
import { Virtualizer } from './vendor/virtua.js';

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
  input: '', // the tool call being written: its input, partial JSON
  thinking: '',
  queue: [],
  status: null,
  last: null,
  inputs: [], // the user's past messages, for Up
  notices: [],
  asides: [], // side questions (/btw), each after the message it followed
  atts: [], // attachments in the input
  voice: null, // a voice message underway: starting, rec or sending
  paths: [], // files the input names by path
  skipped: new Set(), // of those, the ones not to attach
  chosen: new Set(), // and the hidden ones the user attached
  lightbox: null,
  dragging: false,
  help: false,
  unseen: false, // Opt replied while the tab was in the background
  asks: [], // tool calls waiting for the user's yes or no
  askSel: 0,
  panel: null, // /mcp, /skills or /plugins, open: { kind, sel, busy, error, signIn, name }
  mcp: null, // the user's MCP servers, once asked
  skills: null, // and skills, asked as the page opens: the / menu offers them
  cmdSel: 0, // the / menu's chosen line
  cmdShut: null, // the input the / menu was shut on (Esc)
  plugins: null, // and plugins
  mention: null, // the @ menu, open: { start, query, items, sel }
  picker: null, // the model picker, open: { sel, effort }
  models: null, // Claude Code's, once asked
  modelsError: '',
  wide: false, // room for the agents rail beside the chat
  railOff: (() => { try { const v = JSON.parse(localStorage.getItem('railOff')); return Array.isArray(v) ? v : null; } catch { return null; } })(), // hidden by the user: the subagents it had then
  drawer: false, // no room: the rail as a drawer, open
  agent: null, // the subagent the rail shows: { name, steps, off, cut, loading }
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

// useKept is useState for what a message shows open, kept by key outside
// it: the log unmounts a message scrolled far away, and one scrolled back
// shows as it was left.
const kept = new Map();
function useKept(key, init) {
  const [, force] = useState(0);
  const v = kept.has(key) ? kept.get(key) : init;
  return [v, x => {
    if (x === init) kept.delete(key); else kept.set(key, x);
    force(n => n + 1);
  }];
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
      const msgs = trim(u.reset ? u.msgs : S.msgs.concat(u.msgs.filter(m => m.i > lastI)));
      if (u.reset) toEnd = true; // pages loaded above are gone: show the end
      lastI = msgs.length ? msgs[msgs.length - 1].i : -1;
      set({
        conn: 'open', msgs, total: u.total, turn: u.turn, turnChars: 0, live: u.turn.text || '', thinking: u.turn.thinking || '', input: u.turn.input || '',
        queue: u.queue, asks: u.asks || [], status: u.status, last: u.last, inputs: mergeInputs(S.inputs, u.inputs), older: false,
        asides: mergeAsides(u.reset ? [] : S.asides, u.asides || []),
      });
      for (const s of sending.values()) send(s.payload); // the connection may have dropped it: the key makes it once
      if (S.panel) { set({ panel: { ...S.panel, busy: true, acting: false } }); send({ t: S.panel.kind }); } // a reply lost with the connection
      if (S.picker && (!S.models || S.modelsError)) send({ t: 'models' });
      if (S.panel?.kind !== 'skills') send({ t: 'skills' }); // for the / menu
      if (S.agent) openAgent(S.agent.name); // its steps meanwhile
      send({ t: 'usage' }); // the plan's limits, for the bar
      break;
    }
    case 'msgs': {
      const fresh = u.msgs.filter(m => m.i > lastI);
      if (!fresh.length) break;
      if (fresh[0].i !== lastI + 1) { ws.close(); break; } // a gap: the reconnect catches up
      lastI = fresh[fresh.length - 1].i;
      const patch = { msgs: trim(S.msgs.concat(fresh)), total: lastI + 1 };
      if (fresh.some(m => m.kind === 'opt')) { patch.live = ''; if (document.hidden) patch.unseen = true; }
      if (fresh.some(m => m.kind === 'tool')) patch.input = '';
      const typed = fresh.filter(m => m.kind === 'user' && !m.part).map(m => histText(m.text)).filter(Boolean);
      if (typed.length) patch.inputs = mergeInputs(S.inputs, typed);
      set(patch);
      break;
    }
    case 'delta':
      if (u.k === 'text') set({ live: (u.r ? '' : S.live) + u.s, turnChars: S.turnChars + u.s.length });
      else if (u.k === 'input') set({ input: (u.r ? '' : S.input) + (u.s || ''), turnChars: S.turnChars + (u.s || '').length });
      else set({ thinking: (u.r ? '' : S.thinking) + u.s, turnChars: S.turnChars + u.s.length });
      break;
    case 'turn':
      set(u.turn.phase ? { turn: u.turn, turnChars: 0 } : { turn: u.turn, turnChars: 0, live: '', thinking: '', input: '' });
      break;
    case 'queue': set({ queue: u.queue }); break;
    case 'asks': {
      const fresh = u.asks.some(q => !S.asks.some(x => x.id === q.id));
      set({ asks: u.asks, askSel: u.asks[0]?.id === S.asks[0]?.id ? S.askSel : 0, unseen: S.unseen || (fresh && document.hidden) }); // a question: news too
      break;
    }
    case 'thought': set({ thinking: '' }); break;
    case 'status': set({ status: u.status }); break;
    case 'steps': if (S.agent?.name === u.name) set({ agent: { name: u.name, steps: u.steps.slice(-agentStepsMax), off: 0, cut: u.cut || u.steps.length > agentStepsMax, loading: false } }); break;
    case 'step': addStep(u.name, u.step); break;
    case 'end': set({ last: u.end }); send({ t: 'usage' }); break;
    case 'notice': notice(u.level, u.s); break;
    case 'aside': set({ asides: mergeAsides(S.asides, [u.aside]), unseen: S.unseen || (!!u.aside.a && document.hidden) }); break;
    case 'history': { // asked before a reconnect's hello reset the log: not the page before it now
      const fits = u.msgs.length && u.msgs[u.msgs.length - 1].i + 1 === S.msgs[0]?.i;
      set({ msgs: fits ? u.msgs.concat(S.msgs) : S.msgs, asides: fits ? mergeAsides(u.asides || [], S.asides) : S.asides, older: false });
      break;
    }
    case 'paths': {
      if (u.seq !== pathSeq || pathsDone) break;
      pathsDone = true;
      const off = u.found.filter(p => secret(p.path) && !S.chosen.has(p.path)).map(p => p.path);
      set({ paths: u.found, skipped: new Set([...S.skipped, ...off]) });
      pathsWait?.();
      break;
    }
    case 'models': set({ models: u.models, modelsError: u.error || '' }); break;
    case 'files': if (S.mention && u.seq === mentionSeq) set({ mention: { ...S.mention, items: u.items, sel: 0, stale: false } }); break;
    case 'mcp': case 'skills': case 'plugins': {
      let p = S.panel;
      if (p?.kind === u.t && p.acting && !u.action) break; // a poll's, from before the action: its answer comes
      if (p?.kind === u.t && (u.action || !p.acting)) {
        p = { ...p, busy: false, acting: false, signIn: u.signIn || p.signIn, name: u.signIn ? u.name : p.name };
        if (u.action || u.error) p.error = u.error || ''; // a poll keeps an action's error
      }
      set({ [u.t]: u.t === 'mcp' ? u.servers : u[u.t], panel: p });
      if (u.t === 'plugins' && u.action) send({ t: 'skills' }); // theirs came or went
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

// histText is a message as ↑ recalls it: without its attachments, which
// don't come back with it, nor their placeholders.
function histText(s) {
  return s.replace(MARK, ' ').replace(PLACE, ' ').replace(/[ \t]{2,}/g, ' ').replace(/ +\n/g, '\n').trim();
}

// mergeAsides is a and b, each side question once (by when it was asked,
// b's copy winning: it is newer), in the order they were asked.
function mergeAsides(a, b) {
  const by = new Map(a.map(x => [x.date, x]));
  for (const x of b) by.set(x.date, x);
  return [...by.values()].sort((x, y) => Date.parse(x.date) - Date.parse(y.date));
}

function mergeInputs(a, b) {
  const out = a.filter(x => !b.includes(x)).concat(b);
  return out.slice(-200);
}

// trim keeps a page open for days light: past keepMax messages, while the
// log is at its end, the oldest go, down to keep; scrolling up brings them
// back. It cuts before a message of the user's, so no reply or tool call
// shows without its start.
const keep = 1500, keepMax = 3000;
let atEnd = true; // the log is scrolled to its end
function trim(msgs) {
  if (msgs.length <= keepMax || !atEnd) return msgs;
  const j = msgs.length - keep;
  const at = msgs.findIndex((m, x) => x >= j && m.kind === 'user' && !m.part);
  return msgs.slice(at >= 0 && at < msgs.length - 100 ? at : j);
}

function loadOlder() {
  if (S.older || !S.msgs.length || S.msgs[0].i === 0) return;
  if (send({ t: 'history', before: S.msgs[0].i })) set({ older: true });
}

// ---------------------------------------------------------------- input

let inputEl = null, histPos = -1, histList = [], draft = '', pathTimer = 0, uploadSeq = 0;

// autosize fits the input to its text. Measuring shrinks it for a moment,
// which makes the log taller, and the browser clamps the log's scroll to
// fit: put back, it would sit lines above where it was, a keystroke at a
// time. So the log keeps its place: at the end if it was there.
function autosize() {
  if (!inputEl) return;
  const log = document.querySelector('.scroll');
  const top = log?.scrollTop, atEnd = log && log.scrollHeight - log.scrollTop - log.clientHeight < 2;
  const own = inputEl.scrollTop; // past its max height it scrolls too
  inputEl.style.height = 'auto';
  inputEl.style.height = inputEl.scrollHeight + 'px';
  inputEl.scrollTop = own;
  if (log) log.scrollTop = atEnd ? log.scrollHeight : top;
}

// keepDraft keeps the input's text for a reload; its attachments go.
function keepDraft() {
  try { if (inputEl.value) localStorage.setItem('draft', inputEl.value); else localStorage.removeItem('draft'); } catch {}
}

function setInput(text) {
  inputEl.value = text;
  keepDraft();
  autosize();
  inputEl.focus();
  inputEl.setSelectionRange(text.length, text.length);
  lookForPaths();
}

function restore(text, files) {
  histPos = -1;
  const cur = inputEl.value.trim();
  // Each queued message numbered its placeholders from 1: one taken already
  // is numbered anew, in the text and its attachment both.
  const taken = new Set([...S.atts, ...gone].map(a => a.at).filter(Boolean));
  for (const m of cur.matchAll(PLACE)) taken.add(m[0]);
  let n = placeMax(cur);
  for (const m of text.matchAll(PLACE)) n = Math.max(n, +m[2]);
  const parts = []; // the text, its placeholders at odd places
  let prev = 0;
  for (const m of text.matchAll(PLACE)) { parts.push(text.slice(prev, m.index), m[0]); prev = m.index + m[0].length; }
  parts.push(text.slice(prev));
  const claimed = new Set();
  const back = (files || []).map(f => {
    const a = { ...f, id: ++uploadSeq, status: 'done', url: kindOf(f) === 'image' ? '/' + f.name : null, promise: Promise.resolve() };
    const j = a.at ? parts.findIndex((p, k) => k % 2 === 1 && p === a.at && !claimed.has(k)) : -1;
    if (j < 0) return a;
    claimed.add(j);
    if (taken.has(a.at)) a.at = parts[j] = a.at.replace(/#\d+\]$/, `#${++n}]`);
    taken.add(a.at);
    return a;
  });
  setInput(parts.join('') + (cur ? '\n\n' + cur : ''));
  set({ atts: back.concat(S.atts) });
}

// secret is a path in a hidden folder or to a hidden file (~/.ssh, ~/.zshrc):
// naming one does not send it, only the chip's + does.
function secret(p) { return /\/\.[^/]/.test(p); }

let pathsDone = true; // S.paths are the input's as it is
let pathsWait = null; // a send waiting for them

function lookForPaths(now) {
  clearTimeout(pathTimer);
  const text = inputEl.value;
  const seq = ++pathSeq; // a reply about an earlier text is stale
  if (!/(^|[\s'"(\[<{`@]|\]\()(\/|~\/)/.test(text)) { pathsDone = true; if (S.paths.length) set({ paths: [] }); return; }
  pathsDone = false;
  pathTimer = setTimeout(() => send({ t: 'paths', text, seq }), now ? 0 : 250);
}

// The @ menu, as Claude Code's: "@" and part of a path at the caret lists
// what it completes to; Tab or Enter takes one, a folder to list its own.
let mentionSeq = 0, mentionTimer = 0, mentionOff = ''; // mentionOff: the text up to the @ Esc closed the menu on

function lookForMention() {
  const el = inputEl;
  const before = el.value.slice(0, el.selectionStart);
  const m = el.selectionStart === el.selectionEnd && before.match(/(^|\s)@((?:\\.|[^\s@\\])*)$/); // a space in it escaped, as @ writes it
  const start = m ? before.length - m[2].length - 1 : -1;
  if (!m || before.slice(0, start + 1) === mentionOff) { if (S.mention) set({ mention: null }); return; }
  const query = m[2].replace(/\\(.)/g, '$1');
  if (S.mention?.query === query && S.mention.start === start) return;
  // The items listed are the last query's until this one's come: not taken.
  set({ mention: { start, query, items: S.mention?.items || [], sel: 0, stale: true } });
  clearTimeout(mentionTimer);
  const seq = ++mentionSeq;
  mentionTimer = setTimeout(() => send({ t: 'files', text: query, seq }), 60);
}

function takeMention(item) {
  const el = inputEl, m = S.mention;
  const end = el.selectionStart;
  el.focus();
  el.setSelectionRange(m.start, end);
  const text = '@' + item.path.replace(/([\\ ])/g, '\\$1') + (item.dir ? '' : ' '); // as a terminal writes a path: a space escaped
  if (!document.execCommand('insertText', false, text)) el.setRangeText(text, m.start, end, 'end');
  autosize();
  set({ mention: null });
  lookForPaths();
  if (item.dir) lookForMention(); // on into the folder
}

// mentionKey takes the keys while the @ menu lists something.
function mentionKey(e) {
  const m = S.mention;
  if (!m || !m.items.length || e.target !== inputEl || e.metaKey || e.ctrlKey || e.altKey) return false;
  switch (e.key) {
    case 'ArrowUp': set({ mention: { ...m, sel: (m.sel + m.items.length - 1) % m.items.length } }); break;
    case 'ArrowDown': set({ mention: { ...m, sel: (m.sel + 1) % m.items.length } }); break;
    case 'Tab': case 'Enter': if (e.shiftKey) return false; if (!m.stale) takeMention(m.items[m.sel]); break; // stale: wait for this query's
    case 'Escape': mentionOff = inputEl.value.slice(0, m.start + 1); set({ mention: null }); break;
    default: return false;
  }
  e.preventDefault();
  e.stopPropagation();
  return true;
}

function Mention() {
  const m = S.mention;
  if (!m?.items.length) return null;
  return html`<div class=${'cmds mention' + (m.stale ? ' stale' : '')}>${m.items.map((it, j) => html`<div class=${'cmd' + (j === m.sel ? ' sel' : '') + (it.dir ? ' dir' : '')}
    onMouseDown=${e => { e.preventDefault(); takeMention(it); }}>${it.path}</div>`)}</div>`;
}

const commands = [
  ['/model', 'choose the model and effort for turns (Alt+P)'],
  ['/effort', 'show or set their effort'],
  ['/mode', 'show or set the permission mode (Shift+Tab)'],
  ['/autocompact', 'compact a subagent at rest, after a report, past this context, as 250k (never the chat\'s)'],
  ['/mcp', 'your Claude Code MCP servers: status, on or off, sign in'],
  ['/skills', 'your Claude Code skills: on or off'],
  ['/plugins', 'your Claude Code plugins, their skills and MCP servers at once: on or off'],
  ['/agents', 'list the subagents at work'],
  ['/status', 'Claude Code\'s version and account, this run\'s settings, the view and the plan\'s usage'],
  ['/usage', 'the plan\'s limits, what uses them, and what Opt\'s calls spent: day, week or run'],
  ['/btw', 'a side question: answered beside the chat, which never sees it'],
  ['/help', 'list the commands'],
];

// The / menu: the commands and the user's skills (Claude Code runs a
// skill's /name sent to Opt) that the input starts, a plugin's skills by
// their own name too; none once it names one alone, or was shut. A skill
// completes mid-text too, as in Claude Code: "/" and part of its name after
// a space, at the caret.
function slashWord(el) {
  const text = el.value, at = el.selectionStart;
  if (/^\/[\w:.-]*$/.test(text)) return { start: 0, word: text, cmds: true };
  const m = el.selectionStart === el.selectionEnd && text.slice(0, at).match(/\s(\/[\w:.-]+)$/);
  if (!m || /^[\w:.\/-]/.test(text.slice(at))) return null; // the caret in a word, or a path
  return { start: at - m[1].length, word: m[1], cmds: false };
}

function suggestions(el) {
  const w = slashWord(el);
  if (!w || el.value === S.cmdShut) return [];
  const t = w.word.toLowerCase();
  const skills = (S.skills || []).filter(s => s.state !== 'off' && !commands.some(([c]) => c === '/' + s.name))
    .map(s => ['/' + s.name, s.description]).sort(([a], [b]) => a.localeCompare(b));
  const all = (w.cmds ? commands : []).concat(skills).filter(([c]) => c.toLowerCase().startsWith(t) || c.toLowerCase().includes(':' + t.slice(1)));
  const exact = all.findIndex(([c]) => c === w.word);
  if (exact >= 0) all.unshift(...all.splice(exact, 1)); // what it names, first: Enter runs it
  return all.length === 1 && exact >= 0 ? [] : all;
}

function isCommand(text) { // one of Opt's own, not a skill's
  const w = text.match(/^\/(\S*)/)?.[1].toLowerCase();
  return w !== undefined && commands.some(([c]) => c === '/' + w);
}

function takeCmd(c, run) {
  const w = slashWord(inputEl);
  if (w && !w.cmds) { // mid-text: the name goes in, nothing is sent
    const text = c + (/^\s/.test(inputEl.value.slice(inputEl.selectionStart)) ? '' : ' ');
    inputEl.setSelectionRange(w.start, inputEl.selectionStart);
    if (!document.execCommand('insertText', false, text)) inputEl.setRangeText(text, w.start, inputEl.selectionEnd, 'end');
    autosize();
    set({ cmdSel: 0 });
    return;
  }
  setInput(run ? c : c + ' ');
  set({ cmdSel: 0 });
  if (run) submit(false);
}

function Cmds({ sugg }) {
  const sel = Math.min(S.cmdSel, sugg.length - 1);
  const from = Math.max(0, Math.min(sel - 4, sugg.length - 8)); // 8 at most, the chosen one in them
  return html`<div class="cmds">${sugg.slice(from, from + 8).map(([c, d], k) => {
    const j = from + k;
    return html`<div class=${'cmd' + (j === sel ? ' sel' : '')} onMouseDown=${e => { e.preventDefault(); takeCmd(c, false); }}>
      <span class="name">${c}</span>${d ? '  ' + d : ''}</div>`;
  })}${sugg.length > 8 ? html`<div class="cmd more">${sel + 1} of ${sugg.length} · ↑↓ · Tab completes · Enter runs</div>` : ''}</div>`;
}

let submitting = false; // waiting for uploads: a second Enter sends nothing more

async function submit(now) {
  if (submitting) return;
  if (!inputEl.value.trim() && !S.atts.length) {
    if (now && S.queue.length) send({ t: 'now' });
    return;
  }
  if (S.conn !== 'open') { notice('warn', 'Not connected: your message stays in the input until opt-chat is back.'); return; }
  if (/^\/model\s*$/.test(inputEl.value) && !S.atts.length) { setInput(''); openPicker(); return; }
  const panel = inputEl.value.match(/^\/(mcp|skills|plugins)\s*$/);
  if (panel && !S.atts.length) { setInput(''); openPanel(panel[1]); return; }
  submitting = true;
  try {
    // Uploads finish, those added meanwhile too, and the paths the input
    // names are looked for, so it goes as its chips say.
    while (S.atts.some(a => a.status === 'uploading')) await Promise.all(S.atts.map(a => a.promise));
    if (!pathsDone) lookForPaths(true);
    if (!pathsDone) {
      await new Promise(r => { pathsWait = r; setTimeout(r, 1500); });
      pathsWait = null;
    }
  } finally { submitting = false; }
  if (S.atts.some(a => a.status === 'error')) { notice('warn', 'An attachment failed: take it off (×) and send again.'); return; }
  const text = inputEl.value; // as it is now: typing went on meanwhile
  if (!text.trim() && !S.atts.length) return;
  const files = S.atts.map(({ name, orig, kind, size, at }) => ({ name, orig, kind, size, at: at && text.includes(at) ? at : undefined }));
  // Not looked for yet (opt-chat slow to answer): the server looks, as told.
  const paths = pathsDone ? S.paths.map(p => p.path).filter(p => !S.skipped.has(p)) : null;
  const seq = ++sendSeq;
  const payload = { t: 'send', text, files, paths, skip: [...S.skipped], chosen: [...S.chosen], now, seq, key: pageKey + ':' + seq };
  if (!send(payload)) return;
  toEnd = true; // what was sent, and its answer, show
  sending.set(seq, { payload, text, files: S.atts });
  if (histText(text) && !isCommand(text)) set({ inputs: mergeInputs(S.inputs, [histText(text)]) });
  if (inputEl.value === text) setInput('');
  histPos = -1;
  set({ atts: [], paths: [], skipped: new Set(), chosen: new Set() });
  forget();
}

function escape() {
  if (S.lightbox) { set({ lightbox: null }); return; }
  if (S.drawer) { set({ drawer: false }); return; }
  if (S.picker) { closePicker(); return; }
  if (S.panel) { closePanel(); return; }
  if (S.help) { set({ help: false }); return; } // one Esc, one thing: never a stop by surprise
  if (S.turn.phase) send({ t: 'stop' });
}

// caretFirstLine and caretLastLine tell whether ↑ or ↓ leaves the input's
// text, as in Claude Code: a long line wraps, and they move in it first.
// A hidden copy of the input, laid out alike, shows which row i is on.
let mirror = null;
function caretRow(el, i) {
  if (!mirror) { mirror = document.createElement('div'); mirror.className = 'mirror'; document.body.append(mirror); }
  const cs = getComputedStyle(el);
  Object.assign(mirror.style, { width: el.clientWidth + 'px', fontFamily: cs.fontFamily, fontSize: cs.fontSize, lineHeight: cs.lineHeight, tabSize: cs.tabSize });
  mirror.textContent = el.value.slice(0, i);
  const at = mirror.appendChild(document.createElement('span')); // the character at i, in a box of its own: no break moves
  at.textContent = i < el.value.length ? String.fromCodePoint(el.value.codePointAt(i)) : '​';
  mirror.append(el.value.slice(i + at.textContent.length) + '​'); // a last empty line has a height
  const lh = parseFloat(cs.lineHeight), top = mirror.getBoundingClientRect().top;
  return [Math.round((at.getBoundingClientRect().top - top) / lh), Math.round(mirror.offsetHeight / lh) - 1];
}
function caretFirstLine(el) { return !el.value.slice(0, el.selectionStart).includes('\n') && caretRow(el, el.selectionStart)[0] === 0; }
function caretLastLine(el) {
  if (el.value.slice(el.selectionEnd).includes('\n')) return false;
  const [row, last] = caretRow(el, el.selectionEnd);
  return row >= last;
}

let composedAt = -1e9; // Safari sends the Enter that ends a composition after compositionend

function onKey(e) {
  const el = e.target;
  if (e.isComposing || e.keyCode === 229) return;
  if (e.key === 'Enter' && e.timeStamp - composedAt < 100) { e.preventDefault(); return; }
  if (e.key === '?' && !el.value) { e.preventDefault(); set({ help: !S.help }); return; }
  const sugg = suggestions(el);
  if (sugg.length && !e.shiftKey && !e.altKey && !e.metaKey && !e.ctrlKey) {
    const sel = Math.min(S.cmdSel, sugg.length - 1);
    switch (e.key) {
      case 'ArrowDown': case 'ArrowUp':
        e.preventDefault();
        set({ cmdSel: (sel + (e.key === 'ArrowDown' ? 1 : sugg.length - 1)) % sugg.length });
        return;
      case 'Tab': e.preventDefault(); takeCmd(sugg[sel][0], false); return;
      case 'Enter': e.preventDefault(); takeCmd(sugg[sel][0], true); return;
      case 'Escape': e.preventDefault(); set({ cmdShut: el.value }); return; // the menu, not the turn
    }
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

// nextPlace is the placeholder for the next attachment, as Claude Code
// writes it: "[Image #2]". It marks the attachment's place in the message.
const PLACE = /\[(Image|PDF|File) #(\d+)\]/g;

// placeMax is the highest placeholder number in use: by an attachment, one
// taken off that may come back, or text that reads as one.
function placeMax(text) {
  let n = 0;
  for (const a of [...S.atts, ...gone]) n = Math.max(n, +(a.at?.match(/#(\d+)\]$/)?.[1] || 0));
  for (const m of text.matchAll(PLACE)) n = Math.max(n, +m[2]);
  return n;
}

function nextPlace(kind) {
  return `[${{ image: 'Image', pdf: 'PDF' }[kind] || 'File'} #${placeMax(inputEl?.value || '') + 1}]`;
}

// insertAtCaret types s where the caret is, or was when the input lost the
// focus (to 📎, say), spaced from the words around it; undo takes it back.
function insertAtCaret(s) {
  const el = inputEl;
  const i = el.selectionStart, j = el.selectionEnd;
  const before = el.value.slice(0, i), after = el.value.slice(j);
  const text = (before && !/\s$/.test(before) ? ' ' : '') + s + (!after || !/^\s/.test(after) ? ' ' : '');
  el.focus();
  el.setSelectionRange(i, j);
  if (!document.execCommand('insertText', false, text)) el.setRangeText(text, i, j, 'end');
  autosize();
}

// gone are the attachments taken off since the last send: one whose
// placeholder comes back (⌘Z) is back on.
let gone = [];

// placesGone takes off the attachments whose placeholder the user deleted,
// and puts back those whose placeholder is back.
function placesGone() {
  const v = inputEl.value;
  const out = S.atts.filter(a => a.at && !v.includes(a.at));
  const back = gone.filter(a => a.at && v.includes(a.at) && !S.atts.some(x => x.at === a.at));
  if (!out.length && !back.length) return;
  gone = gone.filter(a => !back.includes(a)).concat(out);
  set({ atts: S.atts.filter(a => !out.includes(a)).concat(back) });
}

// forget lets the attachments taken off go for good.
function forget() {
  for (const a of gone) {
    if (a.xhr && a.xhr.readyState < 4) a.xhr.abort(); // not uploaded for nothing
    if (a.url?.startsWith('blob:')) URL.revokeObjectURL(a.url);
  }
  gone = [];
}

function upload(file) {
  const id = ++uploadSeq;
  let orig = file.name || 'file';
  if (/^image\.(png|jpe?g|gif|webp)$/i.test(orig)) orig = `pasted-${new Date().toISOString().replace(/[:.]/g, '-')}.${orig.split('.').pop()}`;
  const kind = file.type.startsWith('image/') ? 'image' : file.type === 'application/pdf' ? 'pdf' : 'file';
  const att = {
    id, orig, size: file.size, kind, status: 'uploading', progress: 0, at: nextPlace(kind),
    url: file.type.startsWith('image/') ? URL.createObjectURL(file) : null,
  };
  if (inputEl) insertAtCaret(att.at); // before it is listed: the input's change can't take it off
  const update = patch => set({ atts: S.atts.map(a => a.id === id ? Object.assign(a, patch) : a) });
  const blob = att.url;
  att.promise = new Promise(resolve => {
    const xhr = att.xhr = new XMLHttpRequest();
    xhr.onloadend = () => resolve();
    xhr.onabort = () => update({ status: 'error', error: 'taken off' });
    xhr.open('POST', '/upload');
    xhr.setRequestHeader('X-Name', encodeURIComponent(orig));
    xhr.upload.onprogress = e => e.lengthComputable && update({ progress: e.loaded / e.total });
    xhr.onload = () => {
      if (xhr.status === 200) {
        const r = JSON.parse(xhr.responseText);
        update({ status: 'done', name: r.name, kind: r.kind, size: r.size, orig: r.orig, url: r.kind === 'image' ? '/' + r.name : null });
        if (blob) URL.revokeObjectURL(blob);
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
    checkbox({ checked }) { return checked ? '☑ ' : '☐ '; },
    // A reply's HTML shows as the text it is, as in Claude Code: Vec<String>
    // or "a <details> element" are words, not markup. A <br>, as in a table's
    // cell, still breaks.
    html({ text, block }) {
      if (/^<br\s*\/?>$/i.test(text)) return text;
      return block ? `<p>${esc(text.trim()).replace(/\n/g, '<br>')}</p>` : esc(text);
    },
    code({ text, lang }) {
      // Only a named language is highlighted, as Claude Code does: a guess
      // costs much and would change the colours when the reply is done. A
      // fence still open as it streams waits to be closed.
      const language = (lang || '').split(/\s/)[0];
      let body;
      try {
        body = language && !plainCode && hljs.getLanguage(language) ? hljs.highlight(text, { language }).value : esc(text);
      } catch { body = esc(text); }
      return `<div class="code"><span class="copy" role="button" tabindex="0">copy</span><pre><code class="hljs">${body}</code></pre></div>`;
    },
  },
});

DOMPurify.addHook('afterSanitizeAttributes', node => {
  if (node.tagName === 'A') { node.setAttribute('target', '_blank'); node.setAttribute('rel', 'noopener noreferrer'); }
});
// Only the classes the renderer and hljs write: no reply passes for a
// question or a message of the user's.
DOMPurify.addHook('uponSanitizeAttribute', (node, data) => {
  if (data.attrName === 'class') data.attrValue = data.attrValue.split(/\s+/).filter(c => /^(hljs|language-)|_$|^(code|copy)$/.test(c)).join(' ');
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
let plainCode = false; // the block underway is a fence not closed yet: hljs on every frame would cost
function mdLive(text) {
  const tokens = marked.lexer(text);
  const links = JSON.stringify(tokens.links || {}); // a block's links can be defined below it
  let out = '';
  tokens.forEach((t, j) => {
    const key = t.raw + '\0' + links;
    let h = blockCache.get(key);
    if (h === undefined) {
      const one = [t];
      one.links = tokens.links;
      plainCode = j === tokens.length - 1 && t.type === 'code' && !/\n {0,3}(`{3,}|~{3,})\s*$/.test(t.raw);
      try { h = pure(marked.parser(one)); } finally { plainCode = false; }
      if (j < tokens.length - 1) { // the last may still grow
        blockCache.set(key, h);
        if (blockCache.size > 500) blockCache.delete(blockCache.keys().next().value);
      }
    }
    out += h;
  });
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

function homeTilde(s) {
  const home = S.status?.dir?.match(/^\/(Users|home)\/[^/]+/)?.[0];
  return home ? s.split(home + '/').join('~/') : s;
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
    } else if (m.kind === 'opt' || m.kind === 'ai') {
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

// rows are what the log shows, in order: the messages, the day lines, and
// the notices, each after the message it followed. A tool call with no
// result is running when the turn underway made it (from on), else it was
// cut short.
function rows(msgs, phase, from, notices, asides) {
  const first = msgs.length ? msgs[0].i : 0; // asides before the page's messages wait for theirs
  const notes = notices.map(n => ({ note: n, after: n.after, key: 'n' + n.id }))
    .concat(asides.filter(a => a.after >= first - 1).map(a => ({ aside: a, after: a.after, key: 'b' + a.date })))
    .sort((a, b) => a.after - b.after);
  const out = [];
  let j = 0;
  const flush = before => {
    for (; j < notes.length && notes[j].after < before; j++) out.push(notes[j]);
  };
  for (const it of items(msgs)) {
    flush(it.sep ? it.i : it.m.i);
    if (it.sep) { out.push(it); continue; }
    it.state = it.m.kind !== 'tool' ? '' : it.echo ? 'done' : phase && it.m.i >= from ? 'run' : 'cut';
    out.push(it);
  }
  flush(Infinity);
  return out;
}

function row(it) {
  if (it.note) return html`<${Notice} key=${it.key} n=${it.note}/>`;
  if (it.aside) return html`<${AsideView} key=${it.key} a=${it.aside}/>`;
  if (it.sep) return html`<div class="sep" key=${it.key}><span class="lbl">${it.sep}</span></div>`;
  return html`<${MsgView} key=${it.key} m=${it.m} echo=${it.echo} state=${it.state}/>`;
}

// Log is the chat, virtualized: only the rows in view and near it are in
// the page, so a log of days stays as quick as a fresh one. virtua measures
// each row as it shows; shift keeps the place when rows come or go above.
const Log = memo(function Log({ list, shift, margin, scroller, onResize }) {
  return html`<${Virtualizer} data=${list} shift=${shift} startMargin=${margin} scrollRef=${scroller} bufferSize=${1200} onResize=${onResize}>${row}</${Virtualizer}>`;
});

function Notice({ n }) {
  return html`<div class=${'notice ' + n.level}><span>${n.level === 'warn' ? '!' : '·'}</span><span>${n.s}</span></div>`;
}

// AsideView is a side question (/btw) and its answer, set apart: the chat
// never sees them.
function AsideView({ a }) {
  const q = useMemo(() => ({ i: 'btw' + a.date, kind: 'user', text: a.q, images: a.images, date: a.date }), [a.date]); // its question never changes
  return html`<div class="aside">
    <div class="aside-tag">btw · beside the chat, which never sees it</div>
    <${UserMsg} m=${q}/>
    ${a.a ? html`<div class="msg opt" title=${when(a.date)}><span class="mark" aria-hidden="true">●</span><div class="body"><${Markdown} text=${a.a}/></div></div>`
      : a.err ? html`<div class="notice warn"><span>!</span><span>No answer: ${a.err}</span></div>`
      : html`<div class="notice"><span>·</span><span>Answering…</span></div>`}
  </div>`;
}

const MsgView = memo(({ m, echo, state }) => {
  switch (m.kind) {
    case 'user': return html`<${UserMsg} m=${m}/>`;
    case 'opt': {
      const cut = m.text.endsWith(' [interrupted]'); // a reply stopped midway, as Turn.Run logs it
      return html`<div class="msg opt" title=${when(m.date)}><span class="mark" aria-hidden="true">●</span><div class="body"><${Markdown} text=${cut ? m.text.slice(0, -14) : m.text}/>
        ${cut && html`<div class="result"><span aria-hidden="true">  ⎿</span><span class="cut-note">Interrupted</span></div>`}</div></div>`;
    }
    case 'ai': // an imported chat's AI: its words, set apart from Opt's
      return html`<div class="msg ai" title=${when(m.date)}><span class="mark" aria-hidden="true">○</span><div class="body"><${Markdown} text=${m.text}/></div></div>`;
    case 'tool': return html`<${ToolMsg} m=${m} echo=${echo} state=${state}/>`;
    case 'echo': return html`<${ToolMsg} echo=${m} state="done"/>`;
    case 'work': return html`<${WorkMsg} m=${m}/>`;
    default: return html`<${NoteMsg} m=${m}/>`;
  }
});

function Markdown({ text, live }) {
  return html`<div class="md" dangerouslySetInnerHTML=${{ __html: live ? mdLive(text) : md(text) }}/>`;
}

// inlineParts splits a message's text at the attachment marks put in its
// words ("do this <img> not this <img>"), which show there; the marks after
// its text, each on a line of its own, show in a row below.
const TRAILING = new RegExp(`^(?:\\s*${MARK.source.replace(/^\\n\*/, '')})*\\s*$`);

function inlineParts(text) {
  const parts = [];
  let prev = 0, m;
  const re = new RegExp(MARK.source, 'g');
  while ((m = re.exec(text))) {
    if (TRAILING.test(text.slice(m.index))) break; // the marks at the end: shown in a row
    parts.push(text.slice(prev, m.index), { name: unesc(m[1]), path: unesc(m[2]), size: m[3], content: m[4] });
    prev = m.index + m[0].length;
  }
  return { parts, tail: text.slice(prev) };
}

function UserMsg({ m }) {
  const { body, marks } = useMemo(() => splitMarks(m.text), [m.text]);
  const { parts, tail } = useMemo(() => inlineParts(m.text), [m.text]);
  const [open, setOpen] = useKept('u' + m.i, false);
  const [shown, setShown] = useKept('i' + m.i, -1); // the inline text file opened
  const lines = body.split('\n');
  const long = lines.length > 20 || body.length > 3000; // a big paste shows folded, as in Claude Code: one long line too
  const head = long ? lines.slice(0, 12).join('\n').slice(0, 1500) : body;
  const hidden = lines.length - head.split('\n').length;
  if (parts.length && body && !long) { // attachments alone show in a row, as ever
    const rest = splitMarks(tail);
    const inline = parts.filter(p => typeof p !== 'string');
    return html`<div class="msg user" title=${when(m.date)}>
      <span class="mark" aria-hidden="true">▸</span>
      <div class="body">
        <div class="text">${parts.map((p, j) => typeof p === 'string' ? p : html`<${MarkView} mk=${p} images=${m.images} inline=${true} onOpen=${() => setShown(shown === j ? -1 : j)} open=${shown === j}/>`)}${rest.body}</div>
        ${shown >= 0 && parts[shown]?.content !== undefined && html`<div class="attach-text"><pre>${parts[shown].content}</pre></div>`}
        ${rest.marks.filter(isVoice).map((mk, j) => html`<${Voice} mk=${mk} k=${`v${m.i}.${j}`}/>`)}
        <${Attached} id=${m.i} marks=${rest.marks.filter(mk => !isVoice(mk))} images=${(m.images || []).filter(n => !inline.some(p => n.endsWith('/' + p.path.split('/').pop())))}/>
      </div>
    </div>`;
  }
  return html`<div class="msg user" title=${when(m.date)}>
    <span class="mark" aria-hidden="true">▸</span>
    <div class="body">
      ${body && html`<div class="text">${long && !open ? head : body}</div>`}
      ${long && html`<button class="more" onClick=${() => setOpen(!open)}>${open ? '▴ fold' : `… +${hidden ? count(hidden, 'line') : count(body.length - head.length, 'character')} (click to expand)`}</button>`}
      ${marks.filter(isVoice).map((mk, j) => html`<${Voice} mk=${mk} k=${`v${m.i}.${j}`}/>`)}
      <${Attached} id=${m.i} marks=${marks.filter(mk => !isVoice(mk))} images=${m.images}/>
    </div>
  </div>`;
}

// MarkView is one attachment where its message put it: an image as a
// thumbnail, a text file as a chip that opens its text, any other a chip.
function MarkView({ mk, images = [], inline, open, onOpen }) {
  const url = fileURL(mk.path);
  const base = mk.path.split('/').pop();
  const cls = inline ? ' inline' : '';
  if (url && images.some(n => n.endsWith('/' + base)) && /\.(png|jpe?g|gif|webp)$/.test(base)) {
    return html`<img class=${'thumb' + cls} src=${url} alt=${mk.name} title=${mk.name + ' · ' + mk.size} loading="lazy" role="button" tabindex="0" onClick=${() => set({ lightbox: url })}/>`;
  }
  if (mk.content !== undefined) {
    return html`<button class=${'chip' + cls} title=${mk.path} onClick=${onOpen}>📄 <span class="name">${mk.name}</span> <span class="size">${mk.size}</span> ${open ? '▴' : '▾'}</button>`;
  }
  return html`<a class=${'chip' + cls} href=${url || undefined} target="_blank" rel="noopener" title=${mk.path}>${/\.pdf$/i.test(mk.name) ? '📕' : '📎'} <span class="name">${mk.name}</span> <span class="size">${mk.size}</span></a>`;
}

// Attached shows a message's attachments: images as thumbnails, other
// files as chips; a text file's chip opens its text.
function Attached({ id, marks = [], images = [] }) {
  const [open, setOpen] = useKept('a' + id, -1);
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
  return html`<div class="msg work" title=${when(m.date)}><span class="mark" aria-hidden="true">◆</span><div class="body">
    <div class="who">[${who}] report</div><${Markdown} text=${text}/></div></div>`;
}

function NoteMsg({ m }) {
  if (!m.text.includes('\n') && / \(imported\)$/.test(m.text)) {
    return html`<div class="sep" title=${m.text}><span class="lbl">${m.text.replace(/ \(imported\)$/, '')} · imported</span></div>`;
  }
  return html`<div class="msg note" title=${when(m.date)}><span class="mark" aria-hidden="true">※</span><div class="body"><div class="text">${m.text}</div>
    <${Attached} id=${m.i} images=${m.images}/></div></div>`;
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
    case 'search': return `"${oneLine(x.text || '', 100)}"${x.before ? ` before ${x.before}` : ''}`;
    case 'spawn': case 'tell': return `[${x.name}] ${oneLine(x.task || x.message, 100)}`;
    default: return oneLine(JSON.stringify(x), 140);
  }
}

// partial reads a tool call's input as far as it is written: the JSON,
// closed where it stops.
function partial(raw) {
  for (const end of ['', '"}', '}', '"]}', ']}', '""}', ':""}']) {
    try { const x = JSON.parse(raw + end); if (x && typeof x === 'object') return x; } catch {}
  }
  return null;
}

// LiveTool is the tool call being written: what it will run, as it comes.
function LiveTool({ name, raw }) {
  const x = partial(raw);
  const long = name === 'Bash' && typeof x?.command === 'string' && x.command.includes('\n');
  return html`<div class="msg tool run live"><span class="mark" aria-hidden="true">●</span><div class="body">
    <div class="head"><span class="name">${name}</span><span class="args">(${toolArgs(name, x, raw)})</span></div>
    ${long && html`<div class="full"><pre>${x.command}</pre></div>`}
  </div></div>`;
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
  const [open, setOpen] = useKept('t' + (m || echo).i, false);
  const tool = m ? parseTool(m.text) : null;
  const out = echo ? echo.text : '';
  const lines = out ? out.replace(/\n+$/, '').split('\n') : [];
  const refused = out.startsWith('The user doesn\'t want to proceed with this tool use.') || out === 'The user said no to this call.';
  const failed = refused || /^(<tool_use_error>|Error\b|error:)/i.test(out.trim());
  const cls = 'msg tool ' + (state === 'run' ? 'run' : state === 'cut' || failed ? 'cut' : '');
  const toggle = () => { if (!getSelection().toString()) setOpen(!open); }; // not when selecting its text
  const sum = echo && !failed && tool ? brief(tool.name, tool.input, lines, echo) : '';
  return html`<div class=${cls} title=${when((m || echo).date)}>
    <span class="mark" aria-hidden="true">●</span>
    <div class="body">
      ${tool && html`<div class="head" role="button" tabindex="0" aria-expanded=${open} onClick=${toggle}><span class="name">${tool.name}</span><span class="args">(${toolArgs(tool.name, tool.input, tool.raw)})</span></div>`}
      ${echo && !open && sum && html`<div class="result" role="button" tabindex=${tool ? -1 : 0} onClick=${toggle}><span aria-hidden="true">  ⎿</span><span class="lines">${sum} <span class="more">(click to expand)</span></span></div>`}
      ${echo && !open && refused && html`<div class="result" role="button" tabindex=${tool ? -1 : 0} onClick=${toggle}><span aria-hidden="true">  ⎿</span><span class="cut-note">No (you said no)</span></div>`}
      ${echo && !open && !sum && !refused && html`<div class="result" role="button" tabindex=${tool ? -1 : 0} onClick=${toggle}><span aria-hidden="true">  ⎿</span><span class="lines">${lines.slice(0, 3).map(l => oneLine(l, 160)).join('\n') || '(no output)'}${lines.length > 3 && html`<span class="more">${'\n'}… +${lines.length - 3} lines (click to expand)</span>`}</span></div>`}
      ${state === 'cut' && !echo && html`<div class="result"><span aria-hidden="true">  ⎿</span><span class="cut-note">Interrupted</span></div>`}
      ${open && html`<div class="full">
        ${tool && html`<div class="label">input</div><${ToolInput} name=${tool.name} x=${tool.input} raw=${tool.raw}/>`}
        ${echo && html`<div class="label">output</div><pre onClick=${toggle}>${out || '(no output)'}</pre>`}
      </div>`}
      ${echo && echo.images && echo.images.length > 0 && html`<${Attached} id=${echo.i} images=${echo.images}/>`}
    </div>
  </div>`;
}

// ---------------------------------------------------------------- model picker

// The picker lists Claude Code's models as its /model does: the aliases,
// each its family's latest, and the one in use when it is another.
const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'];

function pickerRows() {
  const all = S.models || [];
  const rows = all.filter(m => !m.value.startsWith('claude-') && m.value !== 'default'); // default: one of those, by another name
  const cur = S.status?.model;
  if (cur && S.models && !rows.some(m => m.value === cur)) {
    rows.push(all.find(m => m.value === cur) || { value: cur, displayName: cur, description: 'set with -model or /model', supportsEffort: true });
  }
  return rows;
}

function modelLabel(m) {
  return /^[a-z]+$/.test(m.value) && m.value !== 'default' ? m.value[0].toUpperCase() + m.value.slice(1) : m.displayName;
}

function modelBlurb(m) {
  return [m.displayName !== modelLabel(m) && m.value !== 'default' && m.displayName, m.description].filter(Boolean).join(' · ');
}

function effortName(e) { return e === 'xhigh' ? 'Extra high' : e[0].toUpperCase() + e.slice(1); }

function levelsOf(m) { return m?.supportedEffortLevels?.length ? m.supportedEffortLevels : EFFORTS; }

function openPicker() {
  if (S.panel) closePanel();
  if (!S.models || S.modelsError) send({ t: 'models' });
  set({ picker: { sel: -1, effort: S.status?.effort || 'high' }, help: false });
}

function closePicker() { set({ picker: null }); inputEl?.focus(); }

function pickerSel(rows) {
  const p = S.picker;
  return p.sel >= 0 ? Math.min(p.sel, rows.length - 1) : Math.max(0, rows.findIndex(m => m.value === S.status?.model));
}

function pickModel(save) {
  const rows = pickerRows(), m = rows[pickerSel(rows)];
  if (!m) return;
  const levels = levelsOf(m);
  const effort = levels.includes(S.picker.effort) ? S.picker.effort : levels.includes('high') ? 'high' : levels[levels.length - 1];
  send({ t: 'model', model: m.value, effort, save });
  closePicker();
}

// pickerKey takes the keys while the picker is open: true when it took e.
function pickerKey(e) {
  const p = S.picker;
  if (!p) return false;
  const rows = pickerRows();
  let sel = pickerSel(rows), effort = p.effort;
  const levels = levelsOf(rows[sel]);
  switch (e.key) {
    case 'Escape': closePicker(); break;
    case 'ArrowUp': sel = (sel + rows.length - 1) % Math.max(1, rows.length); break;
    case 'ArrowDown': sel = (sel + 1) % Math.max(1, rows.length); break;
    case 'ArrowLeft': case 'ArrowRight': {
      let i = levels.indexOf(effort);
      if (i < 0) i = Math.max(0, levels.indexOf('high'));
      effort = levels[Math.min(levels.length - 1, Math.max(0, i + (e.key === 'ArrowRight' ? 1 : -1)))];
      break;
    }
    case 'Enter': pickModel(true); break;
    case 's': case 'S': pickModel(false); break;
    default:
      if (/^[1-9]$/.test(e.key) && +e.key <= rows.length) sel = +e.key - 1;
      else if (e.key === 'Tab' || e.metaKey || e.ctrlKey) return false;
  }
  e.preventDefault();
  e.stopPropagation();
  if (S.picker) set({ picker: { sel, effort } });
  return true;
}

function Picker() {
  const p = S.picker, rows = pickerRows(), sel = pickerSel(rows), m = rows[sel];
  const supports = !m || m.supportsEffort !== false;
  const levels = levelsOf(m);
  const effort = levels.includes(p.effort) ? p.effort : 'high';
  return html`<div class="picker" role="dialog" aria-label="Select model">
    <div class="title">Select model</div>
    <div class="dim">Switch between Claude models. Enter keeps your pick for later runs too; s, for this run only.</div>
    ${S.models === null ? html`<div class="rows dim">Asking Claude Code for its models…</div>` : html`<div class="rows">
      ${S.modelsError && html`<div class="err">Claude Code didn't list its models: ${S.modelsError}</div>`}
      ${rows.map((r, j) => html`<div class=${'row' + (j === sel ? ' sel' : '')} key=${r.value}
          onClick=${() => set({ picker: { ...S.picker, sel: j } })} onDblClick=${() => { set({ picker: { ...S.picker, sel: j } }); pickModel(true); }}>
        <span class="ptr">${j === sel ? '❯' : ''}</span><span class="num">${j + 1}.</span>
        <span class="lbl">${modelLabel(r)}${r.value === S.status?.model ? html` <span class="ok">✔</span>` : ''}</span>
        <span class="desc">${modelBlurb(r)}</span>
      </div>`)}
    </div>`}
    <div class="effort">${supports
      ? html`<span class="dot">●</span> ${effortName(effort)} effort <span class="dim">←/→ to adjust</span>
          <span class="scale">${levels.map(l => html`<span class=${l === effort ? 'on' : ''}>${l}</span>`)}</span>`
      : html`<span class="dim">This model takes no effort setting.</span>`}</div>
    <div class="dim">Enter to set as default · s to use this run only · Esc to cancel</div>
  </div>`;
}

// ---------------------------------------------------------------- permissions

// The modes Shift+Tab cycles, as Claude Code's, and how the bar names them.
const MODES = ['manual', 'acceptEdits', 'auto'];
const modeNames = { manual: '⏵ manual', default: '⏵ manual', acceptEdits: '⏵⏵ accept edits on', auto: '⏵⏵ auto mode on', bypassPermissions: '⏵⏵ bypass permissions on', dontAsk: '⏵⏵ don\'t ask on' };

function cycleMode() {
  const m = S.status?.mode === 'default' ? 'manual' : S.status?.mode; // Claude Code's name for manual
  send({ t: 'mode', mode: MODES[(MODES.indexOf(m) + 1) % MODES.length] });
}

function askChoices(q) {
  return [['Yes', true, false], q.always && [`Yes, and don't ask again for ${homeTilde(q.always)}`, true, true], ['No, and tell Opt what to do differently (esc)', false, false]].filter(Boolean);
}

function answer(q, [, allow, always]) {
  send({ t: 'answer', id: q.id, allow, always });
  set({ asks: S.asks.filter(x => x.id !== q.id), askSel: 0 }); // at once: the server confirms
  if (!S.asks.length) inputEl?.focus(); // else the next question takes it
}

// askKey takes the keys for the question shown while it has the focus: it
// takes it as it comes up, unless the user is typing, whose keys never
// answer it by chance; Tab or a click goes to it. True when it took e.
function askKey(e) {
  const q = S.asks[0];
  if (!q || S.picker || S.lightbox || S.help || e.metaKey || e.ctrlKey || e.altKey) return false;
  if (!e.target.closest?.('.ask')) return false;
  const choices = askChoices(q);
  let sel = Math.min(S.askSel, choices.length - 1);
  switch (e.key) {
    case 'ArrowUp': sel = (sel + choices.length - 1) % choices.length; break;
    case 'ArrowDown': sel = (sel + 1) % choices.length; break;
    case 'Enter': answer(q, choices[sel]); break;
    case 'Escape': answer(q, choices[choices.length - 1]); break;
    default:
      if (/^[1-9]$/.test(e.key) && +e.key <= choices.length) { answer(q, choices[+e.key - 1]); break; }
      return false;
  }
  e.preventDefault();
  e.stopPropagation();
  if (S.asks[0] === q) set({ askSel: sel });
  return true;
}

function askTitle(tool, x) {
  switch (tool) {
    case 'Bash': return 'Bash command';
    case 'Edit': case 'MultiEdit': return 'Edit file';
    case 'Write': return 'Create file';
    case 'WebFetch': return 'Fetch';
    case 'WebSearch': return 'Web search';
    default: return tool;
  }
}

let typedAt = 0; // the last key typed in the input

function Ask({ q }) {
  const x = q.input && typeof q.input === 'object' ? q.input : null;
  const choices = askChoices(q), sel = Math.min(S.askSel, choices.length - 1);
  const ref = useRef();
  const [focused, setFocused] = useState(false);
  useEffect(() => { // up: the keys are its, unless the user is typing
    if (!inputEl?.value && performance.now() - typedAt > 1500) ref.current.focus();
  }, [q.id]);
  return html`<div class=${'ask' + (focused ? ' focused' : '')} role="dialog" aria-label="Permission" tabindex="-1" ref=${ref}
      onFocusIn=${() => setFocused(true)} onFocusOut=${() => setFocused(false)}>
    <div class="title">${askTitle(q.tool, x)}${q.who ? html` <span class="dim">· [${q.who}] asks</span>` : ''}${S.asks.length > 1 ? html` <span class="dim">· ${S.asks.length - 1} more waiting</span>` : ''}</div>
    <div class="what">
      ${q.tool === 'Bash' && x ? html`<pre>${x.command}</pre>` : html`<div class="args">${q.tool}(${toolArgs(q.tool, x, JSON.stringify(q.input))})</div><${ToolInput} name=${q.tool} x=${x} raw=${JSON.stringify(q.input)}/>`}
      ${q.desc && html`<div class="dim">${q.desc}</div>`}
    </div>
    <div>Do you want to proceed?${!focused && html` <span class="dim">(Tab or a click here to answer)</span>`}</div>
    <div class="choices">${choices.map((c, j) => html`<div class=${'row' + (j === sel ? ' sel' : '')} role="button" tabindex="-1" onClick=${() => answer(q, c)}>
      <span class="ptr">${j === sel ? '❯' : ''}</span><span>${j + 1}. ${c[0]}</span></div>`)}</div>
  </div>`;
}

// ---------------------------------------------------------------- /mcp and /skills

// The panels show the user's Claude Code MCP servers and skills, as its
// /mcp and /skills do. The MCP list is asked again while it is open, as
// servers connect.
let panelTimer = 0;

function openPanel(kind) {
  set({ panel: { kind, sel: 0, busy: true, error: '' }, picker: null, help: false });
  send({ t: kind });
  clearInterval(panelTimer);
  if (kind === 'mcp') panelTimer = setInterval(() => { if (S.panel?.kind === 'mcp' && !S.panel.busy) send({ t: 'mcp' }); }, 2000);
}

function closePanel() { clearInterval(panelTimer); set({ panel: null }); inputEl?.focus(); }

function panelRows() { return (S.panel && S[S.panel.kind]) || []; }

function panelAct(action, row, extra) {
  if (!row || S.panel.acting) return; // one at a time: a second Enter would undo the first
  set({ panel: { ...S.panel, busy: true, acting: true, error: '' } });
  send({ t: S.panel.kind, action, name: row.id || row.name, ...extra });
}

function toggleRow(row) {
  if (!row) return;
  if (S.panel.kind === 'mcp') panelAct(row.off ? 'on' : 'off', row);
  else if (S.panel.kind === 'plugins') panelAct(row.on ? 'off' : 'on', row);
  else if (!row.locked_by) panelAct('set', row, { state: row.state === 'off' ? 'on' : 'off' });
}

// panelKey takes the keys while the panel has the focus, as it has once
// opened: a click in the input types a message.
function panelKey(e) {
  const p = S.panel;
  if (!p || S.picker || e.metaKey || e.ctrlKey || e.altKey) return false;
  if (!e.target.closest?.('.panel')) return false;
  const rows = panelRows(), row = rows[p.sel];
  let sel = p.sel;
  switch (e.key) {
    case 'Escape': closePanel(); break;
    case 'ArrowUp': sel = Math.max(0, sel - 1); break;
    case 'ArrowDown': sel = Math.min(rows.length - 1, sel + 1); break;
    case 'PageUp': sel = Math.max(0, sel - 10); break;
    case 'PageDown': sel = Math.min(rows.length - 1, sel + 10); break;
    case 'Enter': case ' ': toggleRow(row); break;
    case 'a': if (p.kind === 'mcp') panelAct('signin', row); break;
    case 'r': if (p.kind === 'mcp') panelAct('reconnect', row); break;
    default: return false;
  }
  e.preventDefault();
  e.stopPropagation();
  if (S.panel && sel !== p.sel) {
    set({ panel: { ...S.panel, sel } });
    requestAnimationFrame(() => document.querySelector('.panel .row.sel > .lbl')?.scrollIntoView({ block: 'nearest' })); // the row itself has no box
  }
  return true;
}

const mcpMarks = { connected: ['✔', 'ok'], pending: ['…', 'dim'], 'needs-auth': ['△', 'warn'], failed: ['✘', 'bad'], disabled: ['○', 'dim'] };

function McpRow({ s }) {
  const [glyph, cls] = s.off ? ['○', 'dim'] : mcpMarks[s.status] || ['·', 'dim'];
  const what = s.off ? 'off' : s.status === 'needs-auth' ? 'needs sign-in (a)' : s.status === 'failed' ? 'failed (r to reconnect)' : s.status;
  return html`<span class="lbl">${s.name}</span><span class=${'st ' + cls}>${glyph} ${what}</span><span class="desc">${s.source || s.scope || ''}${s.error ? ' · ' + s.error : ''}</span>`;
}

function SkillRow({ s }) {
  const on = s.state !== 'off';
  return html`<span class="lbl">${s.display_name || s.name}</span><span class=${'st ' + (on ? 'ok' : 'dim')}>${on ? '●' : '○'} ${s.state}</span>
    <span class="desc">${s.source}${s.locked_by === 'plugin' ? ' (its plugin\'s: /plugins)' : s.locked_by ? ` (managed by its ${s.locked_by})` : ''} · ${s.tokens} tokens · ${oneLine(s.description, 120)}</span>`;
}

function PluginRow({ s }) {
  const what = [s.skills && count(s.skills, 'skill'), s.mcp && count(s.mcp, 'MCP server'), s.tokens && `${s.tokens.toLocaleString()} tokens`].filter(Boolean).join(' · ');
  return html`<span class="lbl">${s.name}</span><span class=${'st ' + (s.on ? 'ok' : 'dim')}>${s.on ? '● on' : '○ off'}</span>
    <span class="desc">${s.id.slice(s.name.length + 1)} · ${s.scope}${s.on && what ? ' · ' + what : ''}</span>`;
}

const panels = {
  mcp: { title: 'MCP servers', Row: McpRow, on: r => !r.off,
    about: (on, all) => `Your Claude Code MCP servers, as its /mcp lists them: ${on.length} of ${all.length} on for Opt. Off is kept for Opt alone; a sign-in is Claude Code's.` },
  skills: { title: 'Skills', Row: SkillRow, on: r => r.state !== 'off',
    about: (on, all) => `Your Claude Code skills, as its /skills lists them: ${on.length} of ${all.length} on, ${on.reduce((n, r) => n + (r.tokens || 0), 0).toLocaleString()} tokens listed. On or off is kept for Opt alone, from its next call.` },
  plugins: { title: 'Plugins', Row: PluginRow, on: r => r.on,
    about: (on, all) => `Your Claude Code plugins, as its /plugins lists them: ${on.length} of ${all.length} on for Opt, each with its skills and MCP servers. On or off is kept for Opt alone, from its next call.` },
};

function Panel() {
  const p = S.panel, rows = panelRows(), mcp = p.kind === 'mcp', kind = panels[p.kind];
  const ref = useRef();
  useEffect(() => ref.current.focus(), [p.kind]);
  const loaded = S[p.kind];
  return html`<div class="picker panel" role="dialog" aria-label=${kind.title} tabindex="-1" ref=${ref}>
    <div class="title">${kind.title}${p.busy ? html` <span class="dim">…</span>` : ''}</div>
    <div class="dim">${kind.about(rows.filter(kind.on), rows)}</div>
    ${p.error && html`<div class="err">${p.error}</div>`}
    ${/^https?:\/\//i.test(p.signIn || '') && html`<div>Sign in to ${p.name}: <a href=${p.signIn} target="_blank" rel="noopener noreferrer">open the sign-in page</a>, then come back here.</div>`}
    ${!loaded ? html`<div class="dim">Asking Claude Code…</div>` : html`<div class="rows list">
      ${rows.map((r, j) => html`<div class=${'row' + (j === p.sel ? ' sel' : '')} key=${r.id || r.name}
          onClick=${() => { set({ panel: { ...S.panel, sel: j } }); ref.current.focus(); }} onDblClick=${() => toggleRow(r)}>
        <span class="ptr">${j === p.sel ? '❯' : ''}</span><${kind.Row} s=${r}/>
      </div>`)}
      ${!rows.length && html`<div class="dim">None.</div>`}
    </div>`}
    <div class="dim">↑/↓ · Enter or Space: on/off${mcp ? ' · a: sign in · r: reconnect' : ''} · Esc to close</div>
  </div>`;
}

// ---------------------------------------------------------------- the page

const still = matchMedia('(prefers-reduced-motion: reduce)'); // then the glyph stays put
const glyphs = ['·', '✢', '✳', '✶', '✻', '✽', '✻', '✶', '✳', '✢'];

function Spinner({ turn, chars, queued }) {
  const [, tick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => tick(n => n + 1), 120);
    return () => clearInterval(t);
  }, []);
  const g = still.matches ? '✻' : glyphs[Math.floor(Date.now() / 120) % glyphs.length];
  let verb = 'Thinking';
  switch (S.asks.length ? 'asks' : turn.phase) {
    case 'asks': verb = 'Waiting for your permission'; break;
    case 'settling': verb = turn.backlog ? `Waiting for ${turn.backlog} message summar${turn.backlog === 1 ? 'y' : 'ies'}` : 'Getting ready'; break;
    case 'reading': verb = 'Reading memory'; break;
    case 'writing': verb = 'Writing'; break;
    case 'tool': verb = !turn.tool ? 'Calling a tool' : S.input ? `Writing the ${turn.tool} call` : `Running ${turn.tool}`; break;
  }
  const meta = [elapsed(Date.now() - (turn.since || Date.now()))];
  const tokens = (turn.tokens || 0) + Math.round(chars / 4);
  if (tokens) meta.push(`↓ ${k(tokens)} tokens`);
  if (turn.context) meta.push(html`<span title="tokens the model reads in its latest request: the view, the prompt, your message and the turn so far">${k(turn.context)} context</span>`);
  if (turn.phase === 'settling' && turn.written) meta.push(`${count(turn.written, 'line')} written`);
  meta.push(html`<button title="stop the turn" onClick=${() => send({ t: 'stop' })}>esc to interrupt</button>`);
  if (queued || turn.phase === 'settling') meta.push(html`<button title="stop the turn and send the queue now" onClick=${() => send({ t: 'now' })}>⌘⏎ send now</button>`);
  return html`<div class="spinner"><span><span class="glyph" aria-hidden="true">${g}</span>${verb}…</span><span class="meta">(${meta.map((x, j) => html`${j ? ' · ' : ''}${x}`)})</span>
    ${turn.partial > 0 && html`<span class="meta">memory partial: ${count(turn.partial, 'message')} not summarized yet</span>`}</div>`;
}

function Queue({ queue }) {
  const [editing, setEditing] = useState(null);
  if (!queue.length) return null;
  return html`<div class="queue">
    ${queue.map(q => html`<div class="item" key=${q.id}>
      <span aria-hidden="true">▸</span>
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

// takeOff removes an attachment, and its placeholder from the input
// unless the user just deleted it (typed).
function takeOff(a, typed) {
  gone.push(a); // its placeholder put back (⌘Z): back on
  set({ atts: S.atts.filter(x => x.id !== a.id) });
  if (!typed && a.at && inputEl?.value.includes(a.at)) {
    const v = inputEl.value, i = v.indexOf(a.at);
    const j = i + a.at.length + (v[i + a.at.length] === ' ' ? 1 : 0);
    inputEl.focus();
    inputEl.setSelectionRange(i, j);
    if (!document.execCommand('delete')) inputEl.setRangeText('', i, j, 'end');
    autosize();
  }
  if (!typed) inputEl?.focus();
}

function Chips() {
  const { atts, paths, skipped } = S;
  if (!atts.length && !paths.length) return null;
  const attach = p => { const s = new Set(S.skipped); s.delete(p); set({ skipped: s, chosen: new Set([...S.chosen, p]) }); inputEl?.focus(); };
  const leave = p => { set({ skipped: new Set([...S.skipped, p]) }); inputEl?.focus(); };
  return html`<div class="atts">
    ${atts.map(a => html`<span class=${'chip' + (a.status === 'error' ? ' err' : '')} key=${a.id} title=${a.error || a.orig}>
      ${a.url ? html`<img src=${a.url} alt=""/>` : icon(kindOf(a))}
      ${a.at && html`<span class="at">${a.at}</span>`}<span class="name">${a.orig}</span>
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
    right.push(html`<button title="choose the model and effort (Alt+P)" onClick=${openPicker}>${s.model} · ${s.effort}</button>`);
    right.push(html`<span title="every message in the chat, kept for good">${count(s.msgs, 'message')}</span>`,
      html`<span title="the view: what each turn reads instead of the whole chat, every message summarized in a tree of about this size">view ${count(s.viewLines, 'line')}, ${Math.round(s.viewBytes / 1000)} KB</span>`);
    if (s.backlog || s.merges) right.push(html`<span class="busy" title=${count(s.running, 'compaction') + ' running'}>summarizing ${[s.backlog && count(s.backlog, 'message'), s.merges && count(s.merges, 'merge')].filter(Boolean).join(' + ')}</span>`);
    if (s.written) right.push(html`<span title="summary lines the view's compactions wrote since opt-chat started">${count(s.written, 'summary line')} written</span>`);
    if (s.failing) right.push(html`<span class="conn bad">${s.failing} failing, retrying</span>`);
    if (s.agents?.length) right.push(html`agents: ${s.agents.map((a, j) => html`${j > 0 && ' '}<button title="see what it does now (Alt+A: the agents)" onClick=${() => toggleRail(a)}>[${a}]</button>`)}`);
    else if (s.subs?.length && !railShown()) right.push(html`<button title="this run's subagents (Alt+A)" onClick=${() => toggleRail()}>agents</button>`);
  }
  if (s?.limits?.length) right.push(html`<${Limits} windows=${s.limits}/>`);
  if (l) {
    // Of all the input the turn's requests sent, the share read back from the
    // prompt cache: low after the view merged, else a sign the cache broke.
    const u = ['view', 'turn'].map(n => l[n] || {}), sum = f => u.reduce((t, x) => t + (x[f] || 0), 0);
    const read = sum('cache_read_input_tokens'), all = read + sum('cache_creation_input_tokens') + sum('input_tokens');
    right.push(html`<span title="tokens. in: the turn's biggest request, its context: the view, the prompt and tools, your message, its steps. cached: the share of all its requests' input read from the prompt cache. out: what Opt wrote, its thinking too">last turn: ${l.context ? k(l.context) + ' in, ' : ''}${all ? Math.floor(100 * read / all) + '% cached, ' : ''}${k(l.turn.output_tokens)} out</span>`);
  }
  const conn = { open: ['ok', '● connected'], connecting: ['', '○ connecting'], closed: ['bad', '● reconnecting'], denied: ['bad', '● no key'] }[S.conn];
  const mode = s?.mode;
  return html`<div class="footer"><div class="segs">
    ${mode && html`<span class=${'seg mode ' + mode}><button title="permission mode: Shift+Tab cycles manual, accept edits, auto" onClick=${cycleMode}>${modeNames[mode] || mode} <span class="hint">(shift+tab to cycle)</span></button></span>`}
    <span class="seg"><button onClick=${() => set({ help: !S.help })}>? for shortcuts</button></span>
    <span class=${'seg conn ' + conn[0]}>${conn[1]}</span>
    ${right.map(x => html`<span class="seg">${x}</span>`)}
  </div></div>`;
}

// Limits are the plan's usage, as Claude Code's status line shows it: the
// five-hour and seven-day windows, and any other in use (a model's, or
// credits).
const windowNames = { five_hour: '5h', seven_day: '7d', seven_day_opus: '7d Opus', seven_day_sonnet: '7d Sonnet' };
function Limits({ windows }) {
  const name = w => windowNames[w.name] || w.name.replace(/_/g, ' ');
  const resets = w => w.resets ? new Date(w.resets * 1000).toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '';
  const tip = 'your plan\'s usage\n' + windows.map(w => `${name(w)}: ${Math.round(w.used)}%${w.limit ? ` ($${w.spent} of $${w.limit})` : ''}${w.resets ? ', resets ' + resets(w) : ''}`).join('\n');
  return html`<span title=${tip}>${windows.map((w, j) => html`${j > 0 && ' · '}<span class=${w.used >= 95 ? 'conn bad' : w.used >= 80 ? 'warnt' : ''}>${name(w)} ${Math.round(w.used)}%</span>`)}</span>`;
}

function Help() {
  const rows = [
    ['Enter', 'send; while Opt works, queue'],
    ['Shift+Enter', 'new line'],
    ['⌘/Ctrl+Enter', 'send now: stop the turn and send'],
    ['Esc', 'stop the turn; queued messages come back here'],
    ['↑ / ↓', 'your earlier messages; ↑ on an empty input edits the last queued'],
    ['drop · paste · 📎', 'attach files, each where the caret is; a path you type (/… or ~/…) attaches its file'],
    ['@', 'find a file or folder by its path or name: Tab or Enter takes it'],
    ['Alt+P · /model', 'choose the model, ←/→ its effort'],
    ['Shift+Tab', 'permission mode: manual, accept edits, auto'],
    ['/mcp · /skills · /plugins', 'your Claude Code MCP servers, skills and plugins: on or off'],
    ['Alt+A · /agents', 'the subagents: what each does now, and its steps'],
    ['Alt+K', 'record a voice message; Alt+K or Enter sends it with the input, Esc drops it; type meanwhile, Shift+Enter breaks the line'],
    ['/help', 'the commands'],
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
    let d = '';
    try { d = localStorage.getItem('draft') || ''; } catch {}
    if (d && !inputEl.value) setInput(histText(d)); // as ↑ recalls: its attachments stayed behind
  }, []);
  const sugg = inputEl ? suggestions(inputEl) : [];
  return html`
    <${Mention}/>
    ${sugg.length > 0 && html`<${Cmds} sugg=${sugg}/>`}
    <div class="box">
      <${Chips}/>
      <span class="gt" aria-hidden="true">▸</span>
      <textarea ref=${ref} rows="1" spellcheck="false" aria-label="Message Opt" placeholder=${S.turn.phase ? 'Queue a message for Opt…' : 'Message Opt'}
        onInput=${() => { histPos = -1; keepDraft(); if (S.cmdSel || S.cmdShut !== null) set({ cmdSel: 0, cmdShut: null }); autosize(); placesGone(); lookForPaths(); lookForMention(); tick(n => n + 1); }}
        onKeyUp=${e => { if (e.key.startsWith('Arrow') || e.key === 'Home' || e.key === 'End') { lookForMention(); tick(n => n + 1); } }}
        onClick=${() => { lookForMention(); tick(n => n + 1); }}
        onBlur=${() => S.mention && set({ mention: null })}
        onKeyDown=${onKey}
        onCompositionEnd=${e => { composedAt = e.timeStamp; }}
        onPaste=${onPaste}/>
      <span class="tools">
        <button title="record a voice message (Alt+K)" onClick=${voiceKey}>🎙</button>
        <button title="attach files" onClick=${() => pick.current.click()}>📎</button>
        <input type="file" multiple hidden ref=${pick} onChange=${e => { addFiles([...e.target.files]); e.target.value = ''; inputEl.focus(); }}/>
      </span>
    </div>`;
}

// ---------------------------------------------------------------- voice messages

// Alt+K records a voice message; Alt+K or Enter sends it, with what the
// input holds (its text, a screenshot), Esc drops it. The input stays
// open meanwhile, Shift+Enter breaking its lines. opt-chat has its
// words transcribed (Whisper, on Groq): the chat plays the audio, Opt reads
// the words.
// S.voice is where it is: starting, rec or sending.
const VOICE_MAX = 10 * 60e3; // ms: a longer one is sent at ten minutes
const AUDIO = /\.(webm|ogg|oga|opus|m4a|mp4|mp3|wav|flac)$/i;
let rec = null; // the recording underway

function clock(ms) {
  const s = Math.floor(ms / 1000);
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`;
}

function voiceKey() {
  if (S.voice === 'rec') stopVoice(true);
  else if (!S.voice) startVoice();
}

async function startVoice() {
  if (S.conn !== 'open') { notice('warn', 'Not connected: a voice message waits until opt-chat is back.'); return; }
  set({ voice: 'starting' });
  let stream;
  try {
    const on = await (await fetch('/voice')).json();
    if (!on.on) throw new Error(on.why);
    if (!navigator.mediaDevices?.getUserMedia || !window.MediaRecorder) throw new Error('This browser can\'t record here.');
    stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  } catch (err) {
    set({ voice: null });
    notice('warn', err.name === 'NotAllowedError' ? 'No microphone: the browser was told no. Allow it for this page in the site settings, then Alt+K again.'
      : err.name === 'NotFoundError' ? 'No microphone found.' : err.message);
    return;
  }
  if (S.voice !== 'starting') { stream.getTracks().forEach(t => t.stop()); return; } // dropped meanwhile
  const type = ['audio/webm;codecs=opus', 'audio/ogg;codecs=opus', 'audio/mp4'].find(t => MediaRecorder.isTypeSupported(t));
  const recorder = new MediaRecorder(stream, type ? { mimeType: type } : {});
  const chunks = [];
  recorder.ondataavailable = e => e.data.size && chunks.push(e.data);
  const ctx = new AudioContext();
  const analyser = ctx.createAnalyser();
  analyser.fftSize = 2048;
  ctx.createMediaStreamSource(stream).connect(analyser);
  const now = performance.now();
  rec = { stream, recorder, chunks, ctx, analyser, since: now, loud: now, levels: [] };
  rec.cap = setTimeout(() => stopVoice(true), VOICE_MAX);
  recorder.start(1000);
  set({ voice: 'rec' });
}

// stopVoice ends the recording, and sends it if keep: now, past the queue,
// as Cmd+Enter sends.
async function stopVoice(keep, now) {
  const r = rec;
  rec = null;
  if (!r) { if (S.voice === 'starting') set({ voice: null }); return; }
  clearTimeout(r.cap);
  const ms = performance.now() - r.since;
  // A recorder whose microphone went away (unplugged) has stopped itself.
  const stopped = r.recorder.state === 'inactive' ? null : new Promise(res => { r.recorder.onstop = res; });
  if (stopped) r.recorder.stop();
  r.stream.getTracks().forEach(t => t.stop()); // the browser's recording light goes off
  r.ctx.close();
  if (!keep) { set({ voice: null }); return; }
  if (ms < 700) { set({ voice: null }); notice('info', 'Too short: nothing sent.'); return; }
  set({ voice: 'sending' });
  await stopped;
  const type = r.recorder.mimeType || 'audio/webm';
  const name = `voice ${clock(ms)}.${/mp4/.test(type) ? 'm4a' : /ogg/.test(type) ? 'ogg' : 'webm'}`;
  let at;
  try {
    const res = await fetch('/voice', { method: 'POST', headers: { 'X-Name': encodeURIComponent(name) }, body: new Blob(r.chunks, { type }) });
    if (!res.ok) throw new Error((await res.text()).trim() || `opt-chat answered ${res.status}`);
    at = await res.json();
  } catch (err) {
    set({ voice: null });
    notice('warn', `Voice message not sent: ${err.message}`);
    return;
  }
  // Into the input, after what it holds, and sent with it.
  set({ voice: null, atts: S.atts.concat({ name: at.name, orig: at.orig, kind: at.kind, size: at.size, id: ++uploadSeq, status: 'done', promise: Promise.resolve() }) });
  if (at.error) notice('warn', `Voice message not transcribed (${at.error}): it is in the input as a file, to send as one or take off (×).`);
  else submit(now);
}

// paintLevels draws the microphone's levels, the latest on the right, and
// a dotted line where the recording goes on.
function paintLevels(c, levels) {
  const dpr = devicePixelRatio || 1, w = c.clientWidth, h = c.clientHeight;
  if (c.width !== Math.round(w * dpr) || c.height !== Math.round(h * dpr)) { c.width = Math.round(w * dpr); c.height = Math.round(h * dpr); }
  const g = c.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);
  g.fillStyle = getComputedStyle(c).color;
  const step = 4, n = Math.min(levels.length, Math.floor(w / step));
  for (let i = 0; i < n; i++) {
    const bar = Math.max(1, Math.min(h, Math.sqrt(levels[levels.length - 1 - i]) * h * 1.8));
    g.fillRect(w - (i + 1) * step, (h - bar) / 2, 2, bar);
  }
  for (let x = w - (n + 1) * step; x >= 0; x -= step) g.fillRect(x, h / 2 - .5, 2, 1);
}

// Recorder is the recording underway: how long, the microphone's level as
// it goes, and its keys.
function Recorder() {
  const canvas = useRef(), timer = useRef(), quiet = useRef();
  useEffect(() => {
    let raf, last = 0;
    const buf = new Float32Array(2048);
    const draw = now => {
      raf = requestAnimationFrame(draw);
      const r = rec;
      if (!r || !canvas.current || now - last < 50) return; // 20 levels a second
      last = now;
      r.analyser.getFloatTimeDomainData(buf);
      let sum = 0;
      for (const v of buf) sum += v * v;
      const level = Math.sqrt(sum / buf.length);
      r.levels.push(level);
      if (r.levels.length > 2000) r.levels.splice(0, 1000);
      if (level > .004) r.loud = now;
      timer.current.textContent = clock(now - r.since);
      quiet.current.hidden = now - r.loud < 2500;
      paintLevels(canvas.current, r.levels);
    };
    raf = requestAnimationFrame(draw);
    return () => cancelAnimationFrame(raf);
  }, []);
  const v = S.voice;
  return html`<div class=${'rec ' + v} role="status">
    <span class="dot" aria-hidden="true">●</span>
    <span class="time" ref=${timer}>0:00</span>
    <canvas ref=${canvas} aria-label="the microphone's level"/>
    <span class="keys">${v === 'sending' ? 'transcribing…' : v === 'starting' ? 'waiting for the microphone…' : html`
      <button onClick=${() => stopVoice(true)}><kbd>Alt+K</kbd> send</button>
      <button onClick=${() => stopVoice(false)}><kbd>Esc</kbd> cancel</button>`}</span>
    <div class="quiet" ref=${quiet} hidden>No sound comes in: is the right microphone on and unmuted?</div>
  </div>`;
}

// Voice is a voice message in the chat: it plays, and ▾ shows its words.
function Voice({ mk, k }) {
  const audio = useRef();
  const [open, setOpen] = useKept(k, false);
  const [at, setAt] = useState(0);
  const [playing, setPlaying] = useState(false);
  const len = mk.name.match(/(\d+):(\d\d)/);
  const total = len ? +len[1] * 60 + +len[2] : 0; // a recording's own length can be unknown until played through
  const toggle = () => { const a = audio.current; if (a.paused) a.play().catch(e => notice('warn', `Can't play it: ${e.message}`)); else a.pause(); };
  return html`<div class="voice">
    <div class="player">
      <button class="play" onClick=${toggle} title=${playing ? 'pause' : 'play'} aria-label=${playing ? 'pause' : 'play the voice message'}>${playing ? '❚❚' : '▶'}</button>
      <progress max=${total || 1} value=${Math.min(at, total || 1)}/>
      <span class="len">${playing || at ? clock(at * 1000) + ' / ' : ''}${len ? len[0] : ''}</span>
      <button class="fold" onClick=${() => setOpen(!open)} title=${open ? 'hide its words' : 'show its words'} aria-expanded=${open}>${open ? '▴' : '▾'}</button>
      <audio ref=${audio} src=${fileURL(mk.path)} preload="none"
        onTimeUpdate=${e => setAt(e.target.currentTime)} onPlay=${() => setPlaying(true)} onPause=${() => setPlaying(false)}
        onEnded=${() => { setPlaying(false); setAt(0); }}/>
    </div>
    ${open && html`<div class="words">${mk.content}</div>`}
  </div>`;
}

const isVoice = mk => mk.content !== undefined && AUDIO.test(mk.path);

let toEnd = false; // scroll to the end at the next render

// ---------------------------------------------------------------- agents rail

// The agents rail: in the free space right of the chat, this run's
// subagents, what each does now, and, one opened, its steps as they come,
// from its log. Where there is no room, it is a drawer: the bar's agents, or
// Alt+A, open it.
let chPx = 0; // a character's width
function roomy() {
  if (!chPx) {
    const p = document.body.appendChild(document.createElement('span'));
    p.style.cssText = 'position:absolute;visibility:hidden;white-space:pre';
    p.textContent = '0'.repeat(100);
    chPx = p.getBoundingClientRect().width / 100 || 8;
    p.remove();
  }
  return innerWidth >= 178 * chPx; // the column's 132ch, and 46 for the rail
}
addEventListener('resize', () => { if (roomy() !== S.wide) set({ wide: roomy(), drawer: false }); });

// A rail the user hid stays hidden until a subagent it didn't have then starts.
function railShown() {
  const subs = S.status?.subs || [];
  return S.wide ? !!subs.length && (!S.railOff || subs.some(s => !S.railOff.includes(s.name))) : S.drawer;
}

function toggleRail(name) {
  if (name) openAgent(name);
  if (S.wide) {
    const off = !name && railShown() ? (S.status?.subs || []).map(s => s.name) : null;
    try { localStorage.setItem('railOff', off ? JSON.stringify(off) : ''); } catch {}
    set({ railOff: off });
  } else set({ drawer: !!name || !S.drawer });
}

function openAgent(name) {
  set({ agent: { name, steps: [], off: 0, cut: false, loading: true } });
  send({ t: 'steps', name });
}

function closeAgent() { set({ agent: null }); }

const agentStepsMax = 3000; // the oldest go, as the chat's own messages do

function addStep(name, s) {
  const a = S.agent;
  if (a?.name !== name || a.loading) return; // a step that came before its log is in the log
  const steps = a.steps.concat([s]);
  const cut = steps.length > agentStepsMax;
  set({ agent: { ...a, steps: cut ? steps.slice(-agentStepsMax) : steps, off: a.off + (cut ? 1 : 0), cut: a.cut || cut } });
}

// what is one sub's state, as the rail says it: a question waiting on the
// user comes first.
function subState(s, now) {
  if (S.asks.some(q => q.who === s.name)) return ['ask', 'waits for your answer'];
  switch (s.state) {
    case 'working': return ['work', 'working · ' + elapsed(now - s.started)];
    case 'starting': return ['work', 'starting'];
    case 'compacting': return ['rest', 'compacting at rest'];
    case 'resting': return ['rest', s.until > now ? `reported · rests ${Math.ceil((s.until - now) / 60000)}m more` : 'reported'];
    case 'failed': return ['bad', 'failed'];
    case 'stopping': return ['stop', 'stopping'];
    case 'halted': return ['stop', 'stopped by you · a tell resumes it'];
    case 'stopped': return ['stop', 'stopped by a restart · a tell resumes it'];
    default: return ['done', 'ended'];
  }
}

function subMeta(s) {
  return [s.model, s.effort, s.context && `${k(s.context)} context${s.window ? ` of ${k(s.window)} before it compacts` : ''}`, s.steps && count(s.steps, 'step')].filter(Boolean).join(' · ');
}

// useClock re-renders each second while on.
function useClock(on) {
  const [, tick] = useState(0);
  useEffect(() => {
    if (!on) return;
    const t = setInterval(() => tick(n => n + 1), 1000);
    return () => clearInterval(t);
  }, [on]);
  return Date.now();
}

// The rail's width is the user's to drag, by its left edge; kept per browser.
// --rail-w, set on the root element, overrides the stylesheet's width, which
// still caps it to the window (style.css).
function railWidth(px) {
  const root = document.documentElement.style;
  if (px) root.setProperty('--rail-w', Math.round(px) + 'px');
  else root.removeProperty('--rail-w');
}
try { railWidth(+localStorage.getItem('railW')); } catch {}
function keepRailWidth() {
  const w = parseFloat(document.documentElement.style.getPropertyValue('--rail-w'));
  try { if (w) localStorage.setItem('railW', w); else localStorage.removeItem('railW'); } catch {}
}
// fitRail sets the width the user asks for, as far as the window has room:
// beside the chat, its 40 columns stay; as a drawer, it covers 94% at most.
function fitRail(px) {
  const most = S.wide ? innerWidth - 40 * chPx : 0.94 * innerWidth;
  railWidth(Math.max(Math.min(30 * chPx, most), Math.min(px, most)));
}

function Grip() {
  const drag = useRef(null); // the pointer dragging it
  const end = () => {
    drag.current = null;
    document.documentElement.classList.remove('resizing');
  };
  useEffect(() => end, []); // gone mid-drag (Esc closed the drawer): no cursor left behind
  const down = e => {
    if (e.button || drag.current != null) return;
    e.preventDefault();
    drag.current = e.pointerId;
    e.currentTarget.setPointerCapture(e.pointerId);
    document.documentElement.classList.add('resizing');
  };
  const move = e => { if (e.pointerId === drag.current) fitRail(innerWidth - e.clientX); };
  const up = e => {
    if (e.pointerId !== drag.current) return;
    end();
    keepRailWidth();
  };
  const key = e => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    fitRail(e.currentTarget.parentElement.getBoundingClientRect().width + (e.key === 'ArrowLeft' ? 4 : -4) * chPx);
    keepRailWidth();
  };
  return html`<div class="grip" role="separator" aria-orientation="vertical" aria-label="the rail's width" tabindex="0"
    title="drag to widen or narrow; a double-click sets it back" onPointerDown=${down} onPointerMove=${move}
    onPointerUp=${up} onPointerCancel=${up} onKeyDown=${key} onDblClick=${() => { railWidth(0); keepRailWidth(); }}/>`;
}

function Rail() {
  const subs = S.status?.subs || [];
  const now = useClock(subs.some(s => s.state === 'working' || s.state === 'resting'));
  const open = S.agent && subs.find(s => s.name === S.agent.name);
  const working = subs.filter(s => s.state === 'working' || s.state === 'starting').length;
  return html`<aside class=${'rail' + (S.wide ? '' : ' drawer')} aria-label="subagents">
    <${Grip}/>
    ${S.agent
      ? html`<${AgentView} a=${S.agent} s=${open} now=${now}/>`
      : html`<div class="rail-head">
          <span class="title">Agents</span>
          <span class="dim">${working ? `${working} at work` : subs.length ? 'none at work' : ''}</span>
          <button class="x" title=${S.wide ? 'hide until a new subagent starts (Alt+A or the bar\'s agents show it again)' : 'close (Esc)'} onClick=${() => toggleRail()}>×</button>
        </div>
        <div class="cards">
          ${subs.map(s => html`<${AgentCard} key=${s.name} s=${s} now=${now}/>`)}
          ${!subs.length && html`<div class="empty">No subagent has run yet. When Opt starts one, it shows here.</div>`}
        </div>`}
  </aside>`;
}

function AgentCard({ s, now }) {
  const [cls, label] = subState(s, now);
  const ask = S.asks.find(q => q.who === s.name);
  const step = ask ? ask.tool + ' ' + JSON.stringify(ask.input ?? '') : s.step;
  const tool = step && parseTool(step);
  const open = e => { if (e.type === 'click' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openAgent(s.name); } };
  return html`<div class=${'card ' + cls} role="button" tabindex="0" onClick=${open} onKeyDown=${e => e.target === e.currentTarget && open(e)} title="open its steps">
    <div class="top"><span class="dot" aria-hidden="true">●</span><span class="name">[${s.name}]</span><span class="state">${label}</span><${SubAction} s=${s}/></div>
    <div class="meta">${subMeta(s)}</div>
    ${tool && cls !== 'done' && html`<div class="now"><span class="el" aria-hidden="true">⎿</span><span class="tname">${tool.name}</span><span class="args">(${toolArgs(tool.name, tool.input, tool.raw)})</span>${cls === 'work' && s.stepAt ? html`<span class="dim">· ${elapsed(now - s.stepAt)}</span>` : ''}</div>`}
  </div>`;
}

// SubAction stops a subagent whose call is underway, at work or at rest; one
// stopped or ended, it clears from the rail: press it once to stop, again
// to clear, as in Claude Code. Its log stays, for zoom.
function SubAction({ s }) {
  if (s.state === 'stopping') return null;
  const live = ['starting', 'working', 'compacting', 'resting'].includes(s.state);
  const act = e => {
    e.stopPropagation();
    send({ t: live ? 'stopAgent' : 'clearAgent', name: s.name });
    if (!live && S.agent?.name === s.name) closeAgent();
  };
  return html`<button class=${'act' + (live ? ' stop' : '')} onClick=${act} onKeyDown=${e => e.stopPropagation()}
    title=${live ? (s.state === 'resting' ? 'end its call: it has reported' : 'stop it now') : 'clear it from this list (its log stays)'}>${live ? 'stop' : 'clear'}</button>`;
}

// stepMsgs are a subagent's steps as the chat's messages: its task and Opt's
// tells as the user's, its text as Opt's, its tool calls and their output.
function stepMsgs(name, steps, off) {
  const out = [];
  steps.forEach((s, j) => {
    const i = `${name}:${off + j}`;
    switch (s.kind) {
      case 'task': case 'opt': out.push({ kind: 'user', text: s.text, i }); break;
      case 'loop': out.push({ kind: 'user', text: 'Stop hook: ' + s.text, i }); break;
      case 'agent': out.push({ kind: 'opt', text: s.text, i }); break;
      case 'tool': case 'echo': out.push({ kind: s.kind, text: s.text, i }); break;
      case 'compacted': out.push({ kind: 'note', text: 'Its context was compacted at rest: ' + s.text.replace(/(\d+) tokens to (\d+)/, (_, x, y) => `${k(+x)} tokens to ${k(+y)}`), i }); break;
    }
  });
  return out;
}

function AgentView({ a, s, now }) {
  const ref = useRef(), stick = useRef(true);
  const busy = s && (s.state === 'working' || s.state === 'starting');
  const list = useMemo(() => items(stepMsgs(a.name, a.steps, a.off)).map(it => ({ ...it, state: it.m.kind !== 'tool' ? '' : it.echo ? 'done' : busy ? 'run' : 'cut' })), [a.steps, busy]);
  const toBottom = () => { const el = ref.current; if (el) el.scrollTop = el.scrollHeight; };
  const onResize = useMemo(() => () => { if (stick.current) toBottom(); }, []);
  useLayoutEffect(() => { if (stick.current) toBottom(); });
  useEffect(() => { stick.current = true; toBottom(); }, [a.name]);
  const [cls, label] = s ? subState(s, now) : ['done', 'not in this run'];
  return html`<div class="rail-head">
      <button class="back" title="all agents (Esc)" onClick=${closeAgent}>←</button>
      <span class=${'agent-name ' + cls}><span class="dot" aria-hidden="true">●</span>[${a.name}]</span>
      <span class=${'state ' + cls}>${label}</span>
      ${s && html`<${SubAction} s=${s}/>`}
      <button class="x" title=${S.wide ? 'hide until a new subagent starts (Alt+A or the bar\'s agents show it again)' : 'close (Esc)'} onClick=${() => toggleRail()}>×</button>
    </div>
    ${s && html`<div class="rail-meta">${subMeta(s)}</div>`}
    <div class="rail-log" ref=${ref} onScroll=${() => { const el = ref.current; stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 60; }}>
      ${a.cut && html`<div class="edge">Its earlier steps: zoom("${a.name}") in the chat, or agents/${a.name}.log</div>`}
      ${a.loading ? html`<div class="edge">Loading its steps…</div>` : !list.length && html`<div class="edge">No steps yet.</div>`}
      <${Virtualizer} data=${list} scrollRef=${ref} bufferSize=${800} onResize=${onResize}>${row}</${Virtualizer}>
    </div>`;
}

function Scroller() {
  const ref = useRef(), inner = useRef(), edge = useRef();
  const stick = useRef(true), was = useRef({ first: -1, last: -1 });
  const [newer, setNewer] = useState(false);
  const [margin, setMargin] = useState(0); // where the rows start: below the edge
  const toBottom = () => { const el = ref.current; if (el) el.scrollTop = el.scrollHeight; };
  const first = S.msgs.length ? S.msgs[0].i : -1, last = lastI;
  const shift = was.current.first >= 0 && first >= 0 && first !== was.current.first; // older came in above, or the oldest went
  const list = useMemo(() => rows(S.msgs, S.turn.phase, S.turn.from || 0, S.notices, S.asides), [S.msgs, S.turn.phase, S.turn.from, S.notices, S.asides]);
  const onResize = useMemo(() => () => { if (stick.current) toBottom(); }, []);
  useLayoutEffect(() => {
    const el = ref.current;
    if (toEnd) { toEnd = false; stick.current = atEnd = true; setNewer(false); toBottom(); }
    else if (stick.current) toBottom();
    else if (last > was.current.last && was.current.last >= 0) setNewer(true);
    was.current = { first, last };
  });
  useEffect(() => {
    const ro = new ResizeObserver(() => {
      if (stick.current) toBottom();
      const e = edge.current, el = ref.current;
      setMargin(Math.round(e.getBoundingClientRect().bottom - el.getBoundingClientRect().top + el.scrollTop));
    });
    ro.observe(inner.current);
    ro.observe(ref.current); // the input growing makes the log shorter
    return () => ro.disconnect();
  }, []);
  const onScroll = () => {
    const el = ref.current;
    stick.current = atEnd = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
    if (stick.current) setNewer(false);
    if (el.scrollTop < 600) loadOlder();
  };
  return html`<div class="scroll" ref=${ref} onScroll=${onScroll}>
    <div class="column log" ref=${inner} role="log" aria-live="polite" aria-busy=${!!S.live}>
      <div class="edge" ref=${edge}>${S.conn === 'connecting' ? 'Connecting…' : first > 0 ? (S.older ? 'Loading older messages…' : '↑ older messages') : S.msgs.length ? 'The beginning of the chat' : 'An empty chat: say something.'}</div>
      <${Log} list=${list} shift=${shift} margin=${margin} scroller=${ref} onResize=${onResize}/>
      ${S.thinking && (S.turn.phase === 'thinking') && html`<div class="thinking"><div>${S.thinking}</div></div>`}
      ${S.input && S.turn.phase === 'tool' && S.turn.tool && html`<${LiveTool} name=${S.turn.tool} raw=${S.input}/>`}
      ${S.live && html`<div class="msg opt live"><span class="mark" aria-hidden="true">●</span><div class="body"><${Markdown} text=${S.live} live=${true}/></div></div>`}
    </div>
    ${newer && html`<div class="newer"><button onClick=${() => { stick.current = atEnd = true; setNewer(false); toBottom(); }}>↓ new messages</button></div>`}
  </div>`;
}

function App() {
  useStore();
  useEffect(() => {
    document.title = (S.asks.length ? '? ' : S.turn.phase ? '✻ ' : S.unseen ? '● ' : '') + 'opt-chat'; // ?: waiting on the user's yes or no
  });
  useEffect(() => { set({ wide: roomy() }); }, []);
  return html`<div class="shell"><div class="main">
    ${S.conn === 'denied' && html`<div class="banner">This page has no key to the chat: open the address opt-chat printed in the terminal.</div>`}
    ${S.conn === 'closed' && html`<div class="banner">Lost opt-chat: reconnecting…</div>`}
    <${Scroller}/>
    <div class="dock"><div class="column">
      ${S.turn.phase && html`<${Spinner} turn=${S.turn} chars=${S.turnChars} queued=${S.queue.length > 0}/>`}
      <${Queue} queue=${S.queue}/>
      ${S.asks.length > 0 && !S.picker && html`<${Ask} q=${S.asks[0]}/>`}
      ${S.picker && html`<${Picker}/>`}
      ${S.panel && !S.picker && html`<${Panel}/>`}
      ${S.voice && html`<${Recorder}/>`}
      <${Input}/>
      ${S.help && html`<${Help}/>`}
      <${Footer}/>
    </div></div>
    </div>
    ${railShown() && !S.wide && html`<div class="scrim" onClick=${() => set({ drawer: false })}/>`}
    ${railShown() && html`<${Rail}/>`}
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
  if (e.isComposing || e.keyCode === 229) return; // the keys that pick an input method's candidates are its own
  if (e.target === inputEl && e.key.length === 1) typedAt = performance.now();
  if (e.key === 'Tab' && !e.shiftKey && e.target === inputEl && S.asks.length && !S.picker && !S.mention?.items.length && !suggestions(inputEl).length) {
    e.preventDefault(); // to the question
    document.querySelector('.ask')?.focus();
    return;
  }
  if (e.altKey && e.code === 'KeyK' && !e.metaKey && !e.ctrlKey) { // a voice message
    e.preventDefault();
    e.stopPropagation();
    voiceKey();
    return;
  }
  // Enter and Esc are the recording's, not the input's; Shift+Enter still
  // breaks the input's line, typed as you speak, and a menu open in it (/,
  // @, the model picker) takes its keys as ever.
  const menu = S.picker || e.target === inputEl && (S.mention?.items.length || suggestions(inputEl).length);
  if (S.voice && !menu && (e.key === 'Enter' && !e.shiftKey && !e.altKey || e.key === 'Escape')) {
    e.preventDefault();
    e.stopPropagation();
    if (S.voice !== 'sending') stopVoice(e.key === 'Enter', e.metaKey || e.ctrlKey);
    return;
  }
  if (e.altKey && e.code === 'KeyA' && !e.metaKey && !e.ctrlKey) { // the agents rail
    e.preventDefault();
    e.stopPropagation();
    toggleRail();
    return;
  }
  if (e.key === 'Escape' && (e.target.closest?.('.rail') || (S.drawer && e.target !== inputEl))) {
    e.preventDefault();
    if (S.agent) closeAgent(); else if (S.drawer) set({ drawer: false });
    return;
  }
  if (e.altKey && e.code === 'KeyP' && !e.metaKey && !e.ctrlKey) { // Option+P types π on a Mac: the code is the key
    e.preventDefault();
    e.stopPropagation();
    if (S.picker) closePicker(); else openPicker();
    return;
  }
  const mine = e.target === inputEl || e.target === document.body || e.target.closest?.('.ask, .panel'); // not another field's
  if (e.key === 'Tab' && e.shiftKey && mine && !S.picker && !e.metaKey && !e.ctrlKey && !e.altKey) {
    e.preventDefault();
    e.stopPropagation();
    cycleMode();
    return;
  }
  pickerKey(e) || panelKey(e) || mentionKey(e) || askKey(e);
}, true); // before the input: while the picker or a question is up, the keys are theirs
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
// A copy keeps a reply's list numbers and bullets, as a copy from the
// terminal does: the browser leaves them out, since it draws them. Each list
// item the copy starts before gets its marker as text, in a copy of the
// selection laid out where the selection is, so it reads as the page does.
const indent = '\uE000'; // a nested item's, which innerText would trim if a space
document.addEventListener('copy', e => {
  const sel = getSelection();
  if (!sel.rangeCount || sel.isCollapsed || e.target.closest?.('textarea, input')) return;
  const r = sel.getRangeAt(0);
  const items = [...document.querySelectorAll('.md li')].filter(li => r.comparePoint(li, 0) === 0);
  if (!items.length) return;
  for (const li of items) {
    const list = li.parentElement;
    li.dataset.mark = indent.repeat([...ancestors(li, 'li')].length) +
      (list.tagName === 'OL' ? `${list.start + [...list.children].indexOf(li)}. ` : '- ');
  }
  const box = document.createElement('div');
  box.style.cssText = 'position:absolute;left:-99999px;top:0';
  box.append(r.cloneContents());
  for (const li of items) delete li.dataset.mark;
  const at = r.commonAncestorContainer;
  (at.nodeType === 1 ? at : at.parentElement).append(box);
  for (const el of box.querySelectorAll('*')) if (getComputedStyle(el).userSelect === 'none') el.remove();
  const marked = [...box.querySelectorAll('li[data-mark]')].map(li => [li, li.dataset.mark]);
  for (const [li] of marked) delete li.dataset.mark;
  const page = box.innerHTML;
  for (const [li, mark] of marked) { // in a loose list's first paragraph, not on a line of its own
    const p = li.firstChild?.nodeName === 'P' ? li.firstChild : li;
    p.prepend(mark);
  }
  const text = box.innerText.replaceAll(indent, '  ');
  box.remove();
  e.clipboardData.setData('text/plain', text);
  e.clipboardData.setData('text/html', page);
  e.preventDefault();
});
// ancestors yields el's ancestors that match sel, nearest first.
function* ancestors(el, sel) {
  for (let a = el.parentElement?.closest(sel); a; a = a.parentElement?.closest(sel)) yield a;
}

render(html`<${App}/>`, document.getElementById('app'));
connect();
