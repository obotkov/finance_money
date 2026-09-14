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

Сайт: https://okayconnect.online (www и http редиректятся туда).

Домены задаются в `.env` на сервере: `DOMAIN=okayconnect.online, www.okayconnect.online` —
Caddy сам получает и продлевает сертификаты Let's Encrypt (хранятся в томе `caddy_data`).
