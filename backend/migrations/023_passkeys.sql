-- Passkeys (WebAuthn) для входа без Google. data — запись ключа целиком, как её
-- хранит go-webauthn (открытый ключ, счётчик подписей, флаги); credential_id —
-- отдельно, чтобы искать ключ при входе. В бекапы не попадают: это не данные учёта.
CREATE TABLE passkeys (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id BYTEA NOT NULL UNIQUE,
    data          JSONB NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ
);

CREATE INDEX passkeys_user_idx ON passkeys (user_id);
