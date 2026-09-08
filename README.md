# operator — Construct cloud operator

A light, **standalone** agent harness. It is *not* the desktop brain: no IDE,
no code/file/browser tools, no long-lived credential. It stands in for the user
when their desktop is offline:

1. **Scheduled automations** — claims due rules from Conductor's service path,
   runs them under a delegated owner token, reports the result back.
2. **Live asks** — `POST /internal/ask` runs a one-shot answer as the asking
   user (source-api routes a phone ask here when the desktop operator is down).

The agent loop, LLM client and tools all live in this module (borrowed in shape
from the desktop brain + `api/tv`, but with zero import of either).

## Tools

| Tool | What it does |
|---|---|
| `space_run_action` | Run an installed Space's action headless via space-runtime |
| `web_search` | DuckDuckGo HTML search (no key) |
| `web_fetch` | Fetch + strip a URL |

## Endpoints

- `GET /health` → `{"ok":true,"service":"operator"}`
- `POST /internal/ask` (header `X-Internal-Secret`) — body `{token, text, request_id}` → `{content, request_id}`

## Config (env)

| Var | Default | Notes |
|---|---|---|
| `INTERNAL_SHARED_SECRET` | — | **Required.** Same value as Conductor + source. |
| `CONDUCTOR_URL` | `https://conductor.lisaos.dev` | Claim/report target. |
| `GATEWAY_URL` | `https://my.lisaos.dev` | LLM base = `…/api/construct`. |
| `OPERATOR_LLM_BASE` | `${GATEWAY_URL}/api/construct` | Override the chat root directly. |
| `OPERATOR_MODEL` | `source-medium` | Construct-managed model id. |
| `CONSTRUCT_SPACE_RUNTIME_URL` | *(unset)* | space-runtime base, e.g. `http://srv-captain--space-runtime:60190`. If unset, `space_run_action` is disabled; web/LLM still work. |
| `PORT` | `8090` | HTTP port (CapRover health + ask). |

## Deploy

CapRover app `operator` (internal only — source reaches it at
`http://srv-captain--operator:8090`). Build context is this directory; the
`captain-definition` points at `./Dockerfile`.

## Build / run locally

```sh
go build .
INTERNAL_SHARED_SECRET=dev CONDUCTOR_URL=http://127.0.0.1:8090 \
  CONSTRUCT_SPACE_RUNTIME_URL=http://127.0.0.1:60190 ./operator
```

## Known follow-ups

- **Action discovery**: there is no `space_list_actions` yet — the cloud path
  needs a space-runtime listing endpoint. Until then the model must name the
  space + action (fine for explicit automations/asks).
- `OPERATOR_MODEL` default is a guess (`source-medium`); confirm against
  `/api/construct/models`.
