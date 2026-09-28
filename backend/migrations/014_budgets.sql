-- Бюджеты: месячный лимит трат на категорию (в рублях). Траты считаются по
-- категории вместе с подкатегориями, за календарный месяц по Москве.
CREATE TABLE budgets (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    category_id BIGINT NOT NULL,
    amount      NUMERIC(24, 8) NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, category_id),
    CONSTRAINT budgets_category_fkey FOREIGN KEY (category_id, user_id)
        REFERENCES categories (id, user_id) ON DELETE CASCADE
);

-- Уведомление о бюджете: при скольки процентах лимита и включено ли вообще.
ALTER TABLE users
    ADD COLUMN budget_alert_on  BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN budget_alert_pct INT NOT NULL DEFAULT 90 CHECK (budget_alert_pct BETWEEN 10 AND 100);

-- Какие уведомления за месяц уже ушли: warn — порог, over — превышение.
-- Каждое уходит один раз за месяц; смена лимита сбрасывает отметки месяца.
CREATE TABLE budget_alerts (
    budget_id BIGINT NOT NULL REFERENCES budgets (id) ON DELETE CASCADE,
    month     TEXT NOT NULL, -- ГГГГ-ММ
    level     TEXT NOT NULL CHECK (level IN ('warn', 'over')),
    sent_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (budget_id, month, level)
);

-- Подписки браузеров на пуш-уведомления (Web Push).
CREATE TABLE push_subscriptions (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint   TEXT NOT NULL UNIQUE,
    p256dh     TEXT NOT NULL,
    auth       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions (user_id);

-- Служебные значения сервера: ключи VAPID для Web Push создаются при первом запуске.
CREATE TABLE server_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
