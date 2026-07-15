# Admin API v1

Frozen contract for the management frontend.
Legacy `/admin/api/*` remains for compatibility and can be removed after clients migrate to v1.

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
| `/v1/*` | persisted client key (`Authorization: Bearer` or `x-api-key`) |
| `/api/admin/v1/*` | short-lived opaque administrator access token (`Authorization: Bearer`) |
| Public | meta, health |

`POST /api/admin/v1/auth/login` accepts `{ "username": "admin", "password": "...", "remember": false }`. It returns a 5-minute access token in JSON and sets the refresh token only in an `HttpOnly`, `SameSite=Strict` cookie. `remember` controls whether that cookie persists across browser sessions; the server-side refresh session expires after 30 days either way.

`POST /api/admin/v1/auth/refresh` rotates both access and refresh credentials. Concurrent use of the previous refresh cookie returns `409 refresh_conflict` without deleting the winner cookie; confirmed replay outside the grace window returns `401 invalid_refresh_session` and revokes the session family. `POST /api/admin/v1/auth/logout` revokes the server-side session and deletes the cookie.

Legacy `panel_password` / `app_key` values are one-time bootstrap inputs only. `api_key` can migrate only to a legacy client key and never grants administrator access.

## Routes

| Method | Path | Auth | Notes |
|--------|------|------|-------|
| GET | `/api/admin/v1/system/meta` | no | `auth_required`, `api_version`, `version` |
| POST | `/api/admin/v1/auth/login` | no | username, password, remember |
| POST | `/api/admin/v1/auth/refresh` | refresh cookie | rotate credentials |
| POST | `/api/admin/v1/auth/logout` | bearer and/or refresh cookie | revoke session and clear cookie |
| GET | `/api/admin/v1/auth/me` | yes | role probe |
| GET | `/api/admin/v1/dashboard` | yes | summary + pool + circuit |
| GET | `/api/admin/v1/pool` | yes | ready/unavailable/reasons |
| GET | `/api/admin/v1/system` | yes | version, default_model (no secrets) |
| GET | `/api/admin/v1/accounts` | yes | query: `pool`, `q`, `page`, `page_size` |
| DELETE | `/api/admin/v1/accounts/{id}` | yes | |
| POST | `/api/admin/v1/accounts/{id}/recover` | yes | |
| POST | `/api/admin/v1/accounts/import/preview` | yes | dry-run import |
| POST | `/api/admin/v1/accounts/import` | yes | commit import |
| GET/POST | `/api/admin/v1/client-keys` | yes | list/create; secret is shown only on create |
| GET/PATCH | `/api/admin/v1/client-keys/{id}` | yes | inspect/update policy |
| POST | `/api/admin/v1/client-keys/{id}/revoke` | yes | irreversible revoke |

### Health aliases

| Path | Behavior |
|------|----------|
| `GET /health` | legacy health + pool |
| `GET /healthz` | same as `/health` |
| `GET /readyz` | 200 if ready>0 else 503 |

## Legacy aliases

| Legacy | v1 |
|--------|-----|
| `GET /admin/api/panel-meta` | `GET /api/admin/v1/system/meta` (shape differs: legacy is flat) |
| `GET /admin/api/cli-accounts` | `GET /api/admin/v1/accounts` |
| `DELETE /admin/api/cli-accounts/{id}` | `DELETE /api/admin/v1/accounts/{id}` |
| `POST /admin/api/cli-accounts/{id}/recover` | `POST /api/admin/v1/accounts/{id}/recover` |
| `POST /admin/api/accounts/import[/preview]` | `POST /api/admin/v1/accounts/import[/preview]` |

Legacy responses stay **flat JSON** (no `ok/data` envelope).

## Account public fields

Never return raw `access_token` / `refresh_token`:

```text
id, email, user_id, team_id, pool, unavailable_reason, retry_at,
last_error_code, quota_actual, quota_limit, request_count,
active, max_active, has_refresh_token
```

## Frontend cutover notes

1. Dev proxy: Vite → `http://127.0.0.1:8787`
2. Keep the access token in memory only; use the HttpOnly refresh cookie to restore/rotate the session
3. Prefer `/api/admin/v1/*` only; do not depend on legacy paths
4. The Docker image serves `/app/frontend/dist` at `/`; bare Go development uses Vite unless `frontend.static_path` is configured
