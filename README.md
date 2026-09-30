# AI Usage Dashboard — Go + React

Read-only dashboard dla **OpenAI**, **Claude / Anthropic**, **OpenCode Go** i **Ollama Cloud**.

## Założenia bezpieczeństwa

- Klucze są czytane wyłącznie przez backend z ENV.
- Frontend nigdy nie otrzymuje kluczy ani ich fragmentów.
- Aplikacja **nie wywołuje endpointów generowania/chat/completions** i nie wysyła promptów, więc samo odświeżenie dashboardu nie zużywa tokenów modeli.
- Każdy z 4 kafelków istnieje zawsze. Brak klucza, brak kompatybilnego API albo brak metryk daje stan `Brak danych`/`Błąd pobierania`, bez ukrywania kafelka.
- Odpowiedzi `/api/dashboard` mają `Cache-Control: no-store`.

## Co faktycznie pokazuje

- **OpenCode Go** — quota 5h / tydzień / miesiąc z `GET https://opencode.ai/zen/go/v1/usage`.
- **OpenAI** — koszt organizacji z Organization Costs API. To **nie jest licznik subskrypcji ChatGPT Plus/Pro**. Opcjonalnie `OPENAI_MONTHLY_LIMIT_USD` dodaje lokalny pasek budżetu.
- **Claude / Anthropic** — koszt organizacji z Usage & Cost Admin API. To **nie jest licznik Claude Pro/Max** i wymaga uprawnień Admin API. Opcjonalnie `ANTHROPIC_MONTHLY_LIMIT_USD` dodaje lokalny pasek budżetu.
- **Ollama Cloud** — best-effort `GET https://ollama.com/api/usage`; ten account-level endpoint nie jest stabilnym publicznym kontraktem. Gdy nie działa lub format się zmieni, kafelek pozostaje i informuje o braku danych.

## Uruchomienie przez Docker

```bash
cp .env.example .env
# wpisz potrzebne klucze do .env

docker compose up --build
```

Dashboard: `http://localhost:8080`

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

Przykładowy kształt:

```json
{
  "providers": [
    {
      "id": "opencode-go",
      "name": "OpenCode Go",
      "status": "ok",
      "configured": true,
      "summary": "Limity subskrypcji",
      "source": "OpenCode Go quota API",
      "metrics": [
        {"label":"5 h","value":"12.0% użyte","percent":12,"resetAt":"2026-09-28T15:00:00Z"}
      ]
    }
  ]
}
```

## ENV

Patrz `.env.example`. Do repo nie commituj `.env`.
