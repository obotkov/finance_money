# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Penny is a multi-user personal finance tracker, live at https://penny.place. The UI, code comments, commit messages, README and user-facing errors are all in Russian — keep writing them in Russian. README.md is the detailed feature spec (API table, CSV format, how every summary/crypto number is computed); update it alongside behaviour changes.

## Commands

```bash
# backend (Go, module in backend/)
cd backend && go vet ./... && go test ./...
cd backend && go test -run TestParseCoinGeckoChart ./...   # single test
cd backend && gofmt -l .                                    # must print nothing

# frontend: static page, no build step. Serve web/ and open it — with no API it runs in demo mode
cd web && python3 -m http.server 8090

# full stack locally: copy .env.example to .env and set DB_PASSWORD (DOMAIN=:80 serves plain http)
docker compose up -d --build

# deploy: pushes nothing itself — the server pulls origin/main and rebuilds containers
git push && ./deploy.sh
```

`go build` in `backend/` leaves a `backend/backend` binary — don't commit it. Tests are pure unit tests (validation, parsers); there are no DB-backed tests.

Established workflow in this repo: after each finished feature — commit, `git push`, `./deploy.sh`, then check the live page with `curl -s https://penny.place/ | grep -c <new identifier>` and `curl https://penny.place/api/auth/config` (200) / `/api/state` (401 without a session). Don't ssh into the production server or query its DB without asking the user first. The server's disk is small (10 GB): `deploy.sh` prunes Docker build cache older than 3 days and prints `df` at the end — if `/api/health` returns 502 after a deploy, check disk space first (a full disk once crash-looped Postgres).

## Architecture

Three containers (`docker-compose.yml`): `web` (Caddy — TLS, serves `web/` statically, proxies `/api/*` to `api:8080`), `api` (Go binary from `backend/`), `db` (Postgres 18). Secrets live only in `.env` on the server.

### Backend (`backend/`, single `main` package)

