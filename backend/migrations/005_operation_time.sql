-- Time of day of an operation, when known (imported, or added in the app for today).
ALTER TABLE transactions ADD COLUMN time TIME(0);

-- Newest first: operations without a time go last within their day.
DROP INDEX transactions_user_date_idx;
CREATE INDEX transactions_user_date_idx ON transactions (user_id, date DESC, time DESC NULLS LAST, id DESC);
