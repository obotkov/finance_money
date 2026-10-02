-- Пул может жить и на крипто-счёте: монеты в него уходят с остатка счёта (в его
-- валюте) и из купленных на нём монет, а при выводе возвращаются туда же.
-- У позиции ровно один хозяин — портфель или счёт.
ALTER TABLE lp_positions ALTER COLUMN portfolio_id DROP NOT NULL;
ALTER TABLE lp_positions ADD COLUMN account_id BIGINT REFERENCES accounts (id) ON DELETE CASCADE;
ALTER TABLE lp_positions ADD CONSTRAINT lp_positions_owner_check CHECK ((portfolio_id IS NULL) <> (account_id IS NULL));
CREATE INDEX lp_positions_account_idx ON lp_positions (account_id) WHERE account_id IS NOT NULL;
