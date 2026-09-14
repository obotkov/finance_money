# Ведомость — учёт финансов

Прототип из Claude Design (`*.dc.html` + рантайм `support.js`), раздаётся как статика через Caddy в Docker.

## Локально

```bash
DOMAIN=:80 docker compose up --build
```

Открыть http://localhost

## Деплой

Сервер: `root@94.103.87.49`, приложение в `/opt/finance-money`, настройки в `/opt/finance-money/.env`.

```bash
git push && ./deploy.sh
```

Домен задаётся в `.env` на сервере (`DOMAIN=finance.example.com`) — Caddy сам получит HTTPS-сертификат.
Пока домена нет, там стоит `DOMAIN=:80`, и сайт открывается по http://94.103.87.49
