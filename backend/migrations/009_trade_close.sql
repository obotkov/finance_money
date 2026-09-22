-- Закрытие покупки: монета продаётся целиком по close_price (в валюте счёта)
-- в день close_date, выручка возвращается на остаток счёта. Покупка и её
-- закрытие остаются одной строкой, а результат сделки — разница выручки и
-- суммы покупки.
ALTER TABLE transactions
    ADD COLUMN close_price NUMERIC(32, 12),
    ADD COLUMN close_date  DATE,
    ADD CONSTRAINT transactions_close_check CHECK (
        (close_price IS NULL AND close_date IS NULL)
        OR (type = 'buy' AND close_price > 0 AND close_date IS NOT NULL));
