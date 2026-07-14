#!/usr/bin/env python3
"""
Flint Dashboard — multi-process daemon monitor (reads flint.db).

The daemon (executor/reconciler/risk_monitor + producer loops) is now the
single source of truth, backed by SQLite `flint.db`. This dashboard is a pure
READ-ONLY view over that db (role="reader" — never writes, never places orders).

Tabs:
    POSITIONS  — open positions + risk_state header (equity, day P&L, open_risk, HALT)
    TRADES     — recent trades with provenance (source: technical/event/user) + pnl
    INTENTS    — intent queue & history: pending/approved/rejected/filled + reject_reason
    PROCESSES  — supervisor.check() health + time-anchor from reflect.recall()
    MEMORY     — reflect.recall(): active lessons + non-expired plans + agg win-rate matrix
    LOGS       — legacy per-cycle log files (logs/*.json), kept for history

Backward tolerance: if flint.db is missing or a table is empty, each panel
renders "no data" rather than crashing.

Usage:
    python3 dashboard/server.py
    open http://localhost:8383
"""

import json
import sys
import http.server
import urllib.parse
from datetime import datetime, timezone, timedelta
from pathlib import Path

FLINT_DIR = Path(__file__).resolve().parent.parent
LOGS_DIR = FLINT_DIR / "logs"
PORT = 8383

# Make the `agent` package importable when run as `python3 dashboard/server.py`.
if str(FLINT_DIR) not in sys.path:
    sys.path.insert(0, str(FLINT_DIR))


# ─────────────────────────────────────────────────────────────────────────
# db access — all reads go through DB(role="reader"); never writes.
# Every accessor is defensive: a missing db or empty table yields [] / None
# rather than an exception, so the dashboard degrades gracefully.
# ─────────────────────────────────────────────────────────────────────────
def _db_available():
    try:
        from agent.db import DB_PATH
        return Path(DB_PATH).exists()
    except Exception:
        return False


def _reader():
    """Open a read-only DB handle, or None if unavailable."""
    try:
        from agent.db import DB
        return DB(role="reader")
    except Exception:
        return None


def _rows(method, *args):
    """Call a DB accessor by name, return list[dict]; [] on any failure."""
    db = _reader()
    if db is None:
        return []
    try:
        res = getattr(db, method)(*args)
        return [dict(r) for r in res] if res else []
    except Exception:
        return []
    finally:
        try:
            db.close()
        except Exception:
            pass


def _query(sql, params=()):
    """Run an arbitrary read SQL, return list[dict]; [] on any failure."""
    db = _reader()
    if db is None:
        return []
    try:
        return [dict(r) for r in db.conn.execute(sql, params).fetchall()]
    except Exception:
        return []
    finally:
        try:
            db.close()
        except Exception:
            pass


def get_risk():
    """risk_state single row as dict, or None."""
    db = _reader()
    if db is None:
        return None
    try:
        r = db.get_risk()
        return dict(r) if r else None
    except Exception:
        return None
    finally:
        try:
            db.close()
        except Exception:
            pass


def get_open_positions():
    return _rows("open_positions")


def get_recent_trades(limit=100):
    return _rows("recent_trades", limit)


def get_intents(limit=200):
    return _query(
        "SELECT id,source,priority,symbol,side,hypothesis_qty,confidence,status,"
        "reason,reject_reason,created_at,decided_at,expires_at "
        "FROM intents ORDER BY id DESC LIMIT ?",
        (limit,),
    )


def get_processes():
    """supervisor.check() health rows; [] if module/db unavailable."""
    try:
        from agent.supervisor import check
        return list(check())
    except Exception:
        return []


def get_recall():
    """reflect.recall() memory view; None if unavailable."""
    try:
        from agent.reflect import recall
        return recall()
    except Exception:
        return None


def get_agg(limit=200):
    return _query(
        "SELECT symbol,session,setup,rsi_bucket,trips,wins,losses,pl_ratio "
        "FROM agg ORDER BY trips DESC LIMIT ?",
        (limit,),
    )


