# AI Usage Dashboard — Go + React

Read-only dashboard zużycia i limitów dla **OpenAI**, **ChatGPT (subskrypcja)**,
**Claude / Anthropic**, **OpenCode Go** i **Ollama Cloud**.

## Kafelki

Dashboard pokazuje zawsze 6 kafelków (w tej kolejności):

1. **OpenAI** — koszt organizacji z Organization Costs API
   (`GET /v1/organization/costs`). To **nie jest** licznik subskrypcji ChatGPT.
   Wymaga klucza z uprawnieniami administracyjnymi organizacji
   (`OPENAI_ADMIN_API_KEY`; fallback `OPENAI_API_KEY`). Opcjonalnie
   `OPENAI_MONTHLY_LIMIT_USD` dokłada pasek własnego (lokalnego) budżetu.
2. **ChatGPT (subskrypcja)** — limity subskrypcji 5 h / tydzień z nieoficjalnego
   backendu ChatGPT (`chatgpt.com/backend-api/wham/usage`) — tego samego, którego
   używa Codex CLI. Wymaga pliku `auth.json` z Codex CLI (patrz ENV).
3. **Claude / Anthropic** — limity subskrypcji Pro/Max: 5 h / tydzień oraz
   tygodniowe per model, z nieoficjalnego `api.anthropic.com/api/oauth/usage`
   (to samo, co `/usage` w Claude Code). Wymaga `credentials.json` z Claude Code.
4. **Ollama Cloud** — best-effort `GET https://ollama.com/api/usage`: miesięczne
   zużycie (%) oraz żądania wg modelu. Endpoint nie jest stabilnym publicznym
   kontraktem; gdy nie działa lub zmieni format, kafelek informuje o braku danych.
   Nie zwraca daty resetu — jest ona wyliczana z `OLLAMA_RESET_ANCHOR`.
5. **OpenCode Go A** — quota 5 h / tydzień / miesiąc z
   `GET https://opencode.ai/zen/go/v1/usage`.
6. **OpenCode Go B** — jak wyżej, dla drugiego konta
   (`OPENCODE_GO_B_API_KEY`).

## Założenia bezpieczeństwa

- Klucze i tokeny czyta wyłącznie backend (z ENV lub z zamontowanych plików);
  frontend nigdy nie otrzymuje kluczy ani ich fragmentów.
- Aplikacja **nie wywołuje endpointów generowania/chat/completions** i nie wysyła
  promptów, więc samo odświeżenie dashboardu nie zużywa tokenów modeli.
- Każdy kafelek istnieje zawsze. Brak klucza, brak kompatybilnego API albo brak
  metryk daje stan `Brak danych` / `Błąd pobierania`, bez ukrywania kafelka.
- Odpowiedzi `/api/dashboard` mają `Cache-Control: no-store`.
- Frontend odświeża dane **tylko ręcznie** (przycisk „Odśwież") — brak
  automatycznego odpytywania w tle.
- Auto-odświeżanie tokenów ChatGPT/Claude zapisuje zrotowane tokeny z powrotem do
  zamontowanego pliku (poza gitem). Uwaga: ten sam `refresh_token` używany
  równolegle lokalnie i na serwerze wzajemnie się unieważnia.

## Uruchomienie przez Docker

```bash
cp .env.example .env
# wpisz potrzebne klucze do .env

docker compose up --build
```

Dashboard: `http://localhost:8080`

Kafelki ChatGPT i Claude wymagają dodatkowo zamontowania plików poświadczeń
(`CHATGPT_AUTH_FILE`, `CLAUDE_AUTH_FILE`) — patrz ENV.

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

Vite proxy przekazuje `/api/*` do `localhost:8080`.

## API dashboardu

`GET /api/dashboard`

Przykładowy kształt (metryki mogą mieć opcjonalne `resetAt` i `rows`):

```json
{
  "providers": [
    {
      "id": "opencode-go",
      "name": "OpenCode Go A",
      "status": "ok",
      "configured": true,
      "summary": "Limity subskrypcji",
      "source": "OpenCode Go quota API",
      "metrics": [
        {"label":"5 h","value":"12.0% użyte","percent":12,"resetAt":"2026-09-28T15:00:00Z"}
      ]
    },
    {
      "id": "ollama-cloud",
      "name": "Ollama Cloud",
      "status": "ok",
      "configured": true,
      "summary": "Limity Ollama Cloud",
      "source": "Nieudokumentowany account usage endpoint",
      "metrics": [
        {"label":"Miesiąc","value":"94.2% użyte","percent":94.2,"resetAt":"2026-01-01T00:00:00Z"},
        {"label":"Żądania wg modelu (miesiąc)","value":"7035","rows":[
          {"label":"deepseek-v4-pro:0813","value":"4148"},
          {"label":"deepseek-v4.1-flash","value":"2832"}
        ]}
      ]
    }
  ]
}
```

`status`: `ok` | `no_data` | `error`.

## ENV

| Zmienna | Do czego |
| --- | --- |
| `PORT` | port backendu (domyślnie 8080) |
| `OPENAI_ADMIN_API_KEY` / `OPENAI_API_KEY` | OpenAI Organization Costs API |
| `OPENAI_MONTHLY_LIMIT_USD` | opcjonalny lokalny budżet OpenAI (pasek) |
| `OPENCODE_GO_API_KEY` | OpenCode Go A |
| `OPENCODE_GO_B_API_KEY` | OpenCode Go B |
| `OLLAMA_API_KEY` | Ollama Cloud |
| `OLLAMA_RESET_ANCHOR` | znana data resetu miesięcznego Ollamy (RFC3339), przesuwana co miesiąc |
| `CHATGPT_AUTH_FILE` | ścieżka do `auth.json` z Codex CLI (kafelek ChatGPT) |
| `CLAUDE_AUTH_FILE` | ścieżka do `.credentials.json` z Claude Code (kafelek Claude) |

Opcjonalne nadpisania endpointów: `OPENAI_COSTS_URL`, `OPENCODE_GO_USAGE_URL`,
`OPENCODE_GO_B_USAGE_URL`, `OLLAMA_USAGE_URL`, `CHATGPT_USAGE_URL`,
`CHATGPT_TOKEN_URL`, `CLAUDE_USAGE_URL`, `CLAUDE_TOKEN_URL`.

Patrz `.env.example`. Do repo nie commituj `.env` ani plików poświadczeń.
