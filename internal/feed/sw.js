"use strict";
// Service worker for cc-proxy push notifications.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", event => event.waitUntil(self.clients.claim()));

self.addEventListener("push", event => {
  let message = {};
  try {
    message = event.data ? event.data.json() : {};
  } catch {
    message = { body: event.data.text() };
  }
  event.waitUntil(self.registration.showNotification(message.title || "cc-proxy", {
    body: message.body || "",
    tag: message.tag,
    icon: "icon.png",
    data: { url: message.url || "" },
  }));
});

self.addEventListener("notificationclick", event => {
  event.notification.close();
  const url = new URL(event.notification.data.url || "", self.registration.scope);
  event.waitUntil((async () => {
    const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const open = windows.find(client => new URL(client.url).pathname === url.pathname);
    if (open) {
      open.postMessage({ hash: url.hash });
      return open.focus();
    }
    return self.clients.openWindow(url.href);
  })());
});
