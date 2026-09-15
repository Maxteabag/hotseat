(()=>{
function stoppedPanel(items) {
  if (!items || !items.length) return "";
  const blockedWork = items.filter(i => i.pending);
  const usage = items.filter(i => !i.pending && i.cause !== "queue_paused");
  const idlePause = items.filter(i => !i.pending && i.cause === "queue_paused");
  const clarpReady = usage.filter(i => i.kind === "clarp" && i.readiness && i.readiness.ready);

  // Blocked work is always shown in full; it is the part someone is waiting on.
  const shown = [...blockedWork, ...usage.slice(0, 6)];
  const hidden = items.length - shown.length;

  const starving = {};
  for (const i of items) {
    if (i.cause !== "waiting_for_account") continue;
    const r = i.readiness || {};
    if (r.ready) continue;
    (starving[i.model || "unknown"] ||= []).push(i.name);
  }
  const starvingNote = Object.entries(starving).map(([model, names]) =>
    `<div class="detail"><span style="color:var(--warn)">No account can serve
      <b>${esc(model)}</b>, wanted by ${esc(names.join(", "))}. Clarp needs one account
      to serve every parked agent at once, so these hold up the other parked agents
      too.</span></div>`).join("");

  return `<div class="panel">
    <h2>Stopped work</h2>
    <p class="cap">
      ${blockedWork.length ? `<b>${blockedWork.length} agent(s) have turns queued that
        cannot run</b>, because stopping a turn leaves the queue paused and a new
        prompt only queues behind it. Clear that in the Clarp app. ` : ""}
      ${usage.length} stopped when an account ran out.
      ${clarpReady.length ? `${clarpReady.length} can be continued with
        <code>hotseat resume --go</code>.` : ""}
      ${idlePause.length ? `${idlePause.length} more have a paused queue holding
        nothing, which is leftover state rather than stalled work.` : ""}
    </p>
    <div>${shown.map(stoppedRow).join("")}</div>
    ${starvingNote}
    ${hidden > 0 ? `<div class="detail"><span>and ${hidden} more —
      <code>hotseat resume</code> lists them all</span></div>` : ""}
  </div>`;
}

function clarpPanel(c) {
  if (!c || !c.total) return "";
  const backends = Object.entries(c.by_backend || {})
    .sort((a, b) => b[1] - a[1]).map(([b, n]) => `${n} ${b}`).join(", ");
  const live = (c.agents || []).filter(a => a.live);

  const spending = Object.entries(c.live_by_account || {})
    .map(([alias, n]) => `<span><b>${esc(alias)}</b> ${n} agent${n === 1 ? "" : "s"}</span>`)
    .join("");

  const rows = live.length
    ? `<div class="legend">${live.map(a => `
        <span class="swatch" style="background:var(--ok)"></span>
        <span>${esc(a.persona || a.session)}</span>
        <span class="tok">${esc(a.backend)}</span>
        <span class="shr">${esc(a.account || "default")}</span>`).join("")}</div>`
    : `<p class="cap" style="margin:0">None are running, so no Clarp agent is drawing
        quota right now. They start a backend process only when given work.</p>`;

  return `<div class="panel">
    <h2>Clarp agents</h2>
    <p class="cap">${c.total} defined (${esc(backends)}); ${c.live} running.
      ${c.claude_backed} are Claude-backed and share these accounts.</p>
    ${rows}
    ${spending ? `<div class="detail">${spending}</div>` : ""}
  </div>`;
}


(window.hotseatPlugins ||= []).push(function(snapshot) {
 const data=(snapshot.extensions || {}).clarp;
 if (!data) return "";
 if (data.error) return `<p class="note">${esc(data.error)}</p>`;
 return clarpPanel(data.agents)+stoppedPanel(data.stopped);
});
})();
