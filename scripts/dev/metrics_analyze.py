#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.10"
# dependencies = ["httpx", "rich"]
# ///
"""Analyze ProviderBridge metrics grouped by Provider.

Fetches per-request records from GET /v1/admin/metrics, aggregates per-provider
stats, and renders three focused tables: Usage, Cache Analysis, SLA.

Usage:
  BASE_URL=http://127.0.0.1:38440 API_KEY=sk-... uv run scripts/dev/metrics_analyze.py

Environment variables:
  BASE_URL   ProviderBridge server base URL (required)
  API_KEY    Bearer token for authentication (optional if auth is disabled)
  LIMIT      Max records per page (default: 1000)
  PROVIDER   Filter by provider key (optional)
  MODEL      Filter by model name (optional)
  SINCE      Filter by start time, RFC3339 (optional)
  UNTIL      Filter by end time, RFC3339 (optional)
"""

from __future__ import annotations

import os
import sys
from dataclasses import dataclass, field

import httpx
from rich.console import Console
from rich.table import Table
from rich.text import Text


@dataclass
class ProviderStats:
    requests: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    cache_read: int = 0
    cache_creation: int = 0
    cost: float = 0.0
    total_response_time_ms: int = 0
    errors: int = 0
    model_breakdown: dict[str, "ProviderStats"] = field(default_factory=dict)

    @property
    def fresh_input(self) -> int:
        v = self.input_tokens - self.cache_read - self.cache_creation
        return v if v > 0 else 0

    @property
    def total_tokens(self) -> int:
        return self.input_tokens + self.output_tokens


# ── Data fetching ──


def fetch_all_records(
    base_url: str,
    api_key: str | None,
    *,
    limit: int = 1000,
    model: str | None = None,
    since: str | None = None,
    until: str | None = None,
) -> list[dict]:
    url = f"{base_url.rstrip('/')}/v1/admin/metrics"
    headers: dict[str, str] = {}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"

    all_records: list[dict] = []
    offset = 0
    while True:
        params: dict[str, str | int] = {"limit": limit, "offset": offset}
        if model:
            params["model"] = model
        if since:
            params["since"] = since
        if until:
            params["until"] = until

        resp = httpx.get(url, params=params, headers=headers, timeout=30)
        resp.raise_for_status()
        data = resp.json()
        records = data.get("records", [])
        all_records.extend(records)
        if len(records) < limit:
            break
        offset += limit
    return all_records


def aggregate(records: list[dict]) -> tuple[dict[str, ProviderStats], ProviderStats]:
    by_provider: dict[str, ProviderStats] = {}
    total = ProviderStats()
    for r in records:
        provider = r.get("provider_key", "") or r.get("protocol", "?") or "?"
        model = r.get("actual_model") or r.get("model", "?")
        ps = by_provider.setdefault(provider, ProviderStats())
        _update(ps, r)
        _update(total, r)
        # Build model breakdown within provider
        ms = ps.model_breakdown.setdefault(model, ProviderStats())
        _update(ms, r)
    return by_provider, total


def _update(ps: ProviderStats, r: dict) -> None:
    ps.requests += 1
    ps.input_tokens += r.get("input_tokens", 0)
    ps.output_tokens += r.get("output_tokens", 0)
    ps.cache_read += r.get("cache_read", 0)
    ps.cache_creation += r.get("cache_creation", 0)
    ps.cost += r.get("cost", 0.0)
    ps.total_response_time_ms += r.get("response_time", 0) // 1_000_000
    if r.get("status") != "success":
        ps.errors += 1


# ── Formatting ──


def fmt_tok(n: int) -> str:
    if n >= 1_000_000_000:
        return f"{n / 1_000_000_000:.2f}B"
    if n >= 1_000_000:
        return f"{n / 1_000_000:.2f}M"
    if n >= 1_000:
        return f"{n / 1_000:.1f}K"
    return str(n)


def fmt_cost(v: float) -> str:
    return f"¥{v:.4f}" if v else "¥0"


def fmt_pct(v: float, *, invert: bool = False) -> Text:
    s = f"{v:.1f}%"
    if invert:
        if v <= 20:
            return Text(s, style="bold green")
        if v <= 50:
            return Text(s, style="yellow")
        return Text(s, style="red")
    if v >= 80:
        return Text(s, style="bold green")
    if v >= 40:
        return Text(s, style="yellow")
    return Text(s, style="red")


def fmt_ratio(v: float | None) -> Text:
    if v is None:
        return Text("N/A", style="dim")
    s = f"{v:.2f}"
    if v >= 4:
        return Text(s, style="bold green")
    if v >= 1:
        return Text(s, style="green")
    return Text(s, style="yellow")


