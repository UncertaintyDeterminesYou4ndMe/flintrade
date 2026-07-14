-- Flint Agent 数据契约 (SQLite, WAL)
-- 所有进程共享。约定:生产者只 INSERT intents/signals;
-- 只有 Executor/Reconciler 写 positions/trades/orders;只有 risk_monitor/Executor 写 risk_state.halt。
-- 约定由 db.py 的 role guard 在代码层强制(见 db.py)。
-- 时间戳一律 ISO8601 UTC 'YYYY-MM-DDTHH:MM:SSZ';金额 REAL;布尔用 INTEGER 0/1。

-- ── 意图队列(消息总线)。生产者写 pending,Executor 消费裁决 ──────────────
CREATE TABLE IF NOT EXISTS intents (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    source        TEXT    NOT NULL,            -- technical | event | user | risk_monitor
    priority      INTEGER NOT NULL DEFAULT 0,  -- 越大越优先(来自 risk.toml [priority])
    symbol        TEXT    NOT NULL,
    side          TEXT    NOT NULL,            -- long | short | flatten | close
    hypothesis_qty INTEGER,                    -- 生产者的建议量;最终量由 Risk Gate 风险定额覆盖
    entry_hint    REAL,                        -- 期望入场价(限价参考)
    stop          REAL,                        -- 止损位(风险定额的分母来源)
    target        REAL,                        -- 目标位
    confidence    INTEGER,                     -- 0-100
    dedup_key     TEXT,                         -- 去重键(舆情同一事件只触发一次)
    reason        TEXT,                         -- LLM 的 chain_of_thought 摘要
    features      TEXT,                         -- JSON:决策当时的特征快照(RSI/VWAP/session/score…)
    status        TEXT    NOT NULL DEFAULT 'pending', -- pending|approved|rejected|filled|expired|cancelled
    reject_reason TEXT,
    created_at    TEXT    NOT NULL,
    decided_at    TEXT,
    expires_at    TEXT                          -- intent 时效;过期 Executor 不再执行
);
CREATE INDEX IF NOT EXISTS idx_intents_pending ON intents(status, priority DESC, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_intents_dedup ON intents(dedup_key) WHERE dedup_key IS NOT NULL;

-- ── 当前持仓(可多条)。单一真相。Executor/Reconciler 维护 ────────────────
CREATE TABLE IF NOT EXISTS positions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    symbol      TEXT    NOT NULL,
    side        TEXT    NOT NULL,              -- long | short
    qty         INTEGER NOT NULL,
    entry_price REAL    NOT NULL,
    stop        REAL,
    target      REAL,
    risk_amt    REAL,                          -- (entry-stop)*qty,组合风险约束的累加项
    source      TEXT,                          -- 开仓信号来源
    intent_id   INTEGER REFERENCES intents(id),
    opened_at   TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'open' -- open | closed
);
CREATE INDEX IF NOT EXISTS idx_positions_open ON positions(status, symbol);

-- ── 成交账本(append-only)。每笔带来源与特征快照,供 Dreaming 聚合 ────────
CREATE TABLE IF NOT EXISTS trades (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              TEXT    NOT NULL,
    symbol          TEXT    NOT NULL,
    action          TEXT    NOT NULL,          -- BUY | SELL | SHORT | COVER
    qty             INTEGER NOT NULL,
    fill_price      REAL    NOT NULL,
    commission      REAL    NOT NULL DEFAULT 0,
    pnl             REAL,                       -- 仅平仓腿有值
    source          TEXT,                       -- technical | event | user
    intent_id       INTEGER REFERENCES intents(id),
    broker_order_id TEXT,
    position_id     INTEGER REFERENCES positions(id),
    features        TEXT,                       -- JSON 特征快照
    reason          TEXT,
    attribution     TEXT                        -- strategy | manual | test | outage-degraded(见 settle.py)
);
CREATE INDEX IF NOT EXISTS idx_trades_symbol_ts ON trades(symbol, ts);
CREATE INDEX IF NOT EXISTS idx_trades_source ON trades(source);

-- ── broker 订单镜像。Executor 创建,Reconciler 更新 ──────────────────────
CREATE TABLE IF NOT EXISTS orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    client_order_id TEXT    NOT NULL UNIQUE,   -- 幂等键,本地生成
    broker_order_id TEXT,
    symbol          TEXT    NOT NULL,
    side            TEXT    NOT NULL,          -- BUY | SELL | SHORT | COVER
    qty             INTEGER NOT NULL,
    price           REAL,
    outside_rth     TEXT,
    status          TEXT    NOT NULL DEFAULT 'submitted', -- submitted|filled|partial|rejected|cancelled|expired
    filled_qty      INTEGER NOT NULL DEFAULT 0,
    avg_price       REAL,
    intent_id       INTEGER REFERENCES intents(id),
    created_at      TEXT    NOT NULL,
    updated_at      TEXT
);
CREATE INDEX IF NOT EXISTS idx_orders_open ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_broker ON orders(broker_order_id);

