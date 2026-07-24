(() => {
  'use strict';

  const storageKey = 'kovan.desktop.workspace.v1';
  const state = { setup: loadSetup(), task: null, events: [], approvals: [], devices: [], pairing: { challenge: null }, deviceManager: { rotateDeviceID: '' }, poll: null, observeInFlight: false };
  const $ = (selector) => document.querySelector(selector);

  const bridgeRequest = async (path, options = {}) => {
    if (window.agentbridge && typeof window.agentbridge.request === 'function') {
      return window.agentbridge.request({ path, method: options.method || 'GET', body: options.body || null });
    }
    const response = await fetch(path, {
      method: options.method || 'GET',
      headers: options.body ? { 'Content-Type': 'application/json' } : {},
      body: options.body ? JSON.stringify(options.body) : undefined,
    });
    const value = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(value.error || `Local API request failed (${response.status})`);
    return value;
  };

  function loadSetup() {
    try {
      const setup = JSON.parse(localStorage.getItem(storageKey)) || {};
      if (setup.lastTask && String(setup.lastTask.id || '').startsWith('task-demo-')) delete setup.lastTask;
      return setup;
    } catch (_) { return {}; }
  }
  function saveSetup() { localStorage.setItem(storageKey, JSON.stringify(state.setup)); }
  function idempotency(prefix) { return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`; }
  function showToast(message, error = false) {
    const toast = $('#toast'); toast.textContent = message; toast.className = `toast show${error ? ' error' : ''}`;
    clearTimeout(showToast.timer); showToast.timer = setTimeout(() => { toast.className = 'toast'; }, 3400);
  }
  function render() {
    const task = state.task;
    $('#workspace-name').textContent = state.setup.projectName || 'Untitled workspace';
    $('#activity-count').textContent = String(state.events.length).padStart(2, '0');
    $('#stat-running').textContent = task && ['running', 'preparing', 'verifying', 'committing', 'pushing'].includes(task.state) ? '01' : '00';
    $('#stat-approval').textContent = task && (task.state === 'awaiting_approval' || state.approvals.some((approval) => approval.task_id === task.id)) ? '01' : '00';
    $('#stat-completed').textContent = task && task.state === 'completed' ? '01' : '00';
    $('#stat-cursor').textContent = state.events.length ? String(state.events[state.events.length - 1].cursor).padStart(2, '0') : '—';
    $('#task-count').textContent = task ? '1 task' : '0 tasks';
    $('#board-list .board-row:first-child .board-total').textContent = task ? '01' : '00';
    $('#task-stack').innerHTML = task ? taskCard(task) : '';
    $('#empty-board').hidden = Boolean(task);
    renderDevices();
    renderDeviceManager();
    renderInspector(task);
    bindTaskActions();
  }
  function renderDevices() {
    const local = { id: 'local-mac', name: 'Mac · local', state: 'paired' };
    const devices = [local, ...state.devices.filter((device) => device.id !== local.id)];
    const ready = devices.find((device) => device.state === 'paired') || local;
    const remoteCount = devices.filter((device) => device.id !== local.id).length;
    const status = ready.state === 'paired' ? (remoteCount ? `${remoteCount} paired device${remoteCount === 1 ? '' : 's'}` : 'ready for work') : String(ready.state || 'unreachable');
    const card = $('#device-card');
    card.innerHTML = `<span class="device-orb ${escapeHTML(ready.state)}"></span><div><strong>${escapeHTML(ready.name)}</strong><span>${escapeHTML(status)}</span></div><span class="device-menu">···</span>`;
  }
  function renderDeviceManager() {
    const list = $('#managed-device-list');
    if (!list) return;
    const local = { id: 'local-mac', name: 'Mac · local', kind: 'local_mac', state: 'paired', fingerprint: 'controller authority' };
    const devices = [local, ...state.devices.filter((device) => device.id !== local.id)];
    if (!devices.length) {
      list.innerHTML = '<div class="device-list-empty">No execution devices are enrolled yet.</div>';
      return;
    }
    list.innerHTML = devices.map((device) => {
      const localDevice = device.id === local.id;
      const stateClass = escapeHTML(device.state || 'unknown');
      let actions = '<span class="device-owner-label">controller</span>';
      if (!localDevice && device.state !== 'revoked') {
        const reachability = device.state === 'paired'
          ? '<button type="button" class="device-action" data-device-action="unreachable">mark unreachable</button>'
          : '<button type="button" class="device-action" data-device-action="paired">mark reachable</button>';
        actions = `${reachability}<button type="button" class="device-action" data-device-action="rotate">rotate key</button><button type="button" class="device-action danger" data-device-action="revoke">revoke</button>`;
      } else if (!localDevice) {
        actions = '<span class="device-owner-label">re-enroll with a fresh challenge</span>';
      }
      return `<article class="managed-device-row" data-device-id="${escapeHTML(device.id)}"><div class="managed-device-main"><span class="device-orb ${stateClass}"></span><div><strong>${escapeHTML(device.name || device.id)}</strong><span>${escapeHTML(device.id)} · ${escapeHTML(device.state || 'unknown')}</span></div></div><div class="managed-device-meta"><span>${escapeHTML(localDevice ? device.fingerprint : `key ${shortID(device.fingerprint)}`)}</span><span>epoch ${escapeHTML(device.connection_epoch || '—')} · rev ${escapeHTML(device.revision || '—')}</span></div><div class="managed-device-actions">${actions}</div></article>`;
    }).join('');
    list.querySelectorAll('[data-device-action]').forEach((button) => button.addEventListener('click', (event) => {
      event.stopPropagation();
      manageDevice(button.closest('.managed-device-row')?.dataset.deviceId || '', button.dataset.deviceAction);
    }));
    const selected = state.devices.find((device) => device.id === state.deviceManager.rotateDeviceID);
    if (!selected || selected.state === 'revoked') {
      state.deviceManager.rotateDeviceID = '';
      $('#device-rotate-panel').hidden = true;
    }
  }
  function taskCard(task) {
    const stateClass = ['queued', 'failed', 'canceled'].includes(task.state) ? task.state : '';
    const approvalPending = state.approvals.some((approval) => approval.task_id === task.id);
    const targetDevice = state.devices.find((device) => device.id === task.target_device_id);
    const needsRebind = targetDevice && targetDevice.state === 'paired' && task.target_device_id !== 'local-mac' && Number(targetDevice.connection_epoch) > Number(task.target_epoch) && !['completed', 'pushing'].includes(task.state);
    const action = needsRebind ? '<button class="task-action primary" data-task-action="rebind">reconnect target</button>' : approvalPending || task.state === 'awaiting_approval' ? '<button class="task-action primary" data-task-action="approve">approve</button><button class="task-action danger" data-task-action="cancel">cancel</button>' : task.state === 'queued' ? '<button class="task-action primary" data-task-action="start">start</button>' : task.state === 'running' ? '<button class="task-action" data-task-action="verify">verify</button><button class="task-action danger" data-task-action="cancel">cancel</button>' : task.state === 'verifying' ? '<button class="task-action primary" data-task-action="commit">commit</button>' : ['paused', 'failed'].includes(task.state) ? '<button class="task-action primary" data-task-action="resume">resume</button>' : '';
    return `<article class="task-card selected" data-task-id="${escapeHTML(task.id)}"><div class="task-top"><div class="task-badges"><span class="provider-badge">${escapeHTML(task.provider || 'runtime')}</span><span class="state-badge ${stateClass}">${escapeHTML(task.state || 'unknown')}</span></div><span class="task-age">rev ${task.revision || '—'}</span></div><h3>${escapeHTML(task.title || 'Untitled task')}</h3><p class="task-prompt">${escapeHTML(task.prompt || 'No prompt recorded.')}</p><div class="task-foot"><span>↗ <strong>${escapeHTML(shortID(task.execution_id))}</strong></span><span>◷ ${relativeTime(task.updated_at)}</span><div class="task-actions">${action}</div></div></article>`;
  }
  function renderInspector(task) {
    $('#inspector-title').textContent = task ? task.title || 'Untitled task' : 'No task selected';
    $('#inspector-state').innerHTML = task ? `<span class="state-dot"></span><span>${escapeHTML(task.state)}</span>` : '<span class="state-dot"></span><span>Waiting for a task</span>';
    $('#lineage-task').textContent = task ? shortID(task.id) : '—'; $('#lineage-revision').textContent = task ? String(task.revision) : '—'; $('#lineage-execution').textContent = task ? shortID(task.execution_id) : '—'; $('#lineage-repository').textContent = task ? shortID(task.repository_id) : '—';
    $('#cursor-badge').textContent = `cursor ${state.events.length ? state.events[state.events.length - 1].cursor : '—'}`;
    $('#event-stream').innerHTML = state.events.length ? state.events.slice().reverse().map(eventRow).join('') : '<div class="stream-empty">Events will appear here<br />after the first command.</div>';
  }
  function eventRow(event) { return `<div class="event-row"><span class="event-marker"></span><div class="event-copy"><span class="event-type">${escapeHTML(event.type || 'event')} · rev ${event.revision || '—'}</span><span class="event-time">${relativeTime(event.created_at)} · cursor ${event.cursor || '—'}</span></div></div>`; }
  function shortID(value) { if (!value) return '—'; return value.length > 18 ? `${value.slice(0, 8)}…${value.slice(-6)}` : value; }
  function relativeTime(value) { if (!value) return 'just now'; const age = Math.max(0, Date.now() - new Date(value).getTime()); if (age < 60000) return 'just now'; const minutes = Math.round(age / 60000); return minutes < 60 ? `${minutes}m ago` : `${Math.round(minutes / 60)}h ago`; }
  function escapeHTML(value) { return String(value ?? '').replace(/[&<>'"]/g, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[character])); }

  async function setupWorkspace(form) {
    const data = new FormData(form); const projectName = String(data.get('project')).trim(); const remote = String(data.get('remote')).trim(); const boardName = String(data.get('board')).trim();
    try {
      const project = await bridgeRequest('/v1/projects', { method: 'POST', body: { name: projectName, idempotency_key: idempotency('project') } });
      const repository = await bridgeRequest('/v1/repositories', { method: 'POST', body: { remote, idempotency_key: idempotency('repository') } });
      const board = await bridgeRequest('/v1/boards', { method: 'POST', body: { project_id: project.project.id, name: boardName, idempotency_key: idempotency('board') } });
      state.setup = { projectID: project.project.id, repositoryID: repository.repository.id, boardID: board.board.id, projectName, boardName }; saveSetup(); state.task = null; state.events = []; state.approvals = []; $('#setup-modal').close(); showToast('Workspace is ready.'); render();
    } catch (error) { showToast(error.message, true); }
  }
  async function createTask(form) {
    if (!state.setup.projectID) { showToast('Set up a workspace first.', true); $('#task-modal').close(); $('#setup-modal').showModal(); return; }
    const data = new FormData(form); const prompt = String(data.get('prompt')).trim();
    try {
      const response = await bridgeRequest('/v1/tasks', { method: 'POST', body: { project_id: state.setup.projectID, board_id: state.setup.boardID, repository_id: state.setup.repositoryID, target_device_id: data.get('target_device') || 'local-mac', provider: data.get('provider'), title: String(data.get('title')).trim(), prompt, idempotency_key: idempotency('task') } });
      state.task = response.task; state.setup.lastTask = response.task; saveSetup(); state.events = []; state.approvals = []; $('#task-modal').close(); showToast('Task queued.'); render();
    } catch (error) { showToast(error.message, true); }
  }
  async function refreshDevices() {
    try {
      const response = await bridgeRequest('/v1/devices');
      state.devices = response.devices || [];
      const select = $('#target-device');
      const devices = [{ id: 'local-mac', name: 'Mac · local', state: 'paired' }, ...state.devices.filter((device) => device.id !== 'local-mac')];
      select.innerHTML = devices.map((device) => {
        const unavailable = device.state !== 'paired';
        return `<option value="${escapeHTML(device.id)}"${unavailable ? ' disabled' : ''}>${escapeHTML(device.name)} · ${escapeHTML(device.state)}${unavailable ? ' · unavailable' : ''}</option>`;
      }).join('');
      render();
    } catch (_) { /* The native host may not expose device discovery until it is connected. */ }
  }
  function openDeviceManager() {
    state.deviceManager.rotateDeviceID = '';
    $('#device-rotate-panel').hidden = true;
    $('#device-modal').showModal();
    refreshDevices();
  }
  function manageDevice(deviceID, actionName) {
    const device = state.devices.find((candidate) => candidate.id === deviceID);
    if (!device) return;
    if (actionName === 'rotate') {
      state.deviceManager.rotateDeviceID = deviceID;
      $('#device-rotate-key').value = '';
      $('#device-rotate-fingerprint').hidden = true;
      $('#device-rotate-fingerprint-value').textContent = '—';
      $('#device-rotate-confirm').checked = false;
      $('#rotate-device').disabled = true;
      $('#device-rotate-status').textContent = 'waiting';
      $('#device-rotate-panel').hidden = false;
      return;
    }
    if (actionName === 'revoke' && !globalThis.confirm(`Revoke ${device.name || device.id}? Existing assignments will be fenced.`)) return;
    setDeviceState(device, actionName);
  }
  async function setDeviceState(device, stateName) {
    const route = stateName === 'paired' ? 'reachable' : stateName;
    try {
      const response = await bridgeRequest(`/v1/devices/${encodeURIComponent(device.id)}/${route}`, {
        method: 'POST', body: { revision: device.revision, idempotency_key: idempotency(`device-${stateName}`) },
      });
      await refreshDevices();
      showToast(`${response.device.name} marked ${response.device.state}.`);
    } catch (error) { showToast(error.message, true); }
  }
  async function inspectRotateKey() {
    const encoded = String($('#device-rotate-key').value || '').trim();
    const fingerprint = await publicKeyFingerprint(encoded);
    $('#device-rotate-fingerprint').hidden = !fingerprint;
    $('#device-rotate-fingerprint-value').textContent = fingerprint || '—';
    $('#device-rotate-status').textContent = fingerprint ? 'fingerprint ready' : 'invalid key';
    $('#rotate-device').disabled = !fingerprint || !$('#device-rotate-confirm').checked;
  }
  async function rotateDevice() {
    const device = state.devices.find((candidate) => candidate.id === state.deviceManager.rotateDeviceID);
    const publicKey = String($('#device-rotate-key').value || '').trim();
    if (!device || !publicKey || !$('#device-rotate-confirm').checked) {
      showToast('Verify the new device fingerprint before rotating.', true);
      return;
    }
    try {
      const response = await bridgeRequest(`/v1/devices/${encodeURIComponent(device.id)}/rotate`, {
        method: 'POST', body: { revision: device.revision, public_key: publicKey, idempotency_key: idempotency('device-rotate') },
      });
      state.deviceManager.rotateDeviceID = '';
      $('#device-rotate-panel').hidden = true;
      await refreshDevices();
      showToast(`${response.device.name} key rotated; epoch ${response.device.connection_epoch}.`);
    } catch (error) { showToast(error.message, true); }
  }
  function browserFingerprint() {
    if (globalThis.crypto?.randomUUID) return `desktop-${globalThis.crypto.randomUUID()}`;
    const bytes = new Uint8Array(16); globalThis.crypto?.getRandomValues?.(bytes);
    return `desktop-${Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('') || idempotency('browser')}`;
  }
  function openPairing() {
    const form = $('#pair-form'); form.reset(); state.pairing = { challenge: null };
    $('#pair-challenge-json').value = ''; $('#pair-proof-json').value = ''; $('#pair-proof-step').hidden = true;
    $('#pair-challenge-status').textContent = 'waiting'; $('#pair-proof-status').textContent = 'waiting'; $('#pair-challenge-expiry').textContent = 'No challenge yet.';
    $('#pair-fingerprint').hidden = true; $('#pair-fingerprint-value').textContent = '—'; $('#pair-confirm-fingerprint').checked = false; $('#pair-confirm-fingerprint').disabled = true; $('#complete-pairing').disabled = true;
    $('#pair-modal').showModal();
  }
  async function startPairing(form) {
    const data = new FormData(form); const deviceID = String(data.get('device_id') || '').trim();
    if (!deviceID || !String(data.get('name') || '').trim() || !String(data.get('endpoint') || '').trim()) { showToast('Device ID, name, and WSS endpoint are required.', true); return; }
    try {
      const response = await bridgeRequest('/v1/devices/challenges', { method: 'POST', body: { device_id: deviceID, browser_fingerprint: browserFingerprint(), idempotency_key: idempotency('device-challenge') } });
      state.pairing = { challenge: response.challenge, deviceID, name: String(data.get('name')).trim(), endpoint: String(data.get('endpoint')).trim() };
      $('#pair-challenge-json').value = JSON.stringify(response.challenge, null, 2); $('#pair-challenge-status').textContent = 'created'; $('#pair-proof-status').textContent = 'paste proof'; $('#pair-challenge-expiry').textContent = `expires ${expiryTime(response.challenge.expires_at)}`; $('#pair-proof-step').hidden = false;
      showToast('Challenge created. Move its JSON to the Pi.');
    } catch (error) { showToast(error.message, true); }
  }
  function proofRequest(value) { return value && value.request && typeof value.request === 'object' ? value.request : value; }
  async function inspectPairProof() {
    const raw = String($('#pair-proof-json').value || '').trim(); $('#pair-fingerprint').hidden = true; $('#pair-confirm-fingerprint').disabled = true; $('#pair-confirm-fingerprint').checked = false; $('#complete-pairing').disabled = true;
    if (!raw) return;
    try {
      const parsed = proofRequest(JSON.parse(raw)); const fingerprint = await publicKeyFingerprint(parsed.public_key);
      if (!fingerprint || (parsed.fingerprint && String(parsed.fingerprint).toLowerCase() !== fingerprint)) throw new Error('device fingerprint does not match the public key');
      $('#pair-fingerprint-value').textContent = fingerprint; $('#pair-fingerprint').hidden = false; $('#pair-confirm-fingerprint').disabled = false; $('#pair-proof-status').textContent = 'fingerprint ready';
      $('#complete-pairing').disabled = false;
    } catch (_) { $('#pair-proof-status').textContent = 'invalid proof JSON'; }
  }
  async function publicKeyFingerprint(encoded) {
    if (typeof encoded !== 'string' || !globalThis.crypto?.subtle) return '';
    const binary = atob(encoded); const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
    if (bytes.length !== 32) return '';
    const digest = new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', bytes)); return Array.from(digest, (value) => value.toString(16).padStart(2, '0')).join('');
  }
  function expiryTime(value) { const date = new Date(value); return Number.isNaN(date.getTime()) ? 'soon' : date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); }
  async function completePairing(form) {
    if (!state.pairing.challenge) { showToast('Create a challenge first.', true); return; }
    if (!$('#pair-confirm-fingerprint').checked) { showToast('Verify the device fingerprint before pairing.', true); return; }
    let parsed;
    try { parsed = proofRequest(JSON.parse(String($('#pair-proof-json').value || ''))); } catch (_) { showToast('Paste a valid device proof JSON.', true); return; }
    const data = new FormData(form); const body = { challenge_id: state.pairing.challenge.id, name: String(data.get('name') || '').trim(), endpoint: String(data.get('endpoint') || '').trim(), kind: parsed.kind || 'raspberry_pi', public_key: parsed.public_key, signature: parsed.signature, idempotency_key: parsed.idempotency_key || idempotency('pair-device') };
    try {
      const response = await bridgeRequest('/v1/devices/pair', { method: 'POST', body });
      $('#pair-modal').close(); state.pairing = { challenge: null }; await refreshDevices(); showToast(`Paired ${response.device.name}. Fingerprint ${shortID(response.device.fingerprint)}.`);
    } catch (error) { showToast(error.message, true); }
  }
  async function recoverDevice(deviceID) {
    if (!deviceID || deviceID === 'local-mac') return;
    try {
      // Replay is durable and idempotent at the local authority. Polling this
      // boundary lets a paired Pi consume accepted work after reconnect without
      // moving provider or repository access into the Desktop process.
      await bridgeRequest(`/v1/devices/${encodeURIComponent(deviceID)}/replay`, {
        method: 'POST', body: { limit: 100 },
      });
    } catch (_) { /* An unreachable device remains queued until the next poll. */ }
  }
  async function action(name) {
    if (!state.task) return;
    const task = state.task; const request = { task_id: task.id, revision: task.revision, idempotency_key: idempotency(name) };
    if (name === 'rebind') {
      const device = state.devices.find((candidate) => candidate.id === task.target_device_id && candidate.state === 'paired');
      if (!device) { showToast('The execution device is not reachable yet.', true); return; }
      try {
        const response = await bridgeRequest(`/v1/tasks/${encodeURIComponent(task.id)}/device`, {
          method: 'POST',
          body: { task_id: task.id, revision: task.revision, target_device_id: device.id, idempotency_key: idempotency('task-device') },
        });
        state.task = response.task; state.setup.lastTask = response.task; saveSetup();
        if (response.event?.cursor) state.events.push(response.event);
        showToast(`Target reconnected on epoch ${device.connection_epoch}.`); render();
        await recoverDevice(device.id);
        await observe();
      } catch (error) { showToast(error.message, true); }
      return;
    }
    if (name === 'approve') {
      const approval = state.approvals.find((candidate) => candidate.task_id === task.id);
      if (!approval) { showToast('No pending approval is available.', true); return; }
      Object.assign(request, { approval_id: approval.id, user_id: 'local-authority', allow: true });
    }
    try {
      const response = await bridgeRequest(`/v1/tasks/${encodeURIComponent(task.id)}/${name}`, { method: 'POST', body: request });
      if (response.task) state.task = response.task; if (response.task) { state.setup.lastTask = response.task; saveSetup(); } if (response.event && response.event.cursor) state.events.push(response.event); showToast(`${name} accepted.`); render();
      await refreshApprovals(); render();
    } catch (error) { showToast(error.message, true); }
  }
  async function refreshApprovals() {
    if (!state.task) { state.approvals = []; return; }
    try {
      const response = await bridgeRequest(`/v1/tasks/${encodeURIComponent(state.task.id)}/approvals`);
      state.approvals = response.approvals || [];
    } catch (_) { state.approvals = []; }
  }
  async function observe() {
    if (!state.task || state.observeInFlight) return;
    state.observeInFlight = true;
    try {
      await recoverDevice(state.task.target_device_id);
      const afterCursor = state.events.length ? state.events[state.events.length - 1].cursor : 0;
      const response = await bridgeRequest(`/v1/tasks/${encodeURIComponent(state.task.id)}/events?after_cursor=${afterCursor}&limit=100`);
      const approvals = await bridgeRequest(`/v1/tasks/${encodeURIComponent(state.task.id)}/approvals`);
      state.task = response.task; state.setup.lastTask = response.task; saveSetup(); if (response.events?.length) state.events = state.events.concat(response.events); state.approvals = approvals.approvals || [];
      $('#sync-status').innerHTML = '<span class="sync-dot"></span> synced just now'; render();
    } catch (error) { $('#sync-status').innerHTML = '<span class="sync-dot" style="background:var(--danger)"></span> transport unavailable'; }
    finally { state.observeInFlight = false; }
  }
  function bindTaskActions() { document.querySelectorAll('[data-task-action]').forEach((button) => button.addEventListener('click', (event) => { event.stopPropagation(); action(button.dataset.taskAction); })); document.querySelectorAll('.task-card').forEach((card) => card.addEventListener('click', () => { document.querySelectorAll('.task-card').forEach((item) => item.classList.remove('selected')); card.classList.add('selected'); })); }

  $('#workspace-switcher').addEventListener('click', () => $('#setup-modal').showModal()); $('#new-board').addEventListener('click', () => $('#setup-modal').showModal()); $('#device-card').addEventListener('click', openDeviceManager); $('#open-pairing').addEventListener('click', () => { $('#device-modal').close(); openPairing(); }); $('#rotate-device').addEventListener('click', rotateDevice); $('#device-rotate-key').addEventListener('input', inspectRotateKey); $('#device-rotate-confirm').addEventListener('change', inspectRotateKey); $('#open-composer').addEventListener('click', () => { refreshDevices(); $('#task-modal').showModal(); }); document.querySelectorAll('[data-open-composer]').forEach((button) => button.addEventListener('click', () => { refreshDevices(); $('#task-modal').showModal(); }));
  $('#setup-form').addEventListener('submit', (event) => { if (event.submitter?.value === 'default') { event.preventDefault(); setupWorkspace(event.currentTarget); } }); $('#task-form').addEventListener('submit', (event) => { if (event.submitter?.value === 'default') { event.preventDefault(); createTask(event.currentTarget); } });
  $('#pair-form').addEventListener('submit', (event) => { if (event.submitter?.value === 'challenge') { event.preventDefault(); startPairing(event.currentTarget); } else if (event.submitter?.value === 'complete') { event.preventDefault(); completePairing(event.currentTarget); } }); $('#pair-proof-json').addEventListener('input', inspectPairProof); $('#copy-pair-challenge').addEventListener('click', async () => { const value = $('#pair-challenge-json').value; if (!value) { showToast('Create a challenge first.', true); return; } try { await navigator.clipboard.writeText(value); showToast('Challenge copied.'); } catch (_) { $('#pair-challenge-json').select(); showToast('Select the challenge JSON and copy it.'); } });
  document.addEventListener('keydown', (event) => { if (event.key.toLowerCase() === 'n' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) { event.preventDefault(); refreshDevices(); $('#task-modal').showModal(); } });
  state.task = state.setup.lastTask || null; state.events = []; state.approvals = []; render(); refreshDevices(); state.poll = setInterval(observe, 2800);
})();