# ─────────────────────────────────────────────────────────────────────────
# legacy logs (history only — live picture comes from the db)
# ─────────────────────────────────────────────────────────────────────────
def query_logs(limit=50):
    entries = []
    if not LOGS_DIR.exists():
        return entries
    all_files = list(LOGS_DIR.glob("*.json"))
    all_files = [f for f in all_files if not f.name.startswith(("launchd", "dispatch"))]
    all_files.sort(key=lambda f: f.name, reverse=True)
    for logfile in all_files[:limit]:
        try:
            entry = json.loads(logfile.read_text())
            entry["_file"] = logfile.name
            entries.append(entry)
        except Exception:
            entries.append({"_file": logfile.name, "action": "ERROR", "detail": "bad json"})
    return entries


# ─────────────────────────────────────────────────────────────────────────
# styling — preserved verbatim from the Arena dashboard (dark/light themes)
# ─────────────────────────────────────────────────────────────────────────
CSS = """
:root {
  --bg: #111; --bg2: #1a1a1a; --bg3: #0a0a0a; --bg-hover: #1a1a1a;
  --fg: #ccc; --fg2: #888; --fg3: #666; --fg4: #444;
  --fg-bright: #fff; --fg-dim: #555;
  --border: #333; --border2: #222;
  --accent: #f59e0b;
  --green: #22c55e; --red: #ef4444; --blue: #60a5fa; --purple: #a78bfa;
  --badge-buy-bg: #064e3b; --badge-buy-fg: #34d399;
  --badge-sell-bg: #1e3a5f; --badge-sell-fg: #60a5fa;
  --badge-hold-bg: #422006; --badge-hold-fg: #fbbf24;
  --badge-wait-bg: #1f1f1f; --badge-wait-fg: #888;
  --badge-monitor-bg: #2e1065; --badge-monitor-fg: #a78bfa;
  --badge-error-bg: #450a0a; --badge-error-fg: #f87171;
}
[data-theme="light"] {
  --bg: #fafafa; --bg2: #fff; --bg3: #f5f5f5; --bg-hover: #f3f4f6;
  --fg: #1a1a1a; --fg2: #6b7280; --fg3: #9ca3af; --fg4: #d1d5db;
  --fg-bright: #000; --fg-dim: #9ca3af;
  --border: #e5e7eb; --border2: #f3f4f6;
  --accent: #d97706;
  --green: #16a34a; --red: #dc2626; --blue: #2563eb; --purple: #7c3aed;
  --badge-buy-bg: #dcfce7; --badge-buy-fg: #16a34a;
  --badge-sell-bg: #dbeafe; --badge-sell-fg: #2563eb;
  --badge-hold-bg: #fef3c7; --badge-hold-fg: #d97706;
  --badge-wait-bg: #f3f4f6; --badge-wait-fg: #6b7280;
  --badge-monitor-bg: #ede9fe; --badge-monitor-fg: #7c3aed;
  --badge-error-bg: #fee2e2; --badge-error-fg: #dc2626;
}
@media (prefers-color-scheme: light) {
  :root:not([data-theme="dark"]) {
    --bg: #fafafa; --bg2: #fff; --bg3: #f5f5f5; --bg-hover: #f3f4f6;
    --fg: #1a1a1a; --fg2: #6b7280; --fg3: #9ca3af; --fg4: #d1d5db;
    --fg-bright: #000; --fg-dim: #9ca3af;
    --border: #e5e7eb; --border2: #f3f4f6;
    --accent: #d97706;
    --green: #16a34a; --red: #dc2626; --blue: #2563eb; --purple: #7c3aed;
    --badge-buy-bg: #dcfce7; --badge-buy-fg: #16a34a;
    --badge-sell-bg: #dbeafe; --badge-sell-fg: #2563eb;
    --badge-hold-bg: #fef3c7; --badge-hold-fg: #d97706;
    --badge-wait-bg: #f3f4f6; --badge-wait-fg: #6b7280;
    --badge-monitor-bg: #ede9fe; --badge-monitor-fg: #7c3aed;
    --badge-error-bg: #fee2e2; --badge-error-fg: #dc2626;
  }
}

* { margin:0; padding:0; box-sizing:border-box; }
body { font-family: 'SF Mono', 'Fira Code', 'Consolas', monospace; background:var(--bg); color:var(--fg); font-size:13px; transition: background 0.2s, color 0.2s; }
a { color:var(--blue); text-decoration:none; }
.topnav { background:var(--bg3); padding:10px 20px; display:flex; justify-content:space-between; align-items:center; border-bottom:1px solid var(--border); }
.topnav .brand { color:var(--fg-bright); font-weight:bold; font-size:18px; letter-spacing:2px; }
.topnav .brand span { color:var(--accent); }
.theme-toggle { background:var(--bg2); border:1px solid var(--border); color:var(--fg2); padding:4px 10px; cursor:pointer; font-family:inherit; font-size:11px; border-radius:3px; }
.theme-toggle:hover { color:var(--fg-bright); border-color:var(--accent); }
.tabs { display:flex; gap:0; background:var(--bg2); border-bottom:2px solid var(--border); flex-wrap:wrap; }
.tab { padding:10px 20px; color:var(--fg2); cursor:pointer; border-bottom:2px solid transparent; font-size:12px; text-transform:uppercase; letter-spacing:1px; font-weight:600; }
.tab:hover { color:var(--fg); }
.tab.active { color:var(--accent); border-bottom-color:var(--accent); }
.stats-bar { background:var(--bg3); border-bottom:1px solid var(--border2); padding:12px 20px; display:flex; gap:40px; flex-wrap:wrap; align-items:center; }
.stat .label { color:var(--fg3); font-size:10px; text-transform:uppercase; letter-spacing:1px; }
.stat .value { color:var(--fg-bright); font-size:20px; font-weight:bold; margin-top:2px; }
.stat .value.green { color:var(--green); }
.stat .value.red { color:var(--red); }
.content { padding:16px 20px; }

.halt-banner { background:var(--badge-error-bg); color:var(--badge-error-fg); padding:8px 16px; font-weight:bold; letter-spacing:1px; border-radius:3px; }

/* Action / status badges */
.action-badge { display:inline-block; padding:2px 8px; font-size:11px; font-weight:600; border-radius:3px; }
.action-BUY, .action-LONG { background:var(--badge-buy-bg); color:var(--badge-buy-fg); }
.action-SELL, .action-CLOSE, .action-FLATTEN { background:var(--badge-sell-bg); color:var(--badge-sell-fg); }
.action-SHORT, .action-COVER { background:#4a1942; color:#e879f9; }
.action-HOLD { background:var(--badge-hold-bg); color:var(--badge-hold-fg); }
.action-WAIT { background:var(--badge-wait-bg); color:var(--badge-wait-fg); }
.action-ERROR { background:var(--badge-error-bg); color:var(--badge-error-fg); }

.st-pending { background:var(--badge-hold-bg); color:var(--badge-hold-fg); }
.st-approved { background:var(--badge-buy-bg); color:var(--badge-buy-fg); }
.st-filled { background:var(--badge-sell-bg); color:var(--badge-sell-fg); }
.st-rejected { background:var(--badge-error-bg); color:var(--badge-error-fg); }
.st-expired, .st-cancelled { background:var(--badge-wait-bg); color:var(--fg-dim); }

.src-technical { color:var(--blue); }
.src-event { color:var(--purple); }
.src-user { color:var(--accent); }
.src-risk_monitor { color:var(--red); }

.st-up { color:var(--green); font-weight:bold; }
.st-STALE { color:var(--accent); font-weight:bold; }
.st-MISSING { color:var(--red); font-weight:bold; }

/* Tables */
table { width:100%; border-collapse:collapse; }
th { text-align:left; padding:8px 12px; border-bottom:1px solid var(--border); color:var(--fg3); font-size:10px; text-transform:uppercase; letter-spacing:1px; }
td { padding:8px 12px; border-bottom:1px solid var(--border2); vertical-align:top; }
tr:hover { background:var(--bg-hover); }
.pnl-pos { color:var(--green); }
.pnl-neg { color:var(--red); }
.pnl-zero { color:var(--fg2); }

/* Cards */
.pos-card { background:var(--bg2); border:1px solid var(--border); padding:16px; margin-bottom:8px; display:flex; justify-content:space-between; align-items:center; }
.pos-symbol { font-size:16px; font-weight:bold; color:var(--fg-bright); }
.pos-detail { color:var(--fg2); font-size:12px; margin-top:4px; }

.mem-card { background:var(--bg2); border:1px solid var(--border); padding:12px 16px; margin-bottom:8px; }
.mem-text { color:var(--fg); line-height:1.5; }
.mem-meta { color:var(--fg3); font-size:11px; margin-top:6px; }
.conf-bar { display:inline-block; height:6px; background:var(--accent); border-radius:3px; vertical-align:middle; margin-left:6px; }

.section-head { color:var(--accent); font-size:12px; font-weight:600; letter-spacing:1px; text-transform:uppercase; border-bottom:1px solid var(--border); padding-bottom:6px; margin:16px 0 10px; }
.empty { color:var(--fg3); padding:20px; }
.anchor { background:var(--bg2); border:1px solid var(--border); padding:12px 16px; margin-bottom:12px; display:flex; gap:32px; flex-wrap:wrap; }
.anchor .a-label { color:var(--fg3); font-size:10px; text-transform:uppercase; letter-spacing:1px; }
.anchor .a-value { color:var(--fg-bright); font-size:14px; margin-top:2px; }
.update-info { color:var(--fg3); font-size:11px; text-align:right; margin-bottom:8px; }
.hidden { display:none; }
"""

