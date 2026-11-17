// The Decisions page's service worker: shows the phone alerts the daemon sends
// this paired device (zm SPEC 16 "Phone alerts") and opens the page on a tap.
// An alert carries a title, a line and where to go; never the item itself.
self.addEventListener("push", (event) => {
  let msg = {};
  try {
    msg = event.data ? event.data.json() : {};
  } catch {
    msg = {};
  }
  event.waitUntil(
    self.registration.showNotification(msg.title || "Decisions", {
      body: msg.body || "Open Decisions",
      icon: "/v0/approvals/icon.png",
      data: { url: "/v0/approvals" },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || "/v0/approvals";
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((wins) => {
      for (const w of wins) {
        if (new URL(w.url).pathname === url && "focus" in w) return w.focus();
      }
      return self.clients.openWindow(url);
    }),
  );
});
