-- Позиции в пулах ликвидности (концентрированная ликвидность, как в Uniswap v3)
-- внутри крипто-портфеля. Пара монет: цена — сколько монеты B за одну монету A,
-- интервал [price_min, price_max] задаётся при открытии и не меняется. Сколько
-- монет сейчас в позиции, не хранится: это считается из суммарной ликвидности
-- событий и текущего курса.
CREATE TABLE lp_positions (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    portfolio_id BIGINT NOT NULL,
    coin_a       TEXT NOT NULL,
    coin_b       TEXT NOT NULL CHECK (coin_b <> coin_a),
    price_min    NUMERIC(38, 18) NOT NULL CHECK (price_min > 0),
    price_max    NUMERIC(38, 18) NOT NULL CHECK (price_max > price_min),
    -- протокол или площадка: «Uniswap v3», может быть пустым
    place        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, user_id),
    CONSTRAINT lp_positions_portfolio_fkey FOREIGN KEY (portfolio_id, user_id)
        REFERENCES crypto_portfolios (id, user_id) ON DELETE CASCADE
);

-- События позиции: внесение, вывод части ликвидности, сбор комиссий. Монеты
-- ходят между позицией и строками монет портфеля (crypto_assets); asset_* —
-- что событие сделало с этими строками, чтобы удаление события вернуло ровно это.
CREATE TABLE lp_events (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    position_id BIGINT NOT NULL,
    kind        TEXT NOT NULL CHECK (kind IN ('deposit', 'withdraw', 'fees')),
    day         DATE NOT NULL,
    price       NUMERIC(38, 18) NOT NULL CHECK (price > 0),
    amount_a    NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (amount_a >= 0),
    amount_b    NUMERIC(38, 18) NOT NULL DEFAULT 0 CHECK (amount_b >= 0),
    -- изменение ликвидности позиции: плюс у внесения, минус у вывода
    liquidity   DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- изменение вложенного в позицию, в USDT
    invested    NUMERIC(24, 8) NOT NULL DEFAULT 0,
    asset_a     NUMERIC(38, 18) NOT NULL DEFAULT 0,
    asset_b     NUMERIC(38, 18) NOT NULL DEFAULT 0,
    asset_inv_a NUMERIC(24, 8) NOT NULL DEFAULT 0,
    asset_inv_b NUMERIC(24, 8) NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT lp_events_position_fkey FOREIGN KEY (position_id, user_id)
        REFERENCES lp_positions (id, user_id) ON DELETE CASCADE
);

CREATE INDEX lp_positions_user_idx ON lp_positions (user_id, portfolio_id, id);
CREATE INDEX lp_events_position_idx ON lp_events (position_id, day, id);