JS = """
function showTab(tabName) {
    document.querySelectorAll('.tab-content').forEach(el => el.classList.add('hidden'));
    document.querySelectorAll('.tab').forEach(el => el.classList.remove('active'));
    document.getElementById('tab-' + tabName).classList.remove('hidden');
    document.querySelector('[data-tab="' + tabName + '"]').classList.add('active');
    try { localStorage.setItem('flint-tab', tabName); } catch (e) {}
}
function toggleTheme() {
    var root = document.documentElement;
    var current = root.getAttribute('data-theme');
    var next = (current === 'dark') ? 'light' : (current === 'light') ? '' : 'dark';
    if (next) { root.setAttribute('data-theme', next); localStorage.setItem('flint-theme', next); }
    else { root.removeAttribute('data-theme'); localStorage.removeItem('flint-theme'); }
    updateThemeBtn();
}
function updateThemeBtn() {
    var t = document.documentElement.getAttribute('data-theme');
    var btn = document.getElementById('theme-btn');
    if (t === 'dark') btn.textContent = 'DARK';
    else if (t === 'light') btn.textContent = 'LIGHT';
    else btn.textContent = 'AUTO';
}
(function() {
    var saved = localStorage.getItem('flint-theme');
    if (saved) document.documentElement.setAttribute('data-theme', saved);
})();
window.addEventListener('DOMContentLoaded', function() {
    updateThemeBtn();
    var t = localStorage.getItem('flint-tab');
    if (t && document.getElementById('tab-' + t)) showTab(t);
});
"""


