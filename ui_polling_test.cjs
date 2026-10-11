// Run with: node --test ui_polling_test.cjs
// Exercise the actual embedded UI script with a simulated clock and network.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync('ui.html', 'utf8').match(/<script>([\s\S]*?)<\/script>/)[1];
const sample = (name, inactive = false) => ({ id: name, name, address: `http://${name}:18081`, inactive });
const agent = (host, id, inactive = false) => ({ id: `${host}-${id}`, name: `Agent ${id}`, address: `http://${host}:18081/${id}`, inactive });
const status = { remaining_seconds: 300, is_running: true, time_up_visible: false, on_behalf_of: [] };
const flush = async () => { for (let i = 0; i < 25; i++) await Promise.resolve(); };

function harness(initial, settings = { pollIntervalSeconds: 1, falsePolling: false }) {
  let now = 0, serial = 0;
  const timers = new Map(), nodes = new Map(), calls = [];
  const node = id => {
    if (!nodes.has(id)) nodes.set(id, {
      textContent: '', innerHTML: '', className: '', hidden: false, children: [], dataset: {},
      close() { this.open = false; }, showModal() { this.open = true; },
    });
    return nodes.get(id);
  };
  const storage = new Map();
  if (settings !== null) storage.set('overlay-timer-ui-settings-v1', JSON.stringify(settings));
  const state = { list: initial, stallList: false, slow: new Set(), responses: new Map(), calls, node, storage, toggles: [], extensions: [] };
  function schedule(fn, delay, interval = 0) {
    const id = ++serial;
    timers.set(id, { fn, at: now + delay, interval });
    return id;
  }
  const context = vm.createContext({
    console, AbortController,
    performance: { now: () => now },
    localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    document: {
      getElementById: node, querySelectorAll: selector => selector === '.mobile-toggle' ? state.toggles
        : selector === '.timer-extension' ? state.extensions : []
    },
    setTimeout: (fn, delay) => schedule(fn, delay), clearTimeout: id => timers.delete(id),
    setInterval: (fn, delay) => schedule(fn, delay, delay), clearInterval: id => timers.delete(id),
    fetch(url, options = {}) {
      const call = { url, options, at: now, pending: true, aborted: false };
      calls.push(call);
      return new Promise((resolve, reject) => {
        call.resolve = data => {
          call.pending = false;
          resolve({ ok: true, json: async () => data });
        };
        options.signal?.addEventListener('abort', () => {
          call.aborted = true;
          call.pending = false;
          reject(new Error('aborted'));
        }, { once: true });
        if (url === '/api/pcs') {
          if (!state.stallList) call.resolve(structuredClone(state.list));
        } else if (!state.slow.has(url)) call.resolve(structuredClone(state.responses.get(url) || status));
      });
    },
  });
  state.run = code => vm.runInContext(code, context);
  state.start = () => vm.runInContext(source, context);
  state.advance = async milliseconds => {
    const end = now + milliseconds;
    await flush();
    while (true) {
      const next = [...timers].filter(([, t]) => t.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
      if (!next) break;
      const [id, timer] = next;
      now = timer.at;
      if (timer.interval) timer.at += timer.interval;
      else timers.delete(id);
      timer.fn();
      await flush();
    }
    now = end;
    await flush();
  };
  return state;
}

test('one online PC polls each second while nine offline PCs time out independently', async () => {
  const h = harness(Array.from({ length: 10 }, (_, i) => sample(`pc${i}`)));
  for (let i = 1; i < 10; i++) h.slow.add(`http://pc${i}:18081/status`);
  h.start();
  await h.advance(3500);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://pc0:18081/status').map(c => c.at), [0, 1000, 2000, 3000]);
  for (let i = 1; i < 10; i++) assert.equal(h.calls.filter(c => c.url === `http://pc${i}:18081/status`).length, 1);
  await h.advance(2500);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://pc0:18081/status').map(c => c.at), [0, 1000, 2000, 3000, 4000, 5000, 6000]);
  assert.ok(h.calls.some(c => c.url === 'http://pc1:18081/status' && c.aborted));
});

test('a stalled saved-list refresh does not delay status polling or duplicate list requests', async () => {
  const h = harness([sample('online')]);
  h.start();
  await h.advance(14000);
  h.stallList = true;
  await h.advance(1000);
  h.run('refreshSavedList(); refreshSavedList();');
  await h.advance(5000);
  assert.deepEqual(h.calls.filter(c => c.url === '/api/pcs').map(c => c.at), [0, 15000]);
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 21);
  assert.match(h.node('discoveryStatus').textContent, /Could not refresh saved PCs/);
  h.stallList = false;
  h.list.push(sample('new'));
  await h.advance(10000);
  assert.ok(h.calls.some(c => c.url === 'http://new:18081/status' && c.at === 30000));
});

