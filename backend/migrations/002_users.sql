CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE, -- lowercased
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT,                 -- NULL for accounts that sign in with Google only
    google_sub    TEXT UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash BYTEA PRIMARY KEY, -- sha256 of the cookie value
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- Until now data belonged to nobody. Refuse to go on rather than guess an owner
-- for real accounts and operations; the seed categories are dropped, every new
-- user gets their own copy.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM accounts) OR EXISTS (SELECT 1 FROM transactions) THEN
        RAISE EXCEPTION 'accounts or transactions without an owner: assign them to a user before this migration';
    END IF;
END $$;
DELETE FROM categories;

ALTER TABLE accounts ADD COLUMN user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE accounts DROP CONSTRAINT accounts_name_key;
ALTER TABLE accounts ADD CONSTRAINT accounts_user_name_key UNIQUE (user_id, name);

ALTER TABLE categories ADD COLUMN user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE categories DROP CONSTRAINT categories_name_key;
ALTER TABLE categories ADD CONSTRAINT categories_user_name_key UNIQUE (user_id, name);

ALTER TABLE transactions ADD COLUMN user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE;
DROP INDEX transactions_date_idx;
CREATE INDEX transactions_user_date_idx ON transactions (user_id, date DESC, id DESC);