def _escape(s):
    if s is None:
        return ""
    return (str(s).replace("&", "&amp;").replace("<", "&lt;")
            .replace(">", "&gt;").replace('"', "&quot;"))


def _num(v, fmt="{:,.2f}", dash="—"):
    try:
        return fmt.format(float(v))
    except (TypeError, ValueError):
        return dash


def _short_ts(ts):
    return (ts or "")[:19].replace("T", " ")


# ─────────────────────────────────────────────────────────────────────────
# tab renderers
# ─────────────────────────────────────────────────────────────────────────
def render_positions(positions, risk):
    # risk header
    if risk is None:
        header = '<div class="empty">No risk_state row — daemon may not be initialized.</div>'
    else:
        equity = risk.get("equity")
        day_start = risk.get("day_start_equity")
        day_pnl = risk.get("day_realized_pnl")
        # derived intraday equity change if both present
        eq_change = None
        if equity is not None and day_start not in (None, 0):
            eq_change = equity - day_start
        halted = bool(risk.get("halt"))
        halt_html = ""
        if halted:
            halt_html = (f'<div class="halt-banner">🛑 HALTED — '
                         f'{_escape(risk.get("halt_reason") or "no reason recorded")}</div>')
        pnl_val = eq_change if eq_change is not None else day_pnl
        pnl_cls = "green" if (pnl_val or 0) >= 0 else "red"
        header = f"""
        {halt_html}
        <div class="stats-bar" style="margin-bottom:12px; border:1px solid var(--border);">
            <div class="stat"><div class="label">Equity</div>
                <div class="value">${_num(equity)}</div></div>
            <div class="stat"><div class="label">Day Start</div>
                <div class="value">${_num(day_start)}</div></div>
            <div class="stat"><div class="label">Day P&L</div>
                <div class="value {pnl_cls}">${_num(pnl_val, '{:+,.2f}')}</div></div>
            <div class="stat"><div class="label">Open Risk</div>
                <div class="value">${_num(risk.get('open_risk'))}</div></div>
            <div class="stat"><div class="label">Halt</div>
                <div class="value {'red' if halted else 'green'}">{'HALTED' if halted else 'OK'}</div></div>
            <div class="stat"><div class="label">Updated</div>
                <div class="value" style="font-size:13px;">{_escape(_short_ts(risk.get('updated_at')))}</div></div>
        </div>"""

    if not positions:
        body = '<div class="empty">No open positions.</div>'
    else:
        rows = ""
        for p in positions:
            side = (p.get("side") or "").upper()
            entry = p.get("entry_price")
            qty = p.get("qty") or 0
            risk_amt = p.get("risk_amt")
            rows += f"""<tr>
                <td style="color:var(--fg-bright); font-weight:bold;">{_escape(p.get('symbol'))}</td>
                <td><span class="action-badge action-{side}">{side or '?'}</span></td>
                <td>{qty}</td>
                <td>${_num(entry)}</td>
                <td>${_num(p.get('stop'))}</td>
                <td>${_num(p.get('target'))}</td>
                <td>${_num(risk_amt)}</td>
                <td class="src-{_escape(p.get('source'))}">{_escape(p.get('source') or '—')}</td>
                <td style="color:var(--fg3);">{_escape(_short_ts(p.get('opened_at')))}</td>
            </tr>"""
        body = f"""<table>
            <tr><th>Symbol</th><th>Side</th><th>Qty</th><th>Entry</th><th>Stop</th>
                <th>Target</th><th>Risk $</th><th>Source</th><th>Opened</th></tr>
            {rows}
        </table>"""
    return header + body