test('ticks, manual refresh and detail navigation share pending requests', async () => {
  const h = harness([sample('slow'), sample('online')]);
  h.slow.add('http://slow:18081/status');
  h.start();
  await h.advance(0);
  h.run('showDetail("slow"); pollAll(); refreshNow();');
  await h.advance(3000);
  assert.equal(h.calls.filter(c => c.url === 'http://slow:18081/status').length, 1);
  assert.ok(h.calls.some(c => c.url === 'http://online:18081/status' && c.at === 3000));
});

test('list changes stop removed/inactive PCs and replace changed addresses', async () => {
  const h = harness([sample('old'), sample('removed'), sample('disabled')]);
  h.slow.add('http://old:18081/status');
  h.start();
  await h.advance(0);
  const old = h.calls.find(c => c.url === 'http://old:18081/status');
  h.list = [{ ...sample('old'), address: 'http://new-address:18081' }, sample('disabled', true)];
  await h.run('load()');
  await h.advance(2000);
  assert.equal(old.aborted, true);
  assert.equal(h.calls.filter(c => c.url === 'http://removed:18081/status').length, 1);
  assert.equal(h.calls.filter(c => c.url === 'http://disabled:18081/status').length, 1);
  assert.equal(h.calls.filter(c => c.url === 'http://new-address:18081/status').length, 3);
  assert.equal(h.node('ds-old').textContent, 'Running');
});

test('default settings poll every five seconds and leave false polling disabled', async () => {
  const h = harness([sample('pc')], null);
  h.start();
  await h.advance(4000);
  assert.equal(h.node('dt-pc').textContent, '00:05:00');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
  await h.advance(6000);
  assert.deepEqual(h.calls.filter(c => c.url.endsWith('/status')).map(c => c.at), [0, 5000, 10000]);
});

test('settings apply to all schedules, preserve pending requests, and survive reload', async () => {
  const h = harness([sample('online'), sample('slow')], null);
  h.slow.add('http://slow:18081/status');
  h.start();
  await h.advance(1000);
  h.run('openSettings()');
  assert.equal(h.node('pollInterval').value, 5);
  assert.equal(h.node('falsePolling').checked, false);
  h.node('pollInterval').value = '2';
  h.node('falsePolling').checked = true;
  h.run('saveSettings({preventDefault() {}})');
  await h.advance(4000);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://online:18081/status').map(c => c.at), [0, 3000, 5000]);
  assert.deepEqual(h.calls.filter(c => c.url === 'http://slow:18081/status').map(c => c.at), [0, 5000]);
  const saved = JSON.parse(h.storage.get('overlay-timer-ui-settings-v1'));
  assert.deepEqual(saved, { pollIntervalSeconds: 2, falsePolling: true });
  const reloaded = harness([sample('pc')], saved);
  reloaded.start();
  await reloaded.advance(3000);
  assert.deepEqual(reloaded.calls.filter(c => c.url.endsWith('/status')).map(c => c.at), [0, 2000]);
  assert.equal(reloaded.node('dt-pc').textContent, '00:04:59');
});