-- ── 原始信号(尤其舆情)。溯源用 ────────────────────────────────────────
CREATE TABLE IF NOT EXISTS signals (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    source  TEXT NOT NULL,
    symbol  TEXT,
    kind    TEXT,                              -- news | filing | quote_anomaly | …
    payload TEXT,                              -- JSON
    ts      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_signals_ts ON signals(ts);

-- ── 被追踪的 catalyst(财报/CPI/FOMC)。供 prompt 上下文与 event loop ─────
CREATE TABLE IF NOT EXISTS events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    symbol   TEXT,
    kind     TEXT,                             -- earnings | macro | …
    title    TEXT,
    fires_at TEXT,                             -- 事件发生时间(UTC)
    payload  TEXT,                             -- JSON
    status   TEXT NOT NULL DEFAULT 'pending',  -- pending | fired | expired
    ts       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_fires ON events(status, fires_at);

-- ── 组合风险状态(单行 id=1)。Executor/risk_monitor 维护 ─────────────────
CREATE TABLE IF NOT EXISTS risk_state (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    equity           REAL,                     -- 当前权益(现金 + 持仓市值)
    day_start_equity REAL,                     -- 今日开盘权益(熔断基准)
    day_realized_pnl REAL NOT NULL DEFAULT 0,
    open_risk        REAL NOT NULL DEFAULT 0,  -- 所有持仓 risk_amt 之和
    halt             INTEGER NOT NULL DEFAULT 0,
    halt_reason      TEXT,
    updated_at       TEXT
);

-- ── Dreaming:语义教训(沉淀,带 confidence,不绑日期)──────────────────────
CREATE TABLE IF NOT EXISTS memory_lessons (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    text           TEXT NOT NULL,
    evidence       TEXT,                        -- JSON: [trade_id,…]
    confidence     REAL NOT NULL DEFAULT 0.5,
    tags           TEXT,                        -- JSON: {symbol,session,setup,…} 供结构化过滤
    status         TEXT NOT NULL DEFAULT 'active', -- active | archived
    created        TEXT NOT NULL,
    last_confirmed TEXT
);
CREATE INDEX IF NOT EXISTS idx_lessons_active ON memory_lessons(status, confidence DESC);

-- ── Dreaming:情景计划(会过期,带 expires_at)────────────────────────────
CREATE TABLE IF NOT EXISTS memory_plans (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    text       TEXT NOT NULL,
    tags       TEXT,
    status     TEXT NOT NULL DEFAULT 'active',  -- active | expired | done
    created    TEXT NOT NULL,
    expires_at TEXT                             -- 唤醒时按当前 ET 决定还活不活
);
CREATE INDEX IF NOT EXISTS idx_plans_active ON memory_plans(status, expires_at);

-- ── 滚动聚合:(symbol × session × setup × rsi_bucket) → 胜率/盈亏比 ────────
CREATE TABLE IF NOT EXISTS agg (
    symbol     TEXT NOT NULL,
    session    TEXT NOT NULL,
    setup      TEXT NOT NULL,
    rsi_bucket TEXT NOT NULL,
    trips      INTEGER NOT NULL DEFAULT 0,
    wins       INTEGER NOT NULL DEFAULT 0,
    losses     INTEGER NOT NULL DEFAULT 0,
    pl_ratio   REAL,
    updated_at TEXT,
    PRIMARY KEY (symbol, session, setup, rsi_bucket)
);

-- ── Dreaming:逐笔平仓复盘(postmortem)。审计 + honesty 层的原材料 ─────────
CREATE TABLE IF NOT EXISTS trade_reviews (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    trade_id INTEGER NOT NULL UNIQUE,
    position_id INTEGER, symbol TEXT, attribution TEXT,
    entry_price REAL, exit_price REAL, qty INTEGER, pnl REAL,
    stop REAL, target REAL, hold_minutes INTEGER,
    exit_kind TEXT, slippage_vs_stop REAL,
    confidence INTEGER, thesis TEXT, exit_reason TEXT,
    thesis_verdict TEXT, entry_grade TEXT, exit_grade TEXT,
    confidence_justified INTEGER, lesson TEXT,
    reviewed_at TEXT
);

-- ── 进程心跳(supervisor 看门狗)────────────────────────────────────────
CREATE TABLE IF NOT EXISTS heartbeats (
    process   TEXT PRIMARY KEY,
    last_beat TEXT NOT NULL,
    pid       INTEGER,
    note      TEXT
);

-- ── 语义记忆向量(embedding 存 BLOB,暴力 cosine;规模到了换 LanceDB)──────
-- vec = struct.pack('<%df') 的 float32 紧凑数组。检索时全扫该 kind 做 cosine。
CREATE TABLE IF NOT EXISTS vectors (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    kind    TEXT NOT NULL,                  -- trade | news | lesson
    ref_id  INTEGER,                        -- 指向 trades.id / signals.id / memory_lessons.id
    text    TEXT NOT NULL,                  -- 被嵌入的原文
    model   TEXT,                           -- 产出向量的 embedding 模型
    dim     INTEGER,
    vec     BLOB NOT NULL,
    created TEXT NOT NULL,
    UNIQUE(kind, ref_id)
);
CREATE INDEX IF NOT EXISTS idx_vectors_kind ON vectors(kind);

-- ── 杂项 KV(last_dream_date / schema_version 等)──────────────────────────
CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT
);
