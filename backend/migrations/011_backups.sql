-- Бекапы данных пользователя: снимок счетов, категорий, операций и
-- крипто-портфелей в JSON (формат — backupFile в backup.go), сжатый gzip.
-- manual — сделан кнопкой, auto — сохранён сам перед восстановлением другого.
CREATE TABLE backups (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind       TEXT NOT NULL CHECK (kind IN ('manual', 'auto')),
    accounts   INT NOT NULL,
    txs        INT NOT NULL,
    size       INT NOT NULL, -- байт в несжатом JSON, столько весит скачанный файл
    data       BYTEA NOT NULL
);

CREATE INDEX backups_user_idx ON backups (user_id, created_at DESC);