def render_trades(trades):
    if not trades:
        return '<div class="empty">No trades in the db yet.</div>'
    rows = ""
    for t in trades:
        pnl = t.get("pnl")
        pnl_cls = "pnl-zero"
        pnl_str = "—"
        if isinstance(pnl, (int, float)):
            pnl_cls = "pnl-pos" if pnl > 0 else ("pnl-neg" if pnl < 0 else "pnl-zero")
            pnl_str = f"${pnl:+.2f}"
        action = (t.get("action") or "?").upper()
        src = t.get("source") or "—"
        rows += f"""<tr>
            <td style="color:var(--fg3);">{_escape(_short_ts(t.get('ts')))}</td>
            <td style="color:var(--fg-bright); font-weight:bold;">{_escape(t.get('symbol'))}</td>
            <td><span class="action-badge action-{action}">{action}</span></td>
            <td>{t.get('qty', '')}</td>
            <td>${_num(t.get('fill_price'))}</td>
            <td class="{pnl_cls}">{pnl_str}</td>
            <td class="src-{_escape(src)}" style="font-weight:600;">{_escape(src)}</td>
            <td style="color:var(--fg3); max-width:360px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">{_escape((t.get('reason') or '')[:140])}</td>
        </tr>"""
    return f"""<table>
        <tr><th>Time (UTC)</th><th>Symbol</th><th>Action</th><th>Qty</th>
            <th>Fill</th><th>P&L</th><th>Source</th><th>Reason</th></tr>
        {rows}
    </table>"""


