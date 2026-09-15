CREATE TABLE accounts (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL,
    currency   TEXT NOT NULL,
    balance    NUMERIC(24, 8) NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE categories (
    id        BIGSERIAL PRIMARY KEY,
    name      TEXT NOT NULL UNIQUE,
    parent_id BIGINT REFERENCES categories (id) ON DELETE SET NULL,
    kind      TEXT NOT NULL CHECK (kind IN ('expense', 'income'))
);

CREATE TABLE transactions (
    id            BIGSERIAL PRIMARY KEY,
    date          DATE NOT NULL,
    title         TEXT NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('expense', 'income', 'transfer')),
    category_id   BIGINT REFERENCES categories (id) ON DELETE SET NULL,
    -- the name stays on the operation when its category is deleted
    category_name TEXT NOT NULL DEFAULT '',
    account_id    BIGINT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    to_account_id BIGINT REFERENCES accounts (id) ON DELETE CASCADE,
    amount        NUMERIC(24, 8) NOT NULL CHECK (amount > 0),
    -- transfers only: the amount credited to to_account, in its currency
    received      NUMERIC(24, 8),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((type = 'transfer') = (to_account_id IS NOT NULL AND received IS NOT NULL))
);

CREATE INDEX transactions_date_idx ON transactions (date DESC, id DESC);
CREATE INDEX transactions_account_idx ON transactions (account_id);
CREATE INDEX transactions_to_account_idx ON transactions (to_account_id);
CREATE INDEX transactions_category_idx ON transactions (category_id);

-- Price of one unit of the currency in rubles
CREATE TABLE rates (
    code       TEXT PRIMARY KEY,
    rub        NUMERIC(24, 8) NOT NULL CHECK (rub > 0),
    source     TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Starting categories from the prototype
INSERT INTO categories (id, name, parent_id, kind) VALUES
    (1, 'Продукты', NULL, 'expense'),
    (2, 'Кафе и доставка', NULL, 'expense'),
    (3, 'Транспорт', NULL, 'expense'),
    (4, 'Жильё и связь', NULL, 'expense'),
    (5, 'Здоровье', NULL, 'expense'),
    (6, 'Подписки', NULL, 'expense'),
    (7, 'Спорт', NULL, 'expense'),
    (8, 'Футбол', 7, 'expense'),
    (9, 'Хоккей', 7, 'expense'),
    (10, 'Баскетбол', 7, 'expense'),
    (11, 'Одежда', NULL, 'expense'),
    (12, 'Зарплата', NULL, 'income'),
    (13, 'Подработка', NULL, 'income'),
    (14, 'Накопления', NULL, 'expense'),
    (15, 'Вёрстка сайтов', 13, 'income'),
    (16, 'Фотосъёмка', 13, 'income'),
    (17, 'Проценты по вкладу', NULL, 'income');
SELECT setval(pg_get_serial_sequence('categories', 'id'), (SELECT max(id) FROM categories));

-- Fallback rates until the first refresh succeeds
INSERT INTO rates (code, rub, source, updated_at) VALUES
    ('USD', 92, 'default', 'epoch'),
    ('EUR', 100, 'default', 'epoch'),
    ('USDT', 92, 'default', 'epoch'),
    ('BTC', 5400000, 'default', 'epoch'),
    ('ETH', 290000, 'default', 'epoch'),
    ('TON', 480, 'default', 'epoch');
