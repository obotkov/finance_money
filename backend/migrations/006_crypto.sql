-- Крипто-портфели: кошелёк или биржа, внутри — монеты с количеством и суммой,
-- вложенной в них в USDT. Курсы монет лежат в общей таблице rates (рубли за единицу).
CREATE TABLE crypto_portfolios (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    -- биржа или кошелёк: «Binance», «Ledger», может быть пустым
    place      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name),
    UNIQUE (id, user_id)
);

CREATE TABLE crypto_assets (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    portfolio_id BIGINT NOT NULL,
    coin         TEXT NOT NULL,
    amount       NUMERIC(32, 12) NOT NULL DEFAULT 0 CHECK (amount >= 0),
    -- сколько в эту монету вложено, в USDT
    invested     NUMERIC(24, 8) NOT NULL DEFAULT 0 CHECK (invested >= 0),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (portfolio_id, coin),
    -- монета принадлежит портфелю того же пользователя
    CONSTRAINT crypto_assets_portfolio_fkey FOREIGN KEY (portfolio_id, user_id)
        REFERENCES crypto_portfolios (id, user_id) ON DELETE CASCADE
);

CREATE INDEX crypto_portfolios_user_idx ON crypto_portfolios (user_id, id);
CREATE INDEX crypto_assets_user_idx ON crypto_assets (user_id, portfolio_id, id);
