-- Свои монеты: кроме встроенного списка (coinIDs в rates.go) монету можно найти
-- в CoinGecko и добавить в отслеживание. Монета заводится один раз на весь
-- сервер — курсы общие, — а в список пользователя попадает через user_coins.
-- Тикер (code) у монеты один: второй монете с тем же тикером места нет.
CREATE TABLE coins (
    code       TEXT PRIMARY KEY,
    gecko_id   TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    image      TEXT NOT NULL DEFAULT '', -- логотип с CoinGecko
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_coins (
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code       TEXT NOT NULL REFERENCES coins (code) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, code)
);
