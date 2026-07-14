CREATE TABLE app_meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT INTO app_meta(key, value) VALUES('schema_version', '1');

CREATE TABLE cli_accounts (
    id TEXT PRIMARY KEY,
    identity_key TEXT NOT NULL UNIQUE,
    key TEXT NOT NULL,
    refresh_token TEXT,
    expires_at TEXT,
    oidc_issuer TEXT NOT NULL,
    oidc_client_id TEXT NOT NULL,
    email TEXT NOT NULL DEFAULT '',
    user_id TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    request_count INTEGER NOT NULL DEFAULT 0,
    fail_count INTEGER NOT NULL DEFAULT 0,
    cooldown_until REAL,
    created_at REAL NOT NULL,
    updated_at REAL NOT NULL,
    disabled_reason TEXT NOT NULL DEFAULT ''
);

INSERT INTO cli_accounts (
    id, identity_key, key, refresh_token, expires_at, oidc_issuer,
    oidc_client_id, email, user_id, enabled, request_count, fail_count,
    cooldown_until, created_at, updated_at, disabled_reason
) VALUES
    (
        'ready-fixture', 'id:ready-fixture', 'access-ready-fixture',
        'refresh-ready-fixture', '2099-01-02T03:04:05Z',
        'https://issuer.example.test', 'client-ready',
        ' READY@EXAMPLE.TEST ', 'user-ready', 1, 17, 2,
        NULL, 1700000000.125, 1700000060.5, ''
    ),
    (
        'quota-fixture', 'id:quota-fixture', 'access-quota-fixture',
        '', '2099-01-02T03:04:05Z', 'https://auth.x.ai', 'client-quota',
        'quota@example.test', 'user-quota', 0, 23, 5,
        NULL, 1700000100, 1700000200, 'subscription:free-usage-exhausted'
    ),
    (
        'auth-fixture', 'id:auth-fixture', 'access-auth-fixture',
        'refresh-auth-fixture', '2020-01-02T03:04:05Z',
        'https://auth.x.ai', 'client-auth', 'auth@example.test',
        'user-auth', 1, 31, 7, NULL, 1700000300, 1700000400,
        'invalid-token'
    ),
    (
        'cooldown-fixture', 'id:cooldown-fixture', 'access-cooldown-fixture',
        '', '2099-01-02T03:04:05Z', 'https://auth.x.ai', 'client-cooldown',
        'cooldown@example.test', 'user-cooldown', 1, 41, 3,
        4070995200, 1700000500, 1700000600, 'rate-limit'
    );
