(() => {
  "use strict";
  const page = document.body.dataset.page;

  if (page === "task") {
    const taskID = document.body.dataset.taskId;
    const projectionVersion = document.body.dataset.projectionVersion;
    const timeline = document.querySelector("#timeline");
    const streamState = document.querySelector("#stream-state");
    const eventTypes = ["task_created", "state_transitioned", "provider_message", "approval_requested", "approval_resolved", "auth_required", "attachment_added", "diff_summary", "verification", "commit_created", "push_completed", "deployment", "failure"];
    let source = null;
    const existingRows = timeline.querySelectorAll("li[data-event-id]");
    let lastEventID = existingRows.length ? existingRows[existingRows.length - 1].dataset.eventId : "";
    let reconnectDelay = 1000;
    let reconnectTimer = null;

    const appendEvent = (event) => {
      lastEventID = event.lastEventId || lastEventID;
      let payload = {};
      try { payload = JSON.parse(event.data); } catch (_) { payload = { message: event.data }; }
      if (payload.projection_version && String(payload.projection_version) !== projectionVersion) {
        window.location.reload();
        return;
      }
      const row = document.createElement("li");
      row.dataset.eventId = lastEventID;
      const stamp = document.createElement("time");
      stamp.textContent = new Date().toLocaleTimeString("tr-TR", { hour12: false });
      const body = document.createElement("div");
      const kind = document.createElement("strong");
      kind.textContent = event.type;
      const message = document.createElement("pre");
      message.textContent = payload.message || payload.summary || JSON.stringify(payload);
      body.append(kind, message);
      row.append(stamp, body);
      timeline.querySelector(".empty-row")?.remove();
      timeline.append(row);
    };

    const reconnect = () => {
      const cursor = lastEventID ? `?last_event_id=${encodeURIComponent(lastEventID)}` : "";
      source = new EventSource(`/api/v1/tasks/${encodeURIComponent(taskID)}/stream${cursor}`);
      source.onopen = () => {
        reconnectDelay = 1000;
        streamState.textContent = "Canlı bağlantı aktif";
      };
      eventTypes.forEach((type) => source.addEventListener(type, appendEvent));
      source.addEventListener("reset", () => window.location.reload());
      source.onerror = () => {
        source?.close();
        source = null;
        streamState.textContent = `Bağlantı kesildi · ${Math.round(reconnectDelay / 1000)} sn`;
        clearTimeout(reconnectTimer);
        reconnectTimer = window.setTimeout(reconnect, reconnectDelay);
        reconnectDelay = Math.min(reconnectDelay * 2, 30000);
      };
    };

    reconnect();
    window.addEventListener("pagehide", () => {
      clearTimeout(reconnectTimer);
      source?.close();
    }, { once: true });
  }

  if (page === "auth") {
    const provider = document.body.dataset.provider;
    const button = document.querySelector("#start-recovery");
    const output = document.querySelector("#recovery-output");
    button?.addEventListener("click", async () => {
      button.disabled = true;
      output.querySelector("strong").textContent = "Recovery hazırlanıyor";
      output.querySelector("pre").textContent = "Güvenli CSRF state alınıyor…";
      try {
        const csrfResponse = await fetch("/api/v1/csrf", { credentials: "same-origin", cache: "no-store" });
        if (!csrfResponse.ok) throw new Error("CSRF state alınamadı");
        const { token } = await csrfResponse.json();
        const response = await fetch(`/api/v1/auth/${encodeURIComponent(provider)}/recovery`, {
          method: "POST", credentials: "same-origin", cache: "no-store",
          headers: { "Content-Type": "application/json", "X-CSRF-Token": token }, body: "{}"
        });
        if (!response.ok) throw new Error("Recovery session başlatılamadı");
        const recovery = await response.json();
        output.querySelector("strong").textContent = recovery.state || "Çalışıyor";
        output.querySelector("pre").textContent = recovery.prompt || "CLI login prompt’u bekleniyor.";
      } catch (error) {
        output.querySelector("strong").textContent = "Recovery başarısız";
        output.querySelector("pre").textContent = error.message;
        button.disabled = false;
      }
    });
  }
})();