def fmt_unit_cost(v: float | None) -> Text:
    if v is None:
        return Text("N/A", style="dim")
    s = f"¥{v:.2f}"
    if v <= 1:
        return Text(s, style="bold green")
    if v <= 5:
        return Text(s, style="yellow")
    return Text(s, style="red")


def fmt_latency(ps: ProviderStats) -> Text:
    avg = ps.total_response_time_ms / ps.requests if ps.requests > 0 else 0
    if avg <= 0:
        return Text("N/A", style="dim")
    if avg >= 1000:
        s = f"{avg / 1000:.1f}s"
    else:
        s = f"{avg:.0f}ms"
    if avg < 2000:
        return Text(s, style="green")
    if avg < 10000:
        return Text(s, style="yellow")
    return Text(s, style="red")


def _sorted(by_provider: dict[str, ProviderStats]) -> list[tuple[str, ProviderStats]]:
    return sorted(by_provider.items(), key=lambda x: x[1].cost, reverse=True)


# ── Table builders ──


def build_usage_table(by_provider: dict[str, ProviderStats], total: ProviderStats) -> Table:
    """Table 1: token volume and cost per provider."""
    t = Table(title="Usage by Provider", expand=True, show_lines=False)
    t.add_column("Provider", style="cyan", no_wrap=True)
    t.add_column("Req", justify="right")
    t.add_column("Fresh", justify="right")
    t.add_column("Cache R", justify="right", style="green")
    t.add_column("Cache W", justify="right", style="dim")
    t.add_column("Output", justify="right")
    t.add_column("Cost", justify="right", style="yellow")

    for provider, ps in _sorted(by_provider):
        t.add_row(
            provider, str(ps.requests),
            fmt_tok(ps.fresh_input), fmt_tok(ps.cache_read),
            fmt_tok(ps.cache_creation), fmt_tok(ps.output_tokens),
            fmt_cost(ps.cost),
        )
        # Show model breakdown per provider
        for model, ms in sorted(ps.model_breakdown.items(), key=lambda x: x[1].cost, reverse=True):
            t.add_row(
                f"  └ {model}", str(ms.requests),
                fmt_tok(ms.fresh_input), fmt_tok(ms.cache_read),
                fmt_tok(ms.cache_creation), fmt_tok(ms.output_tokens),
                fmt_cost(ms.cost),
            )

    if len(by_provider) > 1:
        t.add_section()
        t.add_row(
            Text("TOTAL", style="bold"),
            Text(str(total.requests), style="bold"),
            fmt_tok(total.fresh_input), fmt_tok(total.cache_read),
            fmt_tok(total.cache_creation), fmt_tok(total.output_tokens),
            fmt_cost(total.cost),
        )
    return t


def build_cache_table(by_provider: dict[str, ProviderStats], total: ProviderStats) -> Table:
    """Table 2: cache efficiency metrics."""
    t = Table(title="Cache Analysis by Provider", expand=True, show_lines=False)
    t.add_column("Provider", style="cyan", no_wrap=True)
    t.add_column("Hit%", justify="right")
    t.add_column("R/W", justify="right")
    t.add_column("Fresh%", justify="right")
    t.add_column("Cache R%", justify="right")
    t.add_column("Cache W%", justify="right")

    for provider, ps in _sorted(by_provider):
        inp = ps.input_tokens
        fresh = ps.fresh_input
        hit = (ps.cache_read / inp * 100) if inp > 0 else 0
        rw = (ps.cache_read / ps.cache_creation) if ps.cache_creation > 0 else None
        fresh_pct = (fresh / inp * 100) if inp > 0 else 0
        cr_pct = (ps.cache_read / inp * 100) if inp > 0 else 0
        cw_pct = (ps.cache_creation / inp * 100) if inp > 0 else 0

        t.add_row(
            provider,
            fmt_pct(hit),
            fmt_ratio(rw),
            fmt_pct(fresh_pct, invert=True),
            fmt_pct(cr_pct),
            fmt_pct(cw_pct, invert=True),
        )

        for model, ms in sorted(ps.model_breakdown.items(), key=lambda x: x[1].cost, reverse=True):
            inp2 = ms.input_tokens
            fresh2 = ms.fresh_input
            hit2 = (ms.cache_read / inp2 * 100) if inp2 > 0 else 0
            rw2 = (ms.cache_read / ms.cache_creation) if ms.cache_creation > 0 else None
            fresh_pct2 = (fresh2 / inp2 * 100) if inp2 > 0 else 0
            cr_pct2 = (ms.cache_read / inp2 * 100) if inp2 > 0 else 0
            cw_pct2 = (ms.cache_creation / inp2 * 100) if inp2 > 0 else 0

            t.add_row(
                f"  └ {model}",
                fmt_pct(hit2),
                fmt_ratio(rw2),
                fmt_pct(fresh_pct2, invert=True),
                fmt_pct(cr_pct2),
                fmt_pct(cw_pct2, invert=True),
            )

    if len(by_provider) > 1:
        inp = total.input_tokens
        fresh = total.fresh_input
        hit = (total.cache_read / inp * 100) if inp > 0 else 0
        rw = (total.cache_read / total.cache_creation) if total.cache_creation > 0 else None
        fresh_pct = (fresh / inp * 100) if inp > 0 else 0
        cr_pct = (total.cache_read / inp * 100) if inp > 0 else 0
        cw_pct = (total.cache_creation / inp * 100) if inp > 0 else 0

        t.add_section()
        t.add_row(
            Text("TOTAL", style="bold"),
            fmt_pct(hit),
            fmt_ratio(rw),
            fmt_pct(fresh_pct, invert=True),
            fmt_pct(cr_pct),
            fmt_pct(cw_pct, invert=True),
        )
    return t