test('false polling interpolates running timers and agents, preserves paused timers, and resyncs', async () => {
  const h = harness([sample('pc'), agent('pc', '1'), agent('pc', '2')], { pollIntervalSeconds: 5, falsePolling: true });
  h.responses.set('http://pc:18081/status', {
    ...status, remaining_seconds: 2135, on_behalf_of: [
      { ...status, id: '1', name: 'Running agent', remaining_seconds: 20 },
      { ...status, id: '2', name: 'Paused agent', remaining_seconds: 40, is_running: false },
    ]
  });
  h.start();
  await h.advance(0);
  h.run('showDetail("pc")');
  await h.advance(2000);
  assert.equal(h.node('dt-pc').textContent, '00:35:33');
  assert.equal(h.node('time').textContent, '00:35:33');
  assert.equal(h.node('dt-pc-1').textContent, '00:00:18');
  assert.equal(h.node('ds-pc-1').textContent, 'Running');
  assert.equal(h.node('dt-pc-2').textContent, '00:00:40');
  assert.equal(h.node('ds-pc-2').textContent, 'Paused');
  const calls = h.calls.length;
  h.run('falsePollingTick(); falsePollingTick();');
  assert.equal(h.calls.length, calls, 'local display ticks must not send requests');
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 90, is_running: false });
  await h.advance(4000);
  assert.equal(h.node('dt-pc').textContent, '00:01:30');
  assert.equal(h.node('ds-pc').textContent, 'Paused');
  assert.equal(h.node('ds-pc-1').textContent, 'Unavailable');
  assert.equal(h.node('dt-pc-1').textContent, '--:--:--');
});

test('discovered systems have separate cards and controls but one status request per host', async () => {
  const h = harness([sample('pc4'), agent('pc4', '1'), agent('pc4', '20'), sample('pc5'), agent('pc5', '1')]);
  h.responses.set('http://pc4:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '20', remaining_seconds: 20 },
      { ...status, id: '1', remaining_seconds: 91, is_running: false },
    ]
  });
  h.responses.set('http://pc5:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '1', remaining_seconds: 45 },
    ]
  });
  h.start();
  await h.advance(0);
  assert.equal(h.node('activeCount').textContent, '(5/5)');
  assert.equal((h.node('dashboardGrid').innerHTML.match(/<article /g) || []).length, 5);
  assert.equal(h.node('dt-pc4-1').textContent, '00:01:31');
  assert.equal(h.node('dt-pc5-1').textContent, '00:00:45');
  assert.equal(h.node('dt-pc4-20').textContent, '00:00:20');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 2);
  h.run('showDetail("pc4-1")');
  await flush();
  assert.equal(h.node('time').textContent, '00:01:31');
  assert.equal(h.node('overlayControls').hidden, true);
  await h.run('quick("pc4-1", "play")');
  await h.run('callSelected("set", {seconds: 120})');
  const play = h.calls.find(c => c.url === 'http://pc4:18081/1/play');
  const set = h.calls.find(c => c.url === 'http://pc4:18081/1/set');
  assert.equal(play.options.method, 'POST');
  assert.deepEqual(JSON.parse(set.options.body), { seconds: 120 });
  assert.ok(!h.calls.some(c => /\/\d+\/status$/.test(c.url)));
  h.run('showDetail("pc4")');
  assert.equal(h.node('overlayControls').hidden, false);
});

test('shared status polling continues for an active agent with its own timer unchecked', async () => {
  const h = harness([sample('pc', true), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', remaining_seconds: 17 }] });
  h.start();
  await h.advance(2000);
  assert.equal(h.node('dt-pc-1').textContent, '00:00:17');
  assert.deepEqual(h.calls.filter(c => c.url.endsWith('/status')).map(c => c.url), Array(3).fill('http://pc:18081/status'));
  const before = h.calls.length;
  assert.equal(await h.run('command("pc", "play")'), false);
  assert.equal(h.calls.length, before);
});

test('mobile toggles use each system status and route actions to its timer URL', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.toggles.push({ dataset: { pc: 'pc', running: 'false' } }, { dataset: { pc: 'pc-1', running: 'false' } });
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', is_running: false }] });
  h.start();
  await h.advance(0);
  assert.equal(h.toggles[0].textContent, 'Pause');
  assert.equal(h.toggles[1].textContent, 'Play');
  await h.run('toggleTimer(document.querySelectorAll(".mobile-toggle")[1], "pc-1")');
  assert.ok(h.calls.some(c => c.url === 'http://pc:18081/1/play' && c.options.method === 'POST'));
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1' }] });
  await h.advance(1000);
  assert.equal(h.toggles[1].textContent, 'Pause');
  await h.run('toggleTimer(document.querySelectorAll(".mobile-toggle")[1], "pc-1")');
  assert.ok(h.calls.some(c => c.url === 'http://pc:18081/1/pause' && c.options.method === 'POST'));
  assert.ok(!h.calls.some(c => /\/\d+\/status$/.test(c.url)));
});

test('removing one sibling retains a pending shared poll; removing the last cancels it', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.slow.add('http://pc:18081/status');
  h.start();
  await h.advance(0);
  const pending = h.calls.find(c => c.url.endsWith('/status'));
  h.list = [agent('pc', '1')];
  await h.run('load()');
  assert.equal(pending.aborted, false);
  h.run('pollAll(); showDetail("pc-1"); refreshNow();');
  await h.advance(1000);
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
  h.list = [];
  await h.run('load()');
  assert.equal(pending.aborted, true);
  await h.advance(4000);
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
});

