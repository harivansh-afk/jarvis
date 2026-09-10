const $ = (id) => document.getElementById(id);
let state, selected, inspection, events, peer, media, dataChannel, heartbeat, polling, starting = false;
let transcriptLine, transcriptKind;
function error(message) { $('error').textContent = message; $('error').hidden = !message; }
async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { 'Content-Type': 'application/json', ...options.headers } });
  if (response.status === 401) { $('login').hidden = false; $('workspace').hidden = true; throw new Error('Connect with your workspace access token.'); }
  const text = await response.text(); let result; try { result = JSON.parse(text); } catch { result = { error: text }; }
  if (!response.ok) throw new Error(result.error || `Request failed (${response.status})`);
  return result;
}
function notice(text, speaker = 'Update') {
  const p = document.createElement('p'), label = document.createElement('span'); label.className = 'speaker'; label.textContent = speaker;
  p.append(label, document.createTextNode(text)); $('feed').append(p);
  while ($('feed').children.length > 100) $('feed').firstElementChild.remove();
  $('feed').scrollTop = $('feed').scrollHeight;
  return p;
}
async function refresh() {
  state = await api('/api/state'); $('login').hidden = true; $('workspace').hidden = false;
  $('connection').textContent = state.mux_connected ? 'Spark connected' : 'Waiting for Mux'; $('connection').classList.toggle('online', state.mux_connected);
  $('count').textContent = state.terminals.length; $('empty').hidden = state.terminals.length > 0;
  $('voice').disabled = starting || (!peer && !state.voice_configured);
  if (!peer) $('voice-status').textContent = state.voice_configured ? `${state.backend_model} · ready to talk` : 'OpenAI project key needed for voice';
  $('terminals').replaceChildren();
  for (const t of state.terminals) {
    const button = document.createElement('button'); button.className = 'pane' + (selected === t.name ? ' active' : '');
    const name = document.createElement('strong'), cwd = document.createElement('small'), status = document.createElement('small');
    name.textContent = t.agent?.topic || t.name; name.title = t.name; cwd.textContent = t.cwd || t.name;
    status.className = 'state'; status.textContent = `${t.agent?.agent || 'terminal'} · ${t.agent?.state || 'running'}${state.watches.some(w => w.name === t.name) ? ' · following' : ''}`;
    button.append(name, cwd, status); button.addEventListener('click', () => select(t.name).catch(e => error(e.message))); $('terminals').append(button);
  }
  $('watch').textContent = state.watches.some(w => w.name === selected) ? 'Following ✓' : 'Follow';
  $('watch').disabled = !selected || !state.mux_connected;
  if (selected && !state.terminals.some(t => t.name === selected)) { selected = null; inspection = null; $('screen').textContent = 'This terminal is no longer available.'; $('message').disabled = true; $('send').disabled = true; }
}
async function inspect() {
  if (!selected) return;
  const name = selected, result = await api('/api/inspect?name=' + encodeURIComponent(name)); if (selected !== name) return;
  inspection = result; $('screen').textContent = result.snapshot.text.join('\n'); $('terminal-name').textContent = name; $('terminal-path').textContent = result.snapshot.cwd || '';
  $('message').disabled = false; $('send').disabled = false;
}
async function select(name) { selected = name; inspection = null; error(''); await refresh(); await inspect(); }
function connectEvents() {
  events?.close(); events = new EventSource('/api/events');
  events.onmessage = (message) => {
    const event = JSON.parse(message.data);
    if (['refresh', 'terminal', 'receipt', 'voice'].includes(event.type)) refresh().catch(e => error(e.message));
    if (event.type === 'notice') notice(event.data);
    if (event.type === 'receipt') notice(`Input to ${event.data.name}: ${event.data.status}.`);
    if (event.type === 'voice' && !event.data.connected && peer) cleanupVoice();
    if (event.type === 'transcript') {
      const kind = event.data.type.includes('input_') ? 'You' : 'Jarvis';
      if (kind !== transcriptKind || !transcriptLine?.isConnected) { transcriptLine = notice('', kind); transcriptKind = kind; }
      transcriptLine.append(document.createTextNode(event.data.delta)); $('feed').scrollTop = $('feed').scrollHeight;
    }
  };
}
async function boot() { await refresh(); connectEvents(); clearInterval(polling); polling = setInterval(() => inspect().catch(e => error(e.message)), 3000); }
$('login-form').addEventListener('submit', async event => { event.preventDefault(); try { await api('/api/login', { method: 'POST', body: JSON.stringify({ token: $('token').value }) }); $('token').value = ''; error(''); await boot(); } catch(e) { error(e.message); } });
$('watch').addEventListener('click', async () => { try { await api('/api/watch', { method: 'POST', body: JSON.stringify({ name: selected, enabled: !state.watches.some(w => w.name === selected) }) }); await refresh(); } catch(e) { error(e.message); } });
$('input-form').addEventListener('submit', async event => {
  event.preventDefault(); if (!selected || !inspection || !$('message').value) return;
  $('send').disabled = true;
  try {
    // Use the screen the user actually saw. Do not refresh silently and send into a changed application.
    const result = await api('/api/input', { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, body: JSON.stringify({ inspection_id: inspection.inspection_id, text: $('message').value, submit: true }) });
    $('input-status').textContent = result.status === 'written' ? 'Written to the terminal. Application acceptance is not yet confirmed.' : 'Delivery is uncertain. Inspect the terminal before any new action.';
    if (result.status === 'written') $('message').value = '';
    inspection = null; await inspect();
  } catch(e) { error(e.message); } finally { $('send').disabled = !selected; }
});
function cleanupVoice() {
  clearInterval(heartbeat); heartbeat = null; media?.getTracks().forEach(t => t.stop()); media = null;
  peer?.close(); peer = null; dataChannel = null; $('audio').srcObject = null; $('audio').hidden = true;
  $('voice').textContent = 'Start conversation ↗'; $('voice').disabled = !state?.voice_configured; $('voice-status').textContent = 'Voice is disconnected'; $('mute').hidden = true; starting = false;
}
async function stopVoice() { try { await api('/api/voice', { method: 'DELETE' }); } finally { cleanupVoice(); } }
async function startVoice() {
  starting = true; $('voice').disabled = true; $('voice-status').textContent = 'Connecting…'; error('');
  try {
    if (!window.isSecureContext) throw new Error('Microphone access needs HTTPS or localhost.');
    media = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
    peer = new RTCPeerConnection(); const current = peer;
    current.ontrack = ({ streams }) => { $('audio').srcObject = streams[0]; $('audio').play().catch(() => { $('audio').hidden = false; }); };
    for (const track of media.getTracks()) current.addTrack(track, media);
    dataChannel = current.createDataChannel('oai-events');
    dataChannel.onmessage = ({ data }) => { const event = JSON.parse(data); if (event.type === 'session.started') { $('voice-status').textContent = 'Connected · listening'; $('voice').textContent = 'End conversation'; $('voice').disabled = false; $('mute').hidden = false; starting = false; } if (event.type === 'session.closed') cleanupVoice(); };
    current.onconnectionstatechange = () => { if (current.connectionState === 'failed') { error('Voice connection failed. Reconnect to continue.'); stopVoice().catch(() => {}); } };
    await current.setLocalDescription(await current.createOffer());
    await new Promise((resolve, reject) => { if (current.iceGatheringState === 'complete') return resolve(); const timeout = setTimeout(() => reject(new Error('Network setup timed out.')), 12000); current.addEventListener('icegatheringstatechange', () => { if (current.iceGatheringState === 'complete') { clearTimeout(timeout); resolve(); } }); });
    const result = await api('/api/voice', { method: 'POST', body: JSON.stringify({ sdp: current.localDescription.sdp }) });
    await current.setRemoteDescription({ type: 'answer', sdp: result.transport.sdp });
    heartbeat = setInterval(() => api('/api/voice/heartbeat', { method: 'POST', body: '{}' }).catch(() => stopVoice().catch(() => {})), 10000);
  } catch(e) { error(e.message); await stopVoice().catch(() => {}); }
}
$('voice').addEventListener('click', () => (peer ? stopVoice() : startVoice()).catch(e => error(e.message)));
$('mute').addEventListener('click', () => { const track = media?.getAudioTracks()[0]; if (track) { track.enabled = !track.enabled; $('mute').textContent = track.enabled ? 'Mute microphone' : 'Unmute microphone'; } });
window.addEventListener('pagehide', () => { if (peer) fetch('/api/voice', { method: 'DELETE', keepalive: true }); events?.close(); });
boot().catch(e => error(e.message));
