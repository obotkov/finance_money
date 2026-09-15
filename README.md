# Ведомость — учёт финансов

Личный учёт: счета, операции, категории, бюджеты и аналитика. Сайт: https://okayconnect.online

## Устройство

| Сервис | Что делает |
| --- | --- |
| `web` | Caddy: HTTPS, вход по паролю (basic auth), страница из `web/`, прокси `/api/*` на `api` |
| `api` | Go-бэкенд из `backend/`: счета, операции, категории, импорт CSV, курсы валют |
| `db` | PostgreSQL 18, данные в томе `pgdata` |

Страница `web/index.html` — экспорт из Claude Design. При открытии она берёт данные с `/api/state`,
а изменения отправляет в API. Без бэкенда (например, в холсте Claude Design) показывает демо-данные.

Курсы к рублю: доллар и евро — ЦБ РФ, BTC/ETH/TON/USDT — CoinGecko. Обновляются при старте и раз в час,
вручную — кнопкой «Обновить» в настройках. Если источник недоступен, остаются последние курсы.

### API

Все изменения отвечают полным состоянием (`accounts`, `cats`, `txs`, `rates`).

| Метод | Путь | Тело |
| --- | --- | --- |
| GET | `/api/state` | |
| POST, PUT `/{id}`, DELETE `/{id}` | `/api/accounts` | `{name, kind, balance, cur}` |
| POST, DELETE `/{id}` | `/api/transactions` | `{date, title, type: expense\|income\|transfer, category, account, toAccount, amount, received}` |
| POST, PUT `/{id}`, DELETE `/{id}` | `/api/categories` | `{name, parent, kind: expense\|income}` |
| POST | `/api/import` | `{text}` — CSV в формате экспорта |
| POST | `/api/rates/refresh` | `{}` |

Операция меняет баланс счёта, удаление операции откатывает изменение.

## Локально

```bash
cp .env.example .env   # задать AUTH_HASH и DB_PASSWORD
docker compose up --build
```

Открыть http://localhost. Тесты бэкенда: `cd backend && go test ./...`

## Деплой

Сервер: `root@94.103.87.49` (ключ `~/.ssh/wildex-deploy`), приложение в `/opt/finance-money`,
настройки в `/opt/finance-money/.env` (домены, логин и хеш пароля, пароль базы).

```bash
git push && ./deploy.sh
```

Сменить пароль входа: получить хеш через
`docker run --rm caddy:2-alpine caddy hash-password --plaintext 'новый пароль'`,
вписать его в `AUTH_HASH='...'` в `.env` на сервере и запустить `./deploy.sh`.
