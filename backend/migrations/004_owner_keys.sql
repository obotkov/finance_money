-- Operations and categories may point only at rows of the same user. Every
-- query already filters by user_id; these keys make a cross-user reference
-- impossible even if some future query forgets to.
ALTER TABLE accounts ADD CONSTRAINT accounts_id_user_key UNIQUE (id, user_id);
ALTER TABLE categories ADD CONSTRAINT categories_id_user_key UNIQUE (id, user_id);

ALTER TABLE transactions
    DROP CONSTRAINT transactions_account_id_fkey,
    DROP CONSTRAINT transactions_to_account_id_fkey,
    DROP CONSTRAINT transactions_category_id_fkey,
    ADD CONSTRAINT transactions_account_fkey
        FOREIGN KEY (account_id, user_id) REFERENCES accounts (id, user_id) ON DELETE CASCADE,
    ADD CONSTRAINT transactions_to_account_fkey
        FOREIGN KEY (to_account_id, user_id) REFERENCES accounts (id, user_id) ON DELETE CASCADE,
    ADD CONSTRAINT transactions_category_fkey
        FOREIGN KEY (category_id, user_id) REFERENCES categories (id, user_id) ON DELETE SET NULL (category_id);

ALTER TABLE categories
    DROP CONSTRAINT categories_parent_id_fkey,
    ADD CONSTRAINT categories_parent_fkey
        FOREIGN KEY (parent_id, user_id) REFERENCES categories (id, user_id) ON DELETE SET NULL (parent_id);