test('adding an agent to a known host reuses its sample and missing agents do not borrow the local countdown', async () => {
  const h = harness([sample('pc')]);
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', remaining_seconds: 70 }] });
  h.start();
  await h.advance(0);
  h.list.push(agent('pc', '1'), agent('pc', '2'));
  await h.run('load()');
  assert.equal(h.node('dt-pc-1').textContent, '00:01:10');
  assert.equal(h.node('ds-pc-2').textContent, 'Unavailable');
  assert.equal(h.node('dt-pc-2').textContent, '--:--:--');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, 1);
  h.responses.set('http://pc:18081/status', { ...status, on_behalf_of: [{ ...status, id: '1', remaining_seconds: -1 }, { ...status, id: '2' }] });
  await h.advance(1000);
  assert.equal(h.node('ds-pc').textContent, 'Running');
  assert.equal(h.node('ds-pc-1').textContent, 'Unavailable');
  assert.equal(h.node('ds-pc-2').textContent, 'Running');
});

test('false polling clamps at zero without inventing TIME UP and disabling restores the real sample', async () => {
  const h = harness([sample('pc')], { pollIntervalSeconds: 30, falsePolling: true });
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 2 });
  h.start();
  await h.advance(4000);
  assert.equal(h.node('dt-pc').textContent, '00:00:00');
  assert.equal(h.node('ds-pc').textContent, 'Running');
  h.run('applySettings({pollIntervalSeconds:30, falsePolling:false})');
  assert.equal(h.node('dt-pc').textContent, '00:00:02');
  await h.advance(2000);
  assert.equal(h.node('dt-pc').textContent, '00:00:02');
});

test('offline timers freeze until a fresh successful response arrives', async () => {
  const h = harness([sample('pc')], { pollIntervalSeconds: 5, falsePolling: true });
  h.start();
  await h.advance(0);
  h.slow.add('http://pc:18081/status');
  await h.advance(9500);
  assert.equal(h.node('ds-pc').textContent, 'Offline');
  const frozen = h.node('dt-pc').textContent;
  await h.advance(2500);
  assert.equal(h.node('dt-pc').textContent, frozen);
  h.slow.clear();
  h.responses.set('http://pc:18081/status', { ...status, remaining_seconds: 120 });
  await h.advance(3000);
  assert.equal(h.node('ds-pc').textContent, 'Running');
  assert.equal(h.node('dt-pc').textContent, '00:02:00');
});

test('invalid settings cannot overwrite the current interval', async () => {
  const h = harness([], null);
  h.start();
  await h.advance(0);
  for (const value of ['0', '-1', '1.5', '301', 'no']) {
    h.node('pollInterval').value = value;
    h.run('saveSettings({preventDefault() {}})');
    assert.match(h.node('settingsMessage').textContent, /whole number/);
    assert.equal(h.run('uiSettings.pollIntervalSeconds'), 5);
  }
});

