# Admin API v1

Frozen contract for the next management frontend.  
Legacy `/admin/api/*` remains for `panel.html` and will be removed after SPA cutover.

## Envelope

Success:

```json
{ "ok": true, "data": { }, "error": null }
```

Error:

```json
{ "ok": false, "data": null, "error": { "code": "unauthorized", "message": "..." } }
```

Inference `/v1/*` keeps OpenAI/Anthropic error shapes — do not mix envelopes.

## Auth

| Surface | Credential |
|---------|------------|
| `/v1/*` | `api_key` (`Authorization: Bearer` or `x-api-key`) |
| `/api/admin/v1/*` | admin key: `panel_password` → `app_key` → `api_key` |
| Public | meta, health |

Admin requests: `Authorization: Bearer <admin-key>` or `x-api-key: <admin-key>`.

`POST /api/admin/v1/auth/login` accepts `{ "password": "..." }` and returns the same secret as `token` (bearer). Future: signed session cookie.

## Routes

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| GET | `/api/admin/v1/system/meta` | no | `auth_required`, `api_version`, `version` |
| POST | `/api/admin/v1/auth/login` | no | body password/token |
| GET | `/api/admin/v1/auth/me` | yes | role probe |
| GET | `/api/admin/v1/dashboard` | yes | summary + pool + circuit |
| GET | `/api/admin/v1/pool` | yes | ready/unavailable/reasons |
| GET | `/api/admin/v1/system` | yes | version, default_model (no secrets) |
| GET | `/api/admin/v1/accounts` | yes | query: `pool`, `q`, `page`, `page_size` |
| DELETE | `/api/admin/v1/accounts/{id}` | yes | |
| POST | `/api/admin/v1/accounts/{id}/recover` | yes | |
| POST | `/api/admin/v1/accounts/import/preview` | yes | dry-run import |
| POST | `/api/admin/v1/accounts/import` | yes | commit import |

### Health aliases

| Path | Behavior |
|------|----------|
| `GET /health` | legacy health + pool |
| `GET /healthz` | same as `/health` |
| `GET /readyz` | 200 if ready>0 else 503 |

## Legacy aliases (keep until SPA ships)

| Legacy | v1 |
|--------|-----|
| `GET /admin/api/panel-meta` | `GET /api/admin/v1/system/meta` (shape differs: legacy is flat) |
| `GET /admin/api/cli-accounts` | `GET /api/admin/v1/accounts` |
| `DELETE /admin/api/cli-accounts/{id}` | `DELETE /api/admin/v1/accounts/{id}` |
| `POST /admin/api/cli-accounts/{id}/recover` | `POST /api/admin/v1/accounts/{id}/recover` |
| `POST /admin/api/accounts/import[/preview]` | `POST /api/admin/v1/accounts/import[/preview]` |

Legacy responses stay **flat JSON** (no `ok/data` envelope) for `panel.html`.

## Account public fields

Never return raw `access_token` / `refresh_token`:

```text
id, email, user_id, team_id, pool, unavailable_reason, retry_at,
last_error_code, quota_actual, quota_limit, request_count,
active, max_active, has_refresh_token
```

## Frontend cutover notes

1. Dev proxy: Vite → `http://127.0.0.1:8787`
2. Store admin token from login; send Bearer on all v1 calls
3. Prefer `/api/admin/v1/*` only; do not depend on legacy paths
4. Embed `frontend/dist` at the service root `/` (see ROADMAP Phase C)
