# AI Usage Dashboard — Go + React

Read-only dashboard of usage and limits for **OpenAI**, **ChatGPT (subscription)**,
**Claude / Anthropic**, **OpenCode Go** and **Ollama Cloud**.

## Tiles

The dashboard always shows 7 tiles (in this order):

1. **OpenAI** — organization cost from the Organization Costs API
   (`GET /v1/organization/costs`). This is **not** a ChatGPT subscription meter.
   Requires a key with organization admin permissions
   (`OPENAI_ADMIN_API_KEY`; falls back to `OPENAI_API_KEY`). Optionally
   `OPENAI_MONTHLY_LIMIT_USD` adds a bar for a custom (local) budget.
2. **ChatGPT (subscription)** — 5 h / weekly subscription limits from the
   unofficial ChatGPT backend (`chatgpt.com/backend-api/wham/usage`) — the same
   one Codex CLI uses. Requires the `auth.json` file from Codex CLI (see ENV).
3. **Claude / Anthropic** — Pro/Max subscription limits: 5 h / weekly and
   weekly per model, from the unofficial `api.anthropic.com/api/oauth/usage`
   (the same as `/usage` in Claude Code). Requires `credentials.json` from Claude Code.
4. **Ollama Cloud** — `GET https://ollama.com/api/usage` (Bearer `OLLAMA_API_KEY`).
   Since October 2026 the endpoint returns documented usage statistics:
   `totals.request_count`, `totals.usage_usd`, tokens and a breakdown into
   buckets (`buckets`). The backend still recognises the older format
   `limits.session|weekly|monthly.usage` (limit fraction 0..1) and `activity.cost`.
   The window is chosen with `OLLAMA_USAGE_RANGE` (`24h`|`7d`|`30d`).
5. **Ollama Cloud — limit** — `GET https://ollama.com/api/balance`: monthly
   limit (`allowance_usd`), remaining credits (`balance_usd`), purchased credits
   and the **reset date** (`period.until`). The bar is `% = (allowance - balance)/allowance`.
   Because of this the reset date comes from the API and the manual
   `OLLAMA_RESET_ANCHOR` is no longer needed. (A per-model breakdown is still
   not exposed by the API — only in the `ollama.com/settings` UI.)
6. **OpenCode Go A** — 5 h / weekly / monthly quota from
   `GET https://opencode.ai/zen/go/v1/usage`.
7. **OpenCode Go B** — as above, for the second account
   (`OPENCODE_GO_B_API_KEY`).

## Security assumptions

- Keys and tokens are read only by the backend (from ENV or mounted files);
  the frontend never receives keys or fragments of them.
- The application **does not call generation/chat/completions endpoints** and
  sends no prompts, so refreshing the dashboard does not consume model tokens.
- Every tile always exists. A missing key, an incompatible API or missing
  metrics yields the `No data` / `Fetch error` state without hiding the tile.
- `/api/dashboard` responses carry `Cache-Control: no-store`.
- The frontend refreshes data **manually only** (the "Refresh" button) — there is
  no automatic background polling.
- ChatGPT/Claude token auto-refresh writes the rotated tokens back to the
  mounted file (kept out of git). Note: the same `refresh_token` used
  concurrently locally and on a server invalidates each other.

## Running with Docker

```bash
cp .env.example .env
# put the keys you need into .env

docker compose up --build
```

Dashboard: `http://localhost:8080`

The ChatGPT and Claude tiles additionally require mounting the credentials files
(`CHATGPT_AUTH_FILE`, `CLAUDE_AUTH_FILE`) — see ENV.

## Development

Terminal 1:

```bash
cd backend
set -a
source ../.env
set +a
go run .
```

Terminal 2:

```bash
cd frontend
npm install
npm run dev
```

The Vite proxy forwards `/api/*` to `localhost:8080`.

## Dashboard API

`GET /api/dashboard`

Example shape (metrics may have optional `resetAt` and `rows`):

```json
{
  "providers": [
    {
      "id": "opencode-go",
      "name": "OpenCode Go A",
      "status": "ok",
      "configured": true,
      "summary": "Subscription limits",
      "source": "OpenCode Go quota API",
      "metrics": [
        {"label":"5 h","value":"12.0% used","percent":12,"resetAt":"2026-09-28T15:00:00Z"}
      ]
    },
    {
      "id": "ollama-cloud",
      "name": "Ollama Cloud",
      "status": "ok",
      "configured": true,
      "summary": "Ollama Cloud usage",
      "source": "Ollama Cloud API (/api/usage)",
      "metrics": [
        {"label":"Requests (7 days)","value":"7035"},
        {"label":"Cost (7 days)","value":"12.44 USD"},
        {"label":"Tokens (7 days)","value":"4180000","rows":[
          {"label":"Input","value":"3900000"},
          {"label":"Cached","value":"1200000"},
          {"label":"Output","value":"280000"}
        ]}
      ]
    },
    {
      "id": "ollama-cloud-quota",
      "name": "Ollama Cloud — limit",
      "status": "ok",
      "configured": true,
      "summary": "Ollama Cloud monthly limit",
      "source": "Ollama Cloud API (/api/balance)",
      "metrics": [
        {"label":"Monthly limit","value":"45.00 / 60.00 USD","percent":75,"resetAt":"2026-01-01T00:00:00Z"}
      ]
    }
  ]
}
```

`status`: `ok` | `no_data` | `error`.

## ENV

| Variable | Purpose |
| --- | --- |
| `PORT` | backend port (default 8080) |
| `OPENAI_ADMIN_API_KEY` / `OPENAI_API_KEY` | OpenAI Organization Costs API |
| `OPENAI_MONTHLY_LIMIT_USD` | optional local OpenAI budget (bar) |
| `OPENCODE_GO_API_KEY` | OpenCode Go A |
| `OPENCODE_GO_B_API_KEY` | OpenCode Go B |
| `OLLAMA_API_KEY` | Ollama Cloud (`GET https://ollama.com/api/usage`) |
| `OLLAMA_USAGE_RANGE` | optional usage window: `24h`, `7d` (API default) or `30d` |
| `OLLAMA_MONTHLY_LIMIT_USD` | optional local budget (percentage bar for cost) |
| `OLLAMA_RESET_ANCHOR` | a known Ollama monthly reset date (RFC3339), shifted month by month |
| `CHATGPT_AUTH_FILE` | path to `auth.json` from Codex CLI (ChatGPT tile) |
| `CLAUDE_AUTH_FILE` | path to `.credentials.json` from Claude Code (Claude tile) |

Optional endpoint overrides: `OPENAI_COSTS_URL`, `OPENCODE_GO_USAGE_URL`,
`OPENCODE_GO_B_USAGE_URL`, `OLLAMA_USAGE_URL`, `OLLAMA_BALANCE_URL`,
`CHATGPT_USAGE_URL`, `CHATGPT_TOKEN_URL`, `CLAUDE_USAGE_URL`, `CLAUDE_TOKEN_URL`.

See `.env.example`. Never commit `.env` or credentials files to the repo.
