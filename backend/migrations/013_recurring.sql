-- Повторяющиеся операции: расход или доход на счёт с заданной частотой.
-- Операции создаёт сервер в день срока (по Москве). Даты считаются от
-- start_date: n-й раз — start_date + n шагов, поэтому 31-е число в коротком
-- месяце становится последним днём месяца и не «уползает» дальше.
-- last_date — день последней созданной операции, next_date — следующий срок.
CREATE TABLE recurring (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type       TEXT NOT NULL CHECK (type IN ('expense', 'income')),
    account_id BIGINT NOT NULL,
    amount     NUMERIC(24, 8) NOT NULL CHECK (amount > 0),
    category   TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    freq       TEXT NOT NULL CHECK (freq IN ('day', 'week', '2weeks', 'month', 'quarter', 'year')),
    start_date DATE NOT NULL,
    last_date  DATE,
    next_date  DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT recurring_account_fkey FOREIGN KEY (account_id, user_id)
        REFERENCES accounts (id, user_id) ON DELETE CASCADE
);

CREATE INDEX recurring_user_idx ON recurring (user_id, id);
CREATE INDEX recurring_next_idx ON recurring (next_date);
