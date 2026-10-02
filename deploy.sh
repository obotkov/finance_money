#!/usr/bin/env bash
# Выкатывает текущий main на сервер: git pull + пересборка контейнеров.
# Использование: ./deploy.sh   (сначала git push)
set -euo pipefail

SERVER="${SERVER:-root@94.103.87.49}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/wildex-deploy}"
APP_DIR="/opt/finance-money"
REPO="https://github.com/obotkov/finance_money.git"

ssh -o IdentitiesOnly=yes -i "$SSH_KEY" "$SERVER" bash -s <<EOF
set -euo pipefail
if [ ! -d "$APP_DIR/.git" ]; then
  git clone "$REPO" "$APP_DIR"
fi
cd "$APP_DIR"
git fetch --quiet origin main
git reset --hard origin/main
[ -f .env ] || cp .env.example .env
# Пароль базы генерируется один раз и дальше не меняется
if ! grep -q '^DB_PASSWORD=.' .env; then
  sed -i '/^DB_PASSWORD=/d' .env
  echo "DB_PASSWORD=\$(openssl rand -hex 24)" >> .env
fi
docker compose up -d --build --remove-orphans
docker image prune -f >/dev/null
# кэш сборки растёт с каждой выкладкой и однажды занял весь диск — база легла;
# оставляем только последние три дня
docker builder prune -f --filter until=72h >/dev/null
docker compose ps
df -h / | tail -1
EOF