def build_sla_table(by_provider: dict[str, ProviderStats], total: ProviderStats) -> Table:
    """Table 3: service-level / cost-efficiency metrics."""
    t = Table(title="SLA by Provider", expand=True, show_lines=False)
    t.add_column("Provider", style="cyan", no_wrap=True)
    t.add_column("Req", justify="right")
    t.add_column("¥/MTok", justify="right")
    t.add_column("Lat.", justify="right")
    t.add_column("Err", justify="right", style="red")
    t.add_column("Err%", justify="right")

    for provider, ps in _sorted(by_provider):
        unit = (ps.cost / ps.total_tokens * 1_000_000) if ps.total_tokens > 0 else None
        err_rate = ps.errors / ps.requests * 100 if ps.requests > 0 else 0
        t.add_row(
            provider,
            str(ps.requests),
            fmt_unit_cost(unit),
            fmt_latency(ps),
            Text(str(ps.errors), style="red") if ps.errors > 0 else Text("0", style="dim"),
            fmt_pct(err_rate, invert=True),
        )

        for model, ms in sorted(ps.model_breakdown.items(), key=lambda x: x[1].cost, reverse=True):
            unit2 = (ms.cost / ms.total_tokens * 1_000_000) if ms.total_tokens > 0 else None
            err_rate2 = ms.errors / ms.requests * 100 if ms.requests > 0 else 0
            t.add_row(
                f"  └ {model}",
                str(ms.requests),
                fmt_unit_cost(unit2),
                fmt_latency(ms),
                Text(str(ms.errors), style="red") if ms.errors > 0 else Text("0", style="dim"),
                fmt_pct(err_rate2, invert=True),
            )

    if len(by_provider) > 1:
        unit = (total.cost / total.total_tokens * 1_000_000) if total.total_tokens > 0 else None
        err_rate = total.errors / total.requests * 100 if total.requests > 0 else 0
        t.add_section()
        t.add_row(
            Text("TOTAL", style="bold"),
            Text(str(total.requests), style="bold"),
            fmt_unit_cost(unit),
            fmt_latency(total),
            Text(str(total.errors), style="bold red") if total.errors > 0 else Text("0", style="bold dim"),
            fmt_pct(err_rate, invert=True),
        )
    return t


def main() -> None:
    base_url = os.environ.get("BASE_URL", "")
    if not base_url:
        print("Error: set BASE_URL environment variable.", file=sys.stderr)
        sys.exit(1)

    api_key = os.environ.get("API_KEY") or None
    limit = int(os.environ.get("LIMIT", "1000"))
    provider_filter = os.environ.get("PROVIDER") or None
    model = os.environ.get("MODEL") or None
    since = os.environ.get("SINCE") or None
    until = os.environ.get("UNTIL") or None

    console = Console()

    try:
        records = fetch_all_records(
            base_url, api_key,
            limit=limit, model=model, since=since, until=until,
        )
    except httpx.HTTPStatusError as e:
        console.print(f"[red]HTTP {e.response.status_code}:[/] {e.response.text}")
        sys.exit(1)
    except httpx.ConnectError as e:
        console.print(f"[red]Connection failed:[/] {e}")
        sys.exit(1)

    if not records:
        console.print("[dim]No metrics records found.[/]")
        return

    # Filter by provider if specified
    if provider_filter:
        records = [r for r in records if r.get("provider_key") == provider_filter]
        if not records:
            console.print(f"[yellow]No records for provider '{provider_filter}'.[/]")
            return

    by_provider, total = aggregate(records)

    console.print(build_usage_table(by_provider, total))
    console.print()
    console.print(build_cache_table(by_provider, total))
    console.print()
    console.print(build_sla_table(by_provider, total))
    console.print(f"\n[dim]{len(records)} records from {base_url}[/]")


if __name__ == "__main__":
    main()
