---
name: web-research
description: Search and extraction strategy for the researcher specialist. Adapted to Retainer's current tools (Brave web search + news search via BRAVE_SEARCH_API_KEY; Jina Reader via JINA_API_KEY).
agents: researcher, cognitive
---

## Web research

The researcher specialist handles web work. From the cog's perspective you delegate via `agent_researcher`; from inside the researcher, here's the tool set.

### Tools available

The researcher's registry is gated on environment keys:

| Tool | Best for | Requires |
|---|---|---|
| `brave_web_search` | Broad discovery — titles, URLs, snippets | `BRAVE_SEARCH_API_KEY` |
| `brave_news_search` | Time-sensitive queries, current events, recent developments | `BRAVE_SEARCH_API_KEY` |
| `jina_reader` | Extract clean markdown from a known URL | `JINA_API_KEY` (or anonymous tier when `RETAINER_ENABLE_JINA_ANON` set) |

### Decision tree

- **General web search?** → `brave_web_search`
- **Time-sensitive / news?** → `brave_news_search`
- **Have a URL, need full content?** → `jina_reader`
- **No keys configured?** → tell the cog (the researcher won't have been started; the delegate tool wouldn't exist)

### Quality signals

After extraction, note in your final summary:
- **Publication date** — when the source was last updated
- **Primary vs secondary** — did the source author the information, or aggregate it
- **Contradictions with earlier results** — flag these explicitly; don't paper over them

Prefer primary sources. When a snippet from search and a full extraction conflict, trust the full extraction.

### Output format

When you complete your task, respond with a concise summary the orchestrator can act on:
- Sources with URLs
- Key facts (with confidence levels when relevant)
- Anything that *didn't* turn up (dead ends matter)

Omit raw page contents. Omit your own intermediate reasoning — the orchestrator wants the answer, not your scratchpad. The cog will pass your reply back to the user; make it readable.

### What's deferred

These tools exist in Springdrift but are not yet ported to Retainer — do NOT assume you have them:

- `brave_answer`, `brave_llm_context`, `brave_summarizer` (extra Brave endpoints; only the search clients are wired today)
- `web_search` (DuckDuckGo fallback) — no implementation
- `fetch_url` — no raw HTTP tool
- `kagi_search`, `kagi_summarize` — Kagi backend not configured
- `store_result` / `retrieve_result` — artifacts subsystem deferred (so big extractions go directly into your reply, capped by `max_tokens`)

If a task genuinely needs one of those, say so in your final reply rather than approximating.

### Honesty under low signal

If a search returns no useful results, say so. Don't synthesise a plausible-sounding answer from nothing — the cog (and the user) need the truth, including "I couldn't find this." Same for partial coverage: report what you found AND what you didn't.