def render_intents(intents):
    if not intents:
        return ('<div class="empty">No intents in the queue. Producers (technical/event/user) '
                'submit intents here; the risk gate approves or rejects them.</div>')
    rows = ""
    for it in intents:
        status = (it.get("status") or "?").lower()
        side = (it.get("side") or "").upper()
        src = it.get("source") or "—"
        reject = it.get("reject_reason")
        reason = it.get("reason") or ""
        detail = reject or reason
        detail_cls = "pnl-neg" if reject else "fg3"
        rows += f"""<tr>
            <td style="color:var(--fg3);">{it.get('id')}</td>
            <td class="src-{_escape(src)}" style="font-weight:600;">{_escape(src)}</td>
            <td>{it.get('priority', 0)}</td>
            <td style="color:var(--fg-bright); font-weight:bold;">{_escape(it.get('symbol'))}</td>
            <td><span class="action-badge action-{side}">{side or '?'}</span></td>
            <td><span class="action-badge st-{status}">{status}</span></td>
            <td>{it.get('confidence') if it.get('confidence') is not None else '—'}</td>
            <td style="color:var(--fg3);">{_escape(_short_ts(it.get('created_at')))}</td>
            <td style="max-width:340px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;"
                class="{'pnl-neg' if reject else ''}">{_escape(detail[:160]) or '—'}</td>
        </tr>"""
    return f"""<table>
        <tr><th>ID</th><th>Source</th><th>Pri</th><th>Symbol</th><th>Side</th>
            <th>Status</th><th>Conf</th><th>Created</th><th>Reason / Reject</th></tr>
        {rows}
    </table>"""


def render_processes(procs, recall):
    # time anchor
    anchor_html = '<div class="empty">reflect.recall() unavailable.</div>'
    if recall:
        ta = recall.get("time_anchor", {})
        anchor_html = f"""<div class="anchor">
            <div><div class="a-label">Now (ET)</div><div class="a-value">{_escape((ta.get('now_et') or '')[:19])}</div></div>
            <div><div class="a-label">Since Last Dream</div><div class="a-value">{_escape(ta.get('since_last_dream'))}</div></div>
            <div><div class="a-label">Since Last User</div><div class="a-value">{_escape(ta.get('since_last_user'))}</div></div>
            <div><div class="a-label">Since Last Trade</div><div class="a-value">{_escape(ta.get('since_last_trade'))}</div></div>
        </div>"""

    if not procs:
        table = '<div class="empty">No process health available (db missing or supervisor import failed).</div>'
    else:
        rows = ""
        for p in procs:
            status = p.get("status", "?")
            age = p.get("age_sec")
            age_s = f"{age}s" if age is not None else "—"
            pid = p.get("pid")
            rows += f"""<tr>
                <td style="color:var(--fg-bright); font-weight:bold;">{_escape(p.get('process'))}</td>
                <td><span class="st-{status}">{status}</span></td>
                <td>{age_s}</td>
                <td style="color:var(--fg3);">{pid if pid else '—'}</td>
                <td style="color:var(--fg3);">{_escape(_short_ts(p.get('last_beat'))) or '—'}</td>
                <td style="color:var(--fg3);">{_escape(p.get('note') or '')}</td>
            </tr>"""
        table = f"""<table>
            <tr><th>Process</th><th>Status</th><th>Beat Age</th><th>PID</th><th>Last Beat</th><th>Note</th></tr>
            {rows}
        </table>"""
    return anchor_html + table


def render_memory(recall, agg):
    if recall is None:
        lessons_html = plans_html = '<div class="empty">reflect.recall() unavailable.</div>'
    else:
        lessons = recall.get("lessons", [])
        plans = recall.get("plans", [])
        if not lessons:
            lessons_html = '<div class="empty">No active lessons. The "dreaming" layer writes these during the Closed session.</div>'
        else:
            lessons_html = ""
            for ls in lessons:
                conf = ls.get("confidence", 0)
                try:
                    width = int(max(0.0, min(1.0, float(conf))) * 120)
                except (TypeError, ValueError):
                    width = 0
                lessons_html += f"""<div class="mem-card">
                    <div class="mem-text">{_escape(ls.get('text'))}</div>
                    <div class="mem-meta">confidence {conf}
                        <span class="conf-bar" style="width:{width}px;"></span>
                        &nbsp;·&nbsp; evidence n={ls.get('n', 0)}</div>
                </div>"""
        if not plans:
            plans_html = '<div class="empty">No active plans.</div>'
        else:
            plans_html = ""
            for pl in plans:
                exp = pl.get("expires_at")
                plans_html += f"""<div class="mem-card">
                    <div class="mem-text">{_escape(pl.get('text'))}</div>
                    <div class="mem-meta">expires {_escape(exp) if exp else 'n/a'}
                        &nbsp;·&nbsp; {_escape(json.dumps(pl.get('tags', {}), ensure_ascii=False))}</div>
                </div>"""

    # agg win-rate matrix
    if not agg:
        agg_html = '<div class="empty">No aggregate stats yet (agg table empty).</div>'
    else:
        rows = ""
        for a in agg:
            trips = a.get("trips") or 0
            wins = a.get("wins") or 0
            wr = (wins / trips * 100) if trips else 0
            wr_cls = "pnl-pos" if wr >= 50 else ("pnl-neg" if trips else "pnl-zero")
            plr = a.get("pl_ratio")
            rows += f"""<tr>
                <td style="color:var(--fg-bright);">{_escape(a.get('symbol'))}</td>
                <td>{_escape(a.get('session'))}</td>
                <td class="src-{_escape(a.get('setup'))}">{_escape(a.get('setup'))}</td>
                <td>{_escape(a.get('rsi_bucket'))}</td>
                <td>{trips}</td>
                <td class="{wr_cls}">{wr:.0f}% ({wins}/{trips})</td>
                <td>{_num(plr, '{:.2f}', '—')}</td>
            </tr>"""
        agg_html = f"""<table>
            <tr><th>Symbol</th><th>Session</th><th>Setup</th><th>RSI</th>
                <th>n</th><th>Win Rate</th><th>P/L Ratio</th></tr>
            {rows}
        </table>"""

    return f"""
    <div class="section-head">Active Lessons</div>
    {lessons_html}
    <div class="section-head">Active Plans</div>
    {plans_html}
    <div class="section-head">Win-Rate Matrix (agg)</div>
    {agg_html}
    """


