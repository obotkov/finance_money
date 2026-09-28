// Service worker Penny: только пуш-уведомления (о бюджетах), без кэша страницы.
// Сообщение от сервера — JSON {title, body, url, tag}, см. backend/push.go.

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));

self.addEventListener("push", event => {
  let msg = {};
  try { msg = event.data ? event.data.json() : {}; } catch (_) { msg = { body: event.data ? event.data.text() : "" }; }
  event.waitUntil(self.registration.showNotification(msg.title || "Penny", {
    body: msg.body || "",
    tag: msg.tag || undefined,
    icon: "/icons/icon-192.png",
    badge: "/icons/icon-192.png",
    data: { url: msg.url || "/" }
  }));
});

// Нажатие открывает нужный раздел: уже открытую вкладку Penny, иначе новую.
self.addEventListener("notificationclick", event => {
  event.notification.close();
  const url = new URL((event.notification.data && event.notification.data.url) || "/", self.location.origin).href;
  event.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(list => {
    const tab = list.find(c => new URL(c.url).origin === self.location.origin);
    if (tab) return tab.navigate(url).then(c => (c || tab).focus());
    return self.clients.openWindow(url);
  }));
});
