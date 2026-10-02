// Cloudflare Worker — посредник между сервером Penny и Telegram Bot API.
// Нужен, когда сервер не может напрямую достучаться до api.telegram.org.
// Пересылает запрос как есть (метод, заголовки, тело) и возвращает ответ.
// Адрес воркера (https://<имя>.<аккаунт>.workers.dev) прописывается
// в .env сервера как TELEGRAM_API_URL. Ничего не хранит и не пишет в логи.

// Номер вашего бота — цифры до двоеточия в токене (123456789:AA… → "123456789").
// Тогда чужие боты через этот воркер ходить не смогут. Пусто — пропускать любых.
const BOT_ID = "";

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const allowed = BOT_ID ? url.pathname.startsWith("/bot" + BOT_ID + ":") : url.pathname.startsWith("/bot");
    if (!allowed) return new Response("Not Found", { status: 404 });
    url.protocol = "https:";
    url.hostname = "api.telegram.org";
    url.port = "";
    return fetch(new Request(url, request));
  },
};
