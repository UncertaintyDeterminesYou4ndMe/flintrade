"""Thin wrapper around the `longbridge` CLI for placing and querying orders.

Used by the Executor (to place buy/sell orders) and the Reconciler (to query
order status and fills). Pure Python 3 stdlib only — no external deps.

Two modes:
  - LIVE: shells out to the real `longbridge` CLI (never shell=True).
  - DRY-RUN: simulates fills without touching the CLI, so the rest of the
    pipeline can be built and tested with zero side effects (no real paper
    orders placed). DRY-RUN is the DEFAULT when FLINT_DRY_RUN is unset, for
    safety.

The longbridge CLI reads its credentials from LONGBRIDGE_* env vars set by the
launcher; we inherit the process environment and never hardcode anything.

CLI patterns (see prompt.md "Execution" + scripts/collect.sh):
  longbridge order buy  SYMBOL QTY --price P --outside-rth FLAG -y --format json
  longbridge order sell SYMBOL QTY --price P --outside-rth FLAG -y --format json
      -> JSON containing an `order_id` on success
  longbridge order executions --format json          # today's fills, newest first
  longbridge order detail <order_id> --format json   # has a `status` field
  longbridge assets --format json
  longbridge positions --format json
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

# Module-level counter used to mint stable, side-effect-free pseudo order ids in
# dry-run mode (so tests are deterministic across a single process run).
_DRY_COUNTER = 0

# How long (seconds) to wait on any single CLI invocation before giving up.
_CLI_TIMEOUT = 15

_TRUTHY = {"1", "true", "yes"}


def _env_dry_run_default() -> bool:
    """Resolve the default dry_run from the FLINT_DRY_RUN env var.

    If the env var is unset, default to True (safe — never place real orders
    unless explicitly opted into live mode).
    """
    raw = os.environ.get("FLINT_DRY_RUN")
    if raw is None:
        return True
    return raw.strip().lower() in _TRUTHY


class Broker:
    """Place and query orders via the longbridge CLI, with a dry-run mode."""

    # longbridge CLI 必需的凭据环境变量(由 flint.env 经 agentctl.sh 注入)。
    _REQUIRED_CREDS = (
        "LONGBRIDGE_APP_KEY",
        "LONGBRIDGE_APP_SECRET",
        "LONGBRIDGE_ACCESS_TOKEN",
    )

    def __init__(self, dry_run: bool | None = None):
        if dry_run is None:
            dry_run = _env_dry_run_default()
        self.dry_run = bool(dry_run)
        # LIVE 模式必须有凭据;缺失就当场大声失败,而不是让每次下单静默被拒。
        if not self.dry_run:
            missing = [k for k in self._REQUIRED_CREDS if not os.environ.get(k)]
            if missing:
                raise RuntimeError(
                    "LIVE 模式缺少 longbridge 凭据环境变量: "
                    f"{missing}。请经 scripts/agentctl.sh 启动(它会 source flint.env),"
                    "或先手动 source flint.env。"
                )

    # ------------------------------------------------------------------ #
    # Internal helpers
    # ------------------------------------------------------------------ #
    def _run_cli(self, args: list[str]) -> tuple[bool, object]:
        """Run `longbridge <args>` through the rate governor (agent.lb).

        Returns (ok, payload):
          - (True, parsed_json) on success
          - (False, error_string) otherwise
        Pacing + 429/timeout backoff are handled centrally in agent.lb so all the
        daemon's concurrent loops share one global longbridge rate budget.
        """
        from agent import lb
        ok, data, raw = lb.run(args, timeout=_CLI_TIMEOUT)
        if not ok:
            return False, raw or "longbridge call failed"
        if data is None:  # 成功但无 JSON 体
            return False, f"could not parse CLI JSON: {raw[:500]}"
        return True, data

    @staticmethod
    def _extract_order_id(payload: object) -> str | None:
        """Pull an order_id out of whatever shape the CLI returned."""
        if isinstance(payload, dict):
            for key in ("order_id", "orderId", "id"):
                val = payload.get(key)
                if val:
                    return str(val)
            # Some CLIs nest the result under a `data` key.
            data = payload.get("data")
            if isinstance(data, dict):
                return Broker._extract_order_id(data)
        return None

    def _dry_order_id(self, client_order_id: str | None) -> str:
        global _DRY_COUNTER
        _DRY_COUNTER += 1
        suffix = client_order_id if client_order_id else f"{os.getpid()}-{_DRY_COUNTER}"
        return f"DRY-{suffix}"

    def _place(
        self,
        side: str,
        symbol: str,
        qty: int,
        price: float,
        outside_rth: str,
        client_order_id: str | None,
    ) -> dict:
        if self.dry_run:
            oid = self._dry_order_id(client_order_id)
            print(
                f"[DRY-RUN] {side.upper()} {qty} {symbol} @ {price} "
                f"(outside_rth={outside_rth}) -> {oid}",
                file=sys.stderr,
            )
            return {
                "ok": True,
                "broker_order_id": oid,
                "status": "Filled",
                "fill_price": price,
                "raw": {"dry_run": True},
            }

        args = [
            "order",
            side,
            symbol,
            str(qty),
            "--price",
            str(price),
            "--outside-rth",
            outside_rth,
            "-y",
            "--format",
            "json",
        ]
        ok, payload = self._run_cli(args)
        if not ok:
            return {
                "ok": False,
                "broker_order_id": None,
                "status": "Rejected",
                "raw": payload,
            }

        oid = self._extract_order_id(payload)
        return {
            "ok": oid is not None,
            "broker_order_id": oid,
            "status": "Submitted" if oid is not None else "Unknown",
            "raw": payload,
        }

    # ------------------------------------------------------------------ #
    # Public API
    # ------------------------------------------------------------------ #
    def buy(
        self,
        symbol: str,
        qty: int,
        price: float,
        outside_rth: str,
        client_order_id: str | None = None,
    ) -> dict:
        """Place a buy order. Returns a normalized result dict."""
        return self._place("buy", symbol, qty, price, outside_rth, client_order_id)

    def sell(
        self,
        symbol: str,
        qty: int,
        price: float,
        outside_rth: str,
        client_order_id: str | None = None,
    ) -> dict:
        """Place a sell order. Returns a normalized result dict."""
        return self._place("sell", symbol, qty, price, outside_rth, client_order_id)

    def order_detail(self, broker_order_id: str) -> dict:
        """Fetch order detail (works across days). Has a `status` field."""
        if self.dry_run:
            return {"status": "Filled"}
        ok, payload = self._run_cli(
            ["order", "detail", str(broker_order_id), "--format", "json"]
        )
        if not ok:
            return {"status": "Unknown", "error": payload}
        if isinstance(payload, dict):
            return payload
        return {"status": "Unknown", "raw": payload}

    def recent_fills(self, limit: int = 20) -> list[dict]:
        """Return today's fills, newest first, truncated to `limit`."""
        if self.dry_run:
            return []
        ok, payload = self._run_cli(["order", "executions", "--format", "json"])
        if not ok:
            return []
        rows: list = []
        if isinstance(payload, list):
            rows = payload
        elif isinstance(payload, dict):
            for key in ("executions", "data", "items"):
                val = payload.get(key)
                if isinstance(val, list):
                    rows = val
                    break
        return [r for r in rows[: max(0, limit)] if isinstance(r, dict)]

    def assets(self) -> list | dict:
        """Return account assets (balances)."""
        if self.dry_run:
            return []
        ok, payload = self._run_cli(["assets", "--format", "json"])
        if not ok:
            return []
        return payload if isinstance(payload, (list, dict)) else []

    def positions(self) -> list | None:
        """Return account positions.

        Returns:
          - [] in dry-run mode (no side effects, nothing to report).
          - None if the underlying CLI call failed (distinguishes "API
            hiccup" from "broker genuinely reports zero positions" — callers
            MUST NOT treat None as flat).
          - list (possibly []) on a successful CLI call.
        """
        if self.dry_run:
            return []
        ok, payload = self._run_cli(["positions", "--format", "json"])
        if not ok:
            return None
        if isinstance(payload, list):
            return payload
        if isinstance(payload, dict):
            for key in ("positions", "data", "items"):
                val = payload.get(key)
                if isinstance(val, list):
                    return val
        return []


if __name__ == "__main__":
    # Self-test: construct a dry-run broker and place a sample order. This must
    # have ZERO side effects — no real CLI calls, no real orders.
    broker = Broker(dry_run=True)
    result = broker.buy(
        "NVDA.US", 6, 200.92, "ANY_TIME", client_order_id="selftest-001"
    )
    print(json.dumps(result, indent=2))
    detail = broker.order_detail(result["broker_order_id"])
    print(json.dumps(detail, indent=2))
