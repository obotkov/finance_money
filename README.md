# Ведомость — учёт финансов

Учёт для нескольких людей: у каждого свои счета, операции, категории, бюджеты и аналитика.
Сайт: https://okayconnect.online

## Устройство

| Сервис | Что делает |
| --- | --- |
| `web` | Caddy: HTTPS, страница из `web/`, прокси `/api/*` на `api` |
| `api` | Go-бэкенд из `backend/`: пользователи и вход, счета, операции, категории, импорт CSV, курсы валют |
| `db` | PostgreSQL 18, данные в томе `pgdata` |

Страница `web/index.html` — экспорт из Claude Design. Без входа она показывает экран входа и регистрации,
после входа берёт данные пользователя с `/api/state` и отправляет изменения в API.
Без бэкенда (например, в холсте Claude Design) показывает демо-данные.

Курсы к рублю общие для всех: доллар и евро — ЦБ РФ, BTC/ETH/TON/USDT — CoinGecko.
Обновляются при старте и раз в час, вручную — кнопкой «Обновить» в настройках.

## Вход

- **Почта и пароль.** Пароль — bcrypt, не короче 8 символов. Почта не подтверждается (нет SMTP).
- **Google** — если заданы `GOOGLE_CLIENT_ID` и `GOOGLE_CLIENT_SECRET`. Google-аккаунт с той же почтой,
  что у существующего аккаунта с паролем, забирает его себе: пароль стирается, другие сессии завершаются —
  иначе чужой человек мог бы заранее зарегистрироваться на ваш адрес.
- Сессия — cookie `session` (HttpOnly, Secure, SameSite=Lax) на 30 дней, в базе только SHA-256 токена.
- Лимиты за 15 минут: 10 неудачных входов на почту, 30 на IP, 10 регистраций на IP.

### Как включить вход через Google

1. [Google Cloud Console](https://console.cloud.google.com/) → APIs & Services → OAuth consent screen:
   тип External, название, почта поддержки; scopes `openid`, `email`, `profile`. Опубликуйте приложение
   (Publish app), иначе войти смогут только тестовые пользователи.
2. Credentials → Create credentials → OAuth client ID → Web application.
   Authorized redirect URI: `https://okayconnect.online/api/auth/google/callback`.
3. Впишите Client ID и Client secret в `/opt/finance-money/.env` на сервере:
   `GOOGLE_CLIENT_ID=...`, `GOOGLE_CLIENT_SECRET=...` — и запустите `./deploy.sh`.

## API

Данные — только для вошедшего пользователя (иначе 401). Изменения отвечают полным состоянием
(`user`, `accounts`, `cats`, `txs`, `rates`).

| Метод | Путь | Тело |
| --- | --- | --- |
| POST | `/api/auth/register` | `{email, password, name}` |
| POST | `/api/auth/login` | `{email, password}` |
| POST | `/api/auth/logout` | `{}` |
| GET | `/api/auth/me`, `/api/auth/config` | |
| GET | `/api/auth/google/start` → Google → `/api/auth/google/callback` | |
| GET | `/api/state` | |
| POST, PUT `/{id}`, DELETE `/{id}` | `/api/accounts` | `{name, kind, balance, cur}` |
| POST, DELETE `/{id}` | `/api/transactions` | `{date, title, type: expense\|income\|transfer, category, account, toAccount, amount, received}` |
| POST, PUT `/{id}`, DELETE `/{id}` | `/api/categories` | `{name, parent, kind: expense\|income}` |
| POST | `/api/import` | `{text}` — CSV в формате экспорта |
| POST | `/api/rates/refresh` | `{}` |

## Локально

```bash
cp .env.example .env   # задать DB_PASSWORD
docker compose up --build
```

Открыть http://localhost. Тесты бэкенда: `cd backend && go test ./...`

## Деплой

Сервер: `root@94.103.87.49` (ключ `~/.ssh/wildex-deploy`), приложение в `/opt/finance-money`,
настройки в `/opt/finance-money/.env` (домены, `PUBLIC_URL`, ключи Google, пароль базы).

```bash
git push && ./deploy.sh
```
