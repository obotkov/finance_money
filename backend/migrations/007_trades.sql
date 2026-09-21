-- Сделка внутри крипто-счёта: монета покупается за валюту счёта и остаётся на
-- нём же, отдельный счёт под монету больше не заводится. coin — что купили или
-- продали, amount — сумма в валюте счёта, received — количество монеты.
-- Покупка уменьшает остаток счёта, продажа увеличивает, а «Вложено» считается
-- только по переводам с других счетов, поэтому сделка его не двигает.
ALTER TABLE transactions ADD COLUMN coin TEXT NOT NULL DEFAULT '';

-- Старые CHECK-и заведены без имён, поэтому снимаем их по каталогу.
DO $$
DECLARE c text;
BEGIN
    FOR c IN SELECT conname FROM pg_constraint
             WHERE conrelid = 'transactions'::regclass AND contype = 'c'
    LOOP
        EXECUTE format('ALTER TABLE transactions DROP CONSTRAINT %I', c);
    END LOOP;
END $$;

ALTER TABLE transactions
    ADD CONSTRAINT transactions_amount_check CHECK (amount > 0),
    ADD CONSTRAINT transactions_type_check
        CHECK (type IN ('expense', 'income', 'transfer', 'buy', 'sell')),
    ADD CONSTRAINT transactions_shape_check CHECK (
        CASE type
            WHEN 'transfer' THEN to_account_id IS NOT NULL AND received IS NOT NULL AND coin = ''
            WHEN 'buy'      THEN to_account_id IS NULL AND received > 0 AND coin <> ''
            WHEN 'sell'     THEN to_account_id IS NULL AND received > 0 AND coin <> ''
            ELSE to_account_id IS NULL AND received IS NULL AND coin = ''
        END);