- `store.go` — all SQL (pgx). `Store.State(uid)` builds the whole per-user snapshot: accounts, categories, transactions, crypto portfolios, shared rates, and rate `history`.
- `handlers.go` — routes (Go 1.22 `mux` patterns). Every mutating endpoint is wrapped in `withState` (settings-like writes) or `withData` (anything that changes the user's financial data — see `backup.go`), so **every write responds with the full state**; the page replaces its state from the response rather than patching locally. Writes from another `Origin` than `PUBLIC_URL` are refused and non-GET bodies must be JSON.
- Migrations: `migrations/NNN_*.sql` are embedded and applied at startup in filename order, each once, tracked in `schema_migrations`. Add a new numbered file; never edit an applied one. A failing migration stops the api from starting.
- Account balances are stored denormalized in `accounts.balance`. Every transaction write goes through `insertTx`/`removeTx`, which apply and roll back the balance effect; an update is remove + insert under the same id. Any new transaction field that affects money must be handled on both sides (see how `close_price` is added in `insertTx` and subtracted in `removeTx` in SQL so rounding matches).
- Transaction types: `expense`, `income`, `transfer` (two accounts, `received` in the target currency), `buy`/`sell` (crypto trade inside one «Криптокошелёк» account: `amount` in account currency, `received` coin quantity, `coin`). A closed buy carries `close_price`/`close_date` and returns the proceeds to the balance.
- `rates.go` — `RateUpdater` refreshes RUB rates hourly (CBR for USD/EUR, CoinGecko for coins in `coinIDs`), writes today's price into `rate_history`, and backfills 31 days of coin history from CoinGecko's `market_chart`, spaced 15 s apart for the free API rate limit. `coinIDs` is the list of built-in coins; the frontend `COINS` list must match it. Users can add more coins found on CoinGecko (`coins.go`: `coins`/`user_coins` tables, in-memory `extraCoins`) — always look coins up with `coinID(code)`/`allCoinIDs()`/`isCurrency(code)`, never `coinIDs` directly; on the page `registerCoins()` pushes them into `COINS`/`COIN`/`CUR`/`CRYPTO_CURS`/`COIN_IMG`.
- `auth.go`/`users.go` — sign-in is Google OAuth only (no passwords; legacy password accounts are claimed by the first Google sign-in with the same email); session cookie whose SHA-256 is stored. `admin.go` — CLI subcommands (`/api users`, `/api healthcheck`).
- `backup.go` — per-user backups (`backups` table, gzip JSON snapshot with NUMERIC as strings); restore remaps ids and runs in one transaction. `users.current_backup_id` is the version the data matches; restoring from the list switches to it without creating a backup, and every data write clears it (`withData` in handlers.go, and the CSV import) — new data-changing routes must use `withData`, not `withState`.
- `recurring.go` — recurring expense/income rules (`recurring` table); `Store.RunRecurring` (started in main.go, every 15 min) writes due operations through `insertTx` on Moscow dates. The n-th date is start + n steps (`occurrence`/`nextAfter`); the page mirrors this in `rcOccurrence`/`rcNextAfter` — keep them in sync.
- `budgets.go` — monthly per-category limits (spent = the category subtree's expenses this Moscow month, in RUB); `push.go` — Web Push (`webpush-go`, VAPID keys generated once into `server_settings`), `Pusher.CheckBudgets` runs in the background after every `withData` write, the CSV import and recurring runs, each alert once per month (`budget_alerts`). The page mirrors the spending rule in `budgetUsage()`; `web/sw.js` shows the push.
- `pools.go` — liquidity-pool positions owned by a crypto portfolio or a crypto account (`lp_positions.portfolio_id` xor `account_id`; `lp_events`, Uniswap v3 math in `poolAmounts`/`poolLiquidity`, mirrored on the page). Current coins are computed from summed liquidity and the price, never stored. Events move the owner's coins via `poolRow.move` — portfolio `crypto_assets` rows, or for an account its `balance` (its own currency) and its derived coin holdings (trades + pool event deltas, `accountCoinHeld`; on the page `holdings()` adds the same deltas) — and record those deltas (`asset_*`) so undoing the last event or deleting a position restores them exactly (revert moves coins before deleting the event). Event dates must not go back in time. Pools are in backups (`backupPortfolio.Pools`, `backupAccount.Pools`). On the page a pool owner is a ref string `"p:<portfolio id>"` / `"a:<account name>"` (`poolOwner`).
- `import.go` — CSV import in the same format the page exports (format documented in README).

### Frontend (`web/index.html`)

One ~7k-line file exported from Claude Design and run by `support.js` (generated dc-runtime on React — do not edit). Structure:

- Markup inside `<x-dc>…</x-dc>` is a template: `{{ expr }}` interpolation, `<sc-if value="{{ flag }}">`, `<sc-for list="{{ items }}" as="x">`. Templates can't evaluate expressions like `!x` — expose explicit booleans (e.g. `noClose` alongside `canClose`). Event attributes (`onClick`, `onChange`, `onMouseEnter`/`onMouseLeave`) take handler functions from the values.
- Logic is `class Component extends DCLogic` in `<script type="text/x-dc">`. `renderVals()` computes **every** template value on each render from `this.state`; larger areas are split into helper methods merged in (`...this.summaryVals()`, `...this.closeVals(...)`, `...this.moversVals()`). Nothing is cached — keep per-render work proportional.
- Data flow: on load it fetches `/api/state`; if that fails it falls back to the `DEMO` object (demo mode, changes kept in memory only). Mutations call `this.api(method, path, body)` when `state.online`; otherwise the same handler updates local state. New features should implement both branches.
- Money model on the page: `t.v` is the signed effect on the source account (a buy is negative); always use `txEffect(t, accName)` for balance effects (it includes closed-trade proceeds), `holdings(acc)` for coins on a crypto account, `isTrade`/`isFlow`/`isClosed` to classify. Trades are excluded from income/expense analytics. Conversions go through `toRub`/`coinRub`/`rubOn(code, day)` (historical, from `state.history`).
- Styling: CSS custom properties on `:root` with a dark-mode override block; reuse existing classes (`card`, `btn btn-p|btn-s|btn-t`, `seg`/`segopt`, `rowline`, `num`, `tone[data-tone]`, `coin` badges via `curBadge(code)`). Mobile uses `isNarrow()` (≤760px) and the `.m-only`/`.d-only` classes; check both widths.
- Template values, state keys and CSS classes all share one flat namespace in this file. Before adding a name, grep that it doesn't exist yet — collisions have broken screens silently (a new `accTo` state key clobbered the transfer form's, a new `.accrow` class restyled the accounts table).
- Date fields don't use `<input type="date">`: they are `.dfield` buttons registered in `dateFields()` that open the shared `.dpop` calendar popover (positioned by `popPos`). Add new date fields there.
- On ≤760px all inputs are forced to 16px — smaller fonts make iPhone zoom the page on focus.
- Coin logos: `web/coins/<code>.svg` plus the code in `COIN_LOGO`; user-added coins use their CoinGecko image (`COIN_IMG`).

`_ds/` holds an exported Claude Design system bundle; the app page does not use it.

### Verifying UI changes

Serve `web/` locally and use demo mode (no login required). Screens are switched through the `.navitem` sidebar on desktop and the bottom bar / «Ещё» on mobile; the last screen is remembered in `localStorage` (`?screen=<id>` opens one directly). Don't enter passwords or create accounts through the browser.

Full-stack checks without production: run Postgres in Docker, start the api binary with `DATABASE_URL=… ADDR=:8092 PUBLIC_URL=http://localhost:8091` (migrations apply on start), insert a user and a session row whose `token_hash` is `sha256('<token>')` into that local DB, and call the API with `Cookie: session=<token>` and `Origin: http://localhost:8091`. To use the page against it, serve `web/` and proxy `/api/` to the binary on the `PUBLIC_URL` port, then set the cookie in the browser. CoinGecko's free API allows only a few calls a minute — a fresh DB backfilling history plus manual tests will hit 429s; that's not a bug.