test('special buttons use each timer labels and send only the name to the owning endpoint', async () => {
  const h = harness([sample('pc'), agent('pc', '1'), agent('pc', '20')]);
  const name = 'Switch "off" & <TV>';
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'Lock PC', short_name: 'LOCK' }],
    on_behalf_of: [
      { ...status, id: '1', special_commands: [{ name, short_name: 'OFF' }] },
      { ...status, id: '20', special_commands: [] },
    ],
  });
  h.start();
  await h.advance(0);
  assert.match(h.node('special-pc').innerHTML, /special-full-name">Lock PC</);
  assert.match(h.node('special-pc').innerHTML, /special-short-name" aria-hidden="true">LOCK</);
  assert.match(h.node('special-pc-1').innerHTML, /Switch &quot;off&quot; &amp; &lt;TV&gt;/);
  assert.match(h.node('special-pc-1').innerHTML, />OFF<\/span>/);
  assert.ok(!h.node('special-pc').innerHTML.includes('OFF'));
  assert.equal(h.node('special-pc-20').hidden, true);
  h.run('showDetail("pc-1")');
  await flush();
  assert.equal(h.node('detailSpecialCommands').innerHTML, h.node('special-pc-1').innerHTML);
  const polls = h.calls.filter(c => c.url.endsWith('/status')).length;
  await h.run(`specialCommand("pc-1", ${JSON.stringify(name)})`);
  await h.run('specialCommand("pc", "Lock PC")');
  const childCall = h.calls.find(c => c.url === 'http://pc:18081/1/special');
  const rootCall = h.calls.find(c => c.url === 'http://pc:18081/special');
  assert.equal(childCall.options.method, 'POST');
  assert.deepEqual(JSON.parse(childCall.options.body), { name });
  assert.deepEqual(JSON.parse(rootCall.options.body), { name: 'Lock PC' });
  assert.equal(h.node('msg').textContent, name + ': requested');
  assert.equal(h.calls.filter(c => c.url.endsWith('/status')).length, polls);
  assert.ok(!h.calls.some(c => c.url.endsWith('/identity') || /\/\d+\/status$/.test(c.url)));
});

test('pending special commands disable both views and coalesce repeated clicks without blocking polls', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '1', special_commands: [{ name: 'switch_off', short_name: 'OFF' }] },
    ]
  });
  h.start();
  await h.advance(0);
  h.run('showDetail("pc-1")');
  await flush();
  h.slow.add('http://pc:18081/1/special');
  h.run('specialCommand("pc-1", "switch_off"); specialCommand("pc-1", "switch_off");');
  await h.advance(2000);
  assert.equal(h.calls.filter(c => c.url.endsWith('/special')).length, 1);
  assert.match(h.node('special-pc-1').innerHTML, /disabled/);
  assert.match(h.node('detailSpecialCommands').innerHTML, /disabled/);
  assert.ok(h.calls.some(c => c.url === 'http://pc:18081/status' && c.at === 2000));
  h.calls.find(c => c.url.endsWith('/special')).resolve({ status: 'accepted' });
  await flush();
  assert.ok(!h.node('special-pc-1').innerHTML.includes('disabled'));
  assert.ok(!h.node('detailSpecialCommands').innerHTML.includes('disabled'));
});

test('timer extension buttons target the selected timer, add exact seconds, and preserve paused state', async () => {
  const h = harness([sample('pc'), agent('pc', '1'), agent('pc', '2')]);
  const agentStatus = (one, two) => ({
    ...status, remaining_seconds: 300, on_behalf_of: [
      { ...status, id: '1', remaining_seconds: one, is_running: false },
      { ...status, id: '2', remaining_seconds: two, is_running: true },
    ]
  });
  h.responses.set('http://pc:18081/status', agentStatus(60, 75));
  h.start();
  await h.advance(0);
  const markup = h.node('dashboardGrid').innerHTML;
  assert.equal((markup.match(/class="timer-extension"/g) || []).length, 9);
  for (const label of ['+15 min', '+30 min', '+1 hr']) assert.ok(markup.includes(`>${label}</button>`));

  let expected = 60;
  for (const amount of [900, 1800, 3600]) {
    expected += amount;
    h.responses.set('http://pc:18081/1/tweak', { remaining_seconds: expected });
    h.responses.set('http://pc:18081/status', agentStatus(expected, 75));
    await h.run(`extendTimer("pc-1", ${amount})`);
    const formatted = `${String(Math.floor(expected / 3600)).padStart(2, '0')}:${String(Math.floor((expected % 3600) / 60)).padStart(2, '0')}:${String(expected % 60).padStart(2, '0')}`;
    assert.equal(h.node('dt-pc-1').textContent, formatted);
    assert.equal(h.node('dt-pc-2').textContent, '00:01:15');
    assert.equal(h.node('dt-pc').textContent, '00:05:00');
    assert.equal(h.node('ds-pc-1').textContent, 'Paused');
  }
  const extensions = h.calls.filter(call => call.url === 'http://pc:18081/1/tweak');
  assert.deepEqual(extensions.map(call => JSON.parse(call.options.body)), [900, 1800, 3600].map(amount => ({ amount })));
  assert.equal(h.calls.filter(call => call.url.endsWith('/play')).length, 0);
  assert.equal(h.calls.filter(call => call.url.endsWith('/pause')).length, 0);
});