def render_logs(logs):
    if not logs:
        return '<div class="empty">No legacy log files.</div>'
    rows = ""
    for l in logs:
        action = (l.get("action") or "?").upper()
        reason = l.get("reasoning") or l.get("reason") or l.get("detail") or ""
        rows += f"""<tr>
            <td style="color:var(--fg3);">{_escape(l.get('_file'))}</td>
            <td><span class="action-badge action-{action}">{action}</span></td>
            <td style="color:var(--fg-bright);">{_escape(l.get('symbol') or '')}</td>
            <td style="color:var(--fg3); max-width:480px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap;">{_escape(str(reason)[:160])}</td>
        </tr>"""
    return f"""<div class="update-info">Legacy per-cycle logs (history only; live state is in flint.db)</div>
    <table>
        <tr><th>File</th><th>Action</th><th>Symbol</th><th>Reasoning</th></tr>
        {rows}
    </table>"""


# ─────────────────────────────────────────────────────────────────────────
# page assembly
# ─────────────────────────────────────────────────────────────────────────
def render_html():
    db_ok = _db_available()
    risk = get_risk()
    positions = get_open_positions()
    trades = get_recent_trades(100)
    intents = get_intents(200)
    procs = get_processes()
    recall = get_recall()
    agg = get_agg()
    logs = query_logs(50)

    now_bj = datetime.now(timezone(timedelta(hours=8))).strftime("%Y-%m-%d %H:%M CST")
    now_et = datetime.now(timezone(timedelta(hours=-4))).strftime("%H:%M EDT")

    # top stats bar (from risk_state + counts)
    equity = risk.get("equity") if risk else None
    day_start = risk.get("day_start_equity") if risk else None
    day_pnl = None
    if risk:
        if equity is not None and day_start not in (None, 0):
            day_pnl = equity - day_start
        else:
            day_pnl = risk.get("day_realized_pnl")
    pnl_cls = "green" if (day_pnl or 0) >= 0 else "red"
    halted = bool(risk.get("halt")) if risk else False

    closed = [t for t in trades if isinstance(t.get("pnl"), (int, float))]
    wins = len([t for t in closed if t["pnl"] > 0])
    losses = len([t for t in closed if t["pnl"] < 0])
    win_rate = (wins / (wins + losses) * 100) if (wins + losses) else 0
    n_pending = len([i for i in intents if (i.get("status") or "") == "pending"])
    n_up = len([p for p in procs if p.get("status") == "up"])

    db_warn = ""
    if not db_ok:
        db_warn = ('<div class="halt-banner" style="margin:12px 20px;">flint.db not found — '
                   'showing empty panels. Start the daemon to populate it.</div>')

    positions_html = render_positions(positions, risk)
    trades_html = render_trades(trades)
    intents_html = render_intents(intents)
    processes_html = render_processes(procs, recall)
    memory_html = render_memory(recall, agg)
    logs_html = render_logs(logs)

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta http-equiv="refresh" content="30">
<title>Flint — Daemon Monitor</title>
<style>{CSS}</style>
<script>{JS}</script>
</head>
<body>

