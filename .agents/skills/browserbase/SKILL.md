---
name: browserbase
description: 'Use when shadow needs the web beyond plain HTTP — fetch/search pages, or drive a real cloud Chrome (JS-rendered sites, interactive flows, logins/auth) via the `browse` CLI + Stagehand from croft.'
when_to_use: 'When shadow needs the web beyond plain HTTP — fetch/search pages or drive a real cloud Chrome (JS-rendered sites, interactive flows, logins) via the browse CLI from croft.'
---

# Browserbase — shadow's cloud browser

Browserbase runs real Chrome in the cloud that I drive from croft via the **`browse`** CLI (and Stagehand). It's how I reach more of the web — JS-rendered pages, interactive flows, and logged-in/auth-walled sites — without a local browser.

## Setup (already done)
- **CLI:** `~/.local/bin/browse` v0.9.1, on PATH (user-local install: `npm i -g --prefix ~/.local browse@latest` to update — NOT a system/sudo install; sudo npm runs as root and lands in `/root/.npm-global`, unusable). It's the official Browserbase CLI (`github.com/browserbase/stagehand`, `@browserbase.com` maintainers). Verified the package identity before trusting the generic name `browse`.
- **Key:** `~/shadow/.browserbase_key` (0600), same handling as `~/shadow/gh_pat`. Use it: `export BROWSERBASE_API_KEY=$(cat ~/shadow/.browserbase_key)`. The key ALONE resolves the project — **never set `BROWSERBASE_PROJECT_ID`** (verified empirically: not needed; ignore older docs that say otherwise). (Key is also in the 2026-06-27 session transcript — rotate if that matters.)
- **Account:** Free plan, "Production project", concurrency 3.
- The CLI prints an "Update available" banner on every command — strip it before parsing JSON.

## What I can do (lightest first)
- **Fetch** — `browse cloud fetch <url> [--format markdown]` → JSON `{statusCode, content, headers}`. No browser, no LLM tokens. Best for static/SSR content. ✓ verified working.
- **Search** — `browse cloud search "<query>" [--num-results N] [--json]` → web results / URLs.
- **Full session (interactive / JS-rendered / auth)** — Stagehand (`new Stagehand({ env: "BROWSERBASE" })`, drive with `act`/`extract`/`observe`/`agent`) or Playwright/Puppeteer over CDP. For pages needing rendering or interaction.
- **Sessions / observability** — `browse cloud sessions list`; Live View (real-time) + replay/logs/network at https://www.browserbase.com/sessions. Always surface the FULL `browserbase.com/sessions/<id>` link, never truncated.

## Auth flows + token acquisition (THE reason this exists)
The croft pain point (operator, 2026-06-27): croft had no browser, so any flow needing browser input — chiefly **OAuth / login-to-get-a-token** (e.g. porter's Google-Drive refresh token) — meant a clunky device-code-on-phone + copy-paste/scp the token back into croft. A cloud browser closes that gap.
- **The pattern:** drive a session to the consent/login URL → the human (jacinta) completes login interactively in **Live View** (watch the real browser, click "approve" — no phone, no device code) → the OAuth redirect lands the auth `code` in that same browser → I read it and exchange it for tokens **in croft** (token never leaves the box, no scp).
- **Contexts** persist the login/cookies across runs so re-consent is rare.
- **⚠️ Google is the hard case** (and it's the porter case): Google aggressively flags automation — the "browser may not be secure" block that forced porter onto device flow can still fire, and Free has no Verified/proxies/CAPTCHA-solving to beat it. Live View (real Chrome + real human clicking) improves the odds vs headless automation but isn't guaranteed. **Non-Google OAuth (GitHub, most SaaS) is the clean win; Google = worth trying, may still fight back.**
- Boundary: porter's own Google-OAuth config is owned by the operator's separate thread — this capability serves that class of flow but don't edit porter's config from here.

## Free-tier caveats (real, plan accordingly)
- **Model Gateway $5 token cap.** Stagehand `act`/`extract`/`agent` route LLM calls through the Browserbase key, capped at $5 of tokens on Free — an otherwise-working run can fail partway once exhausted (looks like "API key not valid" but isn't). To avoid burning it: prefer Fetch/Search (no tokens) for plain content, or point Stagehand at MY OWN LLM (an Anthropic key, or the local qwen on robo-dog) so AI calls bypass the cap. Leave any `MODEL_API_KEY` blank otherwise (a placeholder value triggers a misleading "API key not valid").
- **No Proxies / Verified on Free.** Bot-protected / heavily-defended sites (LinkedIn, Yelp, Instagram, TikTok, ticketing, most large retailers) will likely block a plain session — they need Verified + proxied sessions (paid: Developer+ for proxies, Scale for Verified). Expect blocks on those until upgraded; for a protected target, fall back to a non-protected source via Fetch/Search or flag the upgrade need.

## Docs
- Skill/source of truth: https://browserbase.com/SKILL.md · docs: https://docs.browserbase.com · sessions/dashboard: https://www.browserbase.com/sessions · keys: https://browserbase.com/settings