test('in-flight timer extension clicks are coalesced and update the cached display immediately', async () => {
  const h = harness([sample('pc'), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', {
    ...status, on_behalf_of: [
      { ...status, id: '1', remaining_seconds: 60, is_running: false },
    ]
  });
  h.start();
  await h.advance(0);
  h.extensions.push(
    { dataset: { pc: 'pc-1' }, disabled: false },
    { dataset: { pc: 'pc-1' }, disabled: false },
    { dataset: { pc: 'pc-1' }, disabled: false },
  );
  h.slow.add('http://pc:18081/1/tweak');
  h.slow.add('http://pc:18081/status');
  h.run('extendTimer("pc-1", 900); extendTimer("pc-1", 900);');
  await flush();
  const extension = h.calls.filter(call => call.url === 'http://pc:18081/1/tweak');
  assert.equal(extension.length, 1);
  assert.ok(h.extensions.every(button => button.disabled));
  extension[0].resolve({ remaining_seconds: 960 });
  await flush();
  assert.equal(h.node('dt-pc-1').textContent, '00:16:00');
  assert.equal(h.node('ds-pc-1').textContent, 'Paused');
  assert.ok(h.extensions.every(button => button.disabled));
});

test('special commands work for active agents when the host timer is unchecked and reject cross-timer names', async () => {
  const h = harness([sample('pc', true), agent('pc', '1')]);
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'Lock PC', short_name: 'LOCK' }],
    on_behalf_of: [{ ...status, id: '1', special_commands: [{ name: 'switch_off', short_name: 'OFF' }] }],
  });
  h.start();
  await h.advance(0);
  await h.run('specialCommand("pc", "Lock PC"); specialCommand("pc-1", "Lock PC");');
  assert.equal(h.calls.filter(c => c.url.endsWith('/special')).length, 0);
  await h.run('specialCommand("pc-1", "switch_off")');
  assert.equal(h.calls.filter(c => c.url === 'http://pc:18081/1/special').length, 1);
});

test('special labels refresh from status and clear for missing commands or legacy hosts', async () => {
  const h = harness([sample('pc')]);
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [null, {}, { name: 'Valid command' }],
  });
  h.start();
  await h.advance(0);
  assert.match(h.node('special-pc').innerHTML, /special-short-name" aria-hidden="true">Valid command</);
  h.responses.set('http://pc:18081/status', status);
  await h.advance(1000);
  assert.equal(h.node('special-pc').innerHTML, '');
  assert.equal(h.node('special-pc').hidden, true);
  await h.run('specialCommand("pc", "Valid command")');
  assert.ok(!h.calls.some(c => c.url.endsWith('/special')));
});

test('special request timeouts restore buttons and display an error instead of success', async () => {
  const h = harness([sample('pc')]);
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'switch_off', short_name: 'OFF' }],
  });
  h.start();
  await h.advance(0);
  h.slow.add('http://pc:18081/special');
  const command = h.run('specialCommand("pc", "switch_off")');
  await h.advance(4100);
  await command;
  assert.match(h.node('dm-pc').textContent, /switch_off: aborted/);
  assert.ok(!h.node('special-pc').innerHTML.includes('disabled'));
  assert.equal(h.run('pendingSpecial.size'), 0);
});

test('unchanged status preserves special button elements despite browser HTML normalization', async () => {
  const h = harness([sample('pc')]);
  let markup = '', replacements = 0;
  Object.defineProperty(h.node('special-pc'), 'innerHTML', {
    get: () => markup,
    set(value) { markup = value.replace(/\s+/g, ' '); replacements++; },
  });
  h.responses.set('http://pc:18081/status', {
    ...status,
    special_commands: [{ name: 'switch_off', short_name: 'OFF' }],
  });
  h.start();
  await h.advance(0);
  const before = replacements;
  await h.advance(3000);
  assert.equal(replacements, before, 'polling must not replace unchanged buttons and lose keyboard focus');
});
