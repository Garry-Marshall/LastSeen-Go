"""Generate internal/bot/testdata/pyref.json from the Python bot.

The Go tests in internal/bot compare the port against these reference values
(view layouts, duration parsing, markdown escaping, member diffing), so the
user-facing behaviour stays identical. Re-run after changing the Python bot:

    <python-venv>/python tools/gen_pyref.py <path-to-LastSeen-Dev>
"""
import asyncio
import json
import os
import random
import sys

PY_ROOT = os.path.abspath(sys.argv[1] if len(sys.argv) > 1 else r"E:\Python\LastSeen-Dev")
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "internal", "bot", "testdata", "pyref.json")
sys.path.insert(0, PY_ROOT)
os.chdir(PY_ROOT)

import discord  # noqa: E402
from bot.locale import load_locales, available_languages  # noqa: E402
from cogs.watch import parse_duration, format_duration, format_away  # noqa: E402
from cogs.tracking import diff_member_state  # noqa: E402
from cogs.commands import PaginationView, ForgetMeConfirmView, UserStatsView, JourneyView  # noqa: E402
from cogs.admin.config_view import ConfigView, DisableReportsConfirmView, LanguageSelectView  # noqa: E402
from cogs.admin.quick_setup import QuickSetupView  # noqa: E402


def layout(view):
    rows = []
    for row in view.to_components():
        items = []
        for c in row["components"]:
            if c["type"] == 2:
                items.append({"type": 2, "style": c["style"], "label": c.get("label") or "",
                              "emoji": (c.get("emoji") or {}).get("name") or "",
                              "disabled": bool(c.get("disabled"))})
            else:
                items.append({"type": c["type"], "placeholder": c.get("placeholder") or "",
                              "min": c.get("min_values", 1), "max": c.get("max_values", 1),
                              "options": [{"label": o["label"], "value": o["value"], "default": bool(o.get("default"))}
                                          for o in c.get("options", [])]})
        rows.append(items)
    return rows


def guild_config(lang, role_required):
    return {"language": lang, "user_role_required": role_required, "inactive_days": 14,
            "bot_admin_role_name": "Mods", "timezone": "Europe/Brussels",
            "notification_channel_id": 123, "report_frequency": "weekly"}


async def views():
    out = []
    for lang in available_languages():
        for req in (0, 1):
            cfg = guild_config(lang, req)
            out.append({"name": f"config/{lang}/{req}", "rows": layout(ConfigView(None, 1, None, cfg))})
        cfg = guild_config(lang, 0)
        out.append({"name": f"quick_setup/{lang}", "rows": layout(QuickSetupView(None, 1, None, cfg))})
        out.append({"name": f"disable_reports/{lang}", "rows": layout(DisableReportsConfirmView(None, 1, cfg))})
        out.append({"name": f"language/{lang}", "rows": layout(LanguageSelectView(None, 1, cfg))})
        out.append({"name": f"forgetme/{lang}", "rows": layout(ForgetMeConfirmView(None, None, 5, lang))})
        out.append({"name": f"user_stats/{lang}", "rows": layout(UserStatsView(1, None, 5, lang))})
        out.append({"name": f"journey/{lang}", "rows": layout(JourneyView(None, 1, 2, "x", 5, lang))})
        for pages in (1, 3):
            for export in (False, True):
                members = [{"username": "a"}] if export else None
                v = PaginationView([discord.Embed()] * pages, 5, export_members=members, lang=lang)
                out.append({"name": f"pagination/{lang}/{pages}/{export}", "rows": layout(v)})
    return out


def diffs():
    r = random.Random(7)
    cases = []
    for _ in range(40):
        cache = {}
        rows = {}
        for uid in r.sample(range(1, 60), r.randint(0, 25)):
            cache[uid] = r.random() < 0.5
        for uid in r.sample(range(1, 60), r.randint(0, 25)):
            rows[uid] = (r.choice([None, 0, 1700000000]), r.choice([0, 1]))
        d = diff_member_state(cache, rows)
        cases.append({
            "cache": {str(k): v for k, v in cache.items()},
            "rows": {str(k): [v[0], v[1]] for k, v in rows.items()},
            "online": sorted(d.online), "offline": sorted(d.offline), "rejoined": sorted(d.rejoined),
            "new": sorted(d.new), "departed": sorted(d.departed),
        })
    return cases


DURATIONS = ["30m", "48h", "7d", " 7D ", "5 m", "0m", "00h", "-5m", "5x", "", "m", "1.5h", "10mm",
             "\t12h\n", "\u00a03d", "99999999999999999999d"]
SECONDS = [0, 1, 59, 60, 61, 3599, 3600, 3660, 7200, 86399, 86400, 90000, 172800, 604800, -30, -90000]
MARKDOWN = [
    "plain", "under_score", "**bold**", "~~strike~~", "||spoiler||", "`code`", "back\slash",
    "> quote", ">>> block", "# heading", "### h3", "#### h4", "- item", "  - nested", "[link](http://x.y)",
    "see https://example.com/a_b*c for more", "<https://example.com/a_b>", "steam://run/123_4",
    "multi\n> line\n# two", "a|b~c*d", "emoji_🙂_name", "x [y](z) w", "trailing.http://a.b/c_d.",
]


def main():
    load_locales()
    data = {
        "views": asyncio.run(views()),
        "parse_duration": [{"in": s, "out": parse_duration(s)} for s in DURATIONS],
        "format_duration": [{"in": s, "out": format_duration(s)} for s in SECONDS],
        "format_away": [{"in": s, "out": format_away(s)} for s in SECONDS],
        "escape_markdown": [{"in": s, "out": discord.utils.escape_markdown(s)} for s in MARKDOWN],
        "diff_member_state": diffs(),
    }
    with open(OUT, "w", encoding="utf-8", newline="\n") as f:
        json.dump(data, f, ensure_ascii=False, indent=1)
    print(f"wrote {OUT}: {len(data['views'])} views")


main()
