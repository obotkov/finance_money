-- Telegram: чат пользователя для сводки и бекапов. Привязка — по одноразовому
-- коду из ссылки t.me/<бот>?start=<код>, которую выдаёт сайт.
ALTER TABLE users ADD COLUMN telegram_chat_id BIGINT;
-- как подписать чат на странице: @username или имя
ALTER TABLE users ADD COLUMN telegram_name TEXT NOT NULL DEFAULT '';
-- по понедельникам — сводка и бекап
ALTER TABLE users ADD COLUMN telegram_weekly BOOLEAN NOT NULL DEFAULT true;
-- неделя (ГГГГ-Wнн), за которую сводка уже ушла: второй раз за неделю не шлём
ALTER TABLE users ADD COLUMN telegram_sent_week TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX users_telegram_chat_idx ON users (telegram_chat_id) WHERE telegram_chat_id IS NOT NULL;

-- бекапы, ушедшие в Telegram, — отдельного вида, чтобы в списке так и подписать
ALTER TABLE backups DROP CONSTRAINT backups_kind_check;
ALTER TABLE backups ADD CONSTRAINT backups_kind_check CHECK (kind IN ('manual', 'auto', 'telegram'));

CREATE TABLE telegram_links (
    code       TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
