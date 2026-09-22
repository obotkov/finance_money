-- Курсы по дням для графиков: последний курс за день (рубли за единицу).
-- Каждое обновление курсов пишет сегодняшний день, прошлые дни монет
-- подкачиваются из CoinGecko, фиат копится с первого дня.
CREATE TABLE rate_history (
    code TEXT NOT NULL,
    day  DATE NOT NULL,
    rub  NUMERIC(24, 8) NOT NULL CHECK (rub > 0),
    PRIMARY KEY (code, day)
);