<div class="topnav">
    <div class="brand"><span>FLINT</span> DAEMON</div>
    <div style="display:flex; align-items:center; gap:12px;">
        <span style="color:var(--fg3); font-size:11px;">Paper Trading | {now_bj} / {now_et} | refresh 30s | flint.db</span>
        <button class="theme-toggle" id="theme-btn" onclick="toggleTheme()">AUTO</button>
    </div>
</div>

{db_warn}

<div class="stats-bar">
    <div class="stat" style="border-right:1px solid var(--border); padding-right:24px;">
        <div class="label">Equity</div>
        <div class="value">${_num(equity)}</div>
    </div>
    <div class="stat">
        <div class="label">Day P&L</div>
        <div class="value {pnl_cls}">${_num(day_pnl, '{:+,.2f}')}</div>
    </div>
    <div class="stat">
        <div class="label">Win Rate</div>
        <div class="value">{win_rate:.0f}%</div>
        <div style="color:var(--fg3); font-size:11px;">{wins}W {losses}L</div>
    </div>
    <div class="stat">
        <div class="label">Open Pos</div>
        <div class="value">{len(positions)}</div>
    </div>
    <div class="stat">
        <div class="label">Pending Intents</div>
        <div class="value">{n_pending}</div>
    </div>
    <div class="stat">
        <div class="label">Processes Up</div>
        <div class="value">{n_up}/{len(procs) if procs else 0}</div>
    </div>
    <div class="stat" style="border-left:1px solid var(--border); padding-left:24px;">
        <div class="label">Halt</div>
        <div class="value {'red' if halted else 'green'}">{'HALTED' if halted else 'OK'}</div>
    </div>
</div>

<div class="tabs">
    <div class="tab active" data-tab="positions" onclick="showTab('positions')">POSITIONS</div>
    <div class="tab" data-tab="trades" onclick="showTab('trades')">TRADES</div>
    <div class="tab" data-tab="intents" onclick="showTab('intents')">INTENTS</div>
    <div class="tab" data-tab="processes" onclick="showTab('processes')">PROCESSES</div>
    <div class="tab" data-tab="memory" onclick="showTab('memory')">MEMORY</div>
    <div class="tab" data-tab="logs" onclick="showTab('logs')">LOGS</div>
</div>

<div class="content">
    <div id="tab-positions" class="tab-content">{positions_html}</div>
    <div id="tab-trades" class="tab-content hidden">{trades_html}</div>
    <div id="tab-intents" class="tab-content hidden">{intents_html}</div>
    <div id="tab-processes" class="tab-content hidden">{processes_html}</div>
    <div id="tab-memory" class="tab-content hidden">{memory_html}</div>
    <div id="tab-logs" class="tab-content hidden">{logs_html}</div>
</div>

</body></html>"""


class FlintHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path
        try:
            if path == "/api/risk":
                self._json(get_risk() or {})
            elif path == "/api/positions":
                self._json(get_open_positions())
            elif path == "/api/trades":
                self._json(get_recent_trades(200))
            elif path == "/api/intents":
                self._json(get_intents(500))
            elif path == "/api/processes":
                self._json(get_processes())
            elif path == "/api/memory":
                self._json({"recall": get_recall(), "agg": get_agg()})
            else:
                html = render_html()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.end_headers()
                self.wfile.write(html.encode())
        except Exception as e:
            # Never 500 the page — show the error so the dashboard stays useful.
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            self.wfile.write(f"<pre>dashboard error: {_escape(repr(e))}</pre>".encode())

    def _json(self, data):
        self.send_response(200)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.end_headers()
        self.wfile.write(json.dumps(data, ensure_ascii=False, indent=2, default=str).encode())

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print(f"Flint Daemon Monitor at http://localhost:{PORT}  (reading {FLINT_DIR / 'flint.db'})")
    server = http.server.HTTPServer(("", PORT), FlintHandler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nDone.")
