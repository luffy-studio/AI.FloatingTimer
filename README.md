# Overlay Timer REST API

## Launcher and automatic updates (Windows x64)

Run `./build.ps1` to build both executables. Keep `FloatingTimerLauncher.exe` and
`OverlayTimer.exe` together in a writable folder, alongside your existing JSON
settings. Point desktop and startup shortcuts at **FloatingTimerLauncher.exe**.
Opening `OverlayTimer.exe` directly still works, but bypasses updates.

On every launch:

1. Verify and install any previously staged update before starting the timer.
2. Check the assets on the exact GitHub release tag
   [`latest`](https://github.com/shubham2110/AI.FloatingTimer/releases/tag/latest).
   Compare the `OverlayTimer.exe` asset's SHA-256 against the installed executable;
   replacing an asset under the same tag is detected without a version-number change.
3. Download changed content, verify its size, SHA-256 and Windows x64 executable
   header, install it as `OverlayTimer.exe`, and start it.
4. If checking/downloading is still running after two minutes, start the installed
   timer. The launcher remains alive in the background until the download finishes
   (each HTTP request has a 30-minute timeout). It leaves `OverlayTimer.pending.exe`
   and `update-pending.json` beside the app for the next launch. It does not restart
   or replace the active timer after this fallback.

Network errors, rate limits, missing assets and invalid downloads fall back to the
installed timer. Partial downloads never replace it. Updates retain
`OverlayTimer.previous.exe`; interrupted replacements recover from that backup.
If Windows refuses replacement because the timer is running, the update stays
pending: quit the timer and open the launcher again. Concurrent launchers are
serialized with an OS file lock, released automatically if the launcher crashes.
Errors are recorded in `launcher.log`.

The two-minute limit bounds waiting for the internet; Windows process startup and
local disk/antivirus operations can add time. An existing app is required for the
offline/timeout fallback, so distribute both executables initially. Settings, peer
data and activity files are preserved. The backup is also tried if Windows cannot
start the installed executable; this does not detect crashes after process startup.

For releases, upload both `OverlayTimer.exe` and `FloatingTimerLauncher.exe` with
the same names used locally. The updater selects only `OverlayTimer.exe` and ignores
the launcher asset when checking for timer updates.
The launcher requires GitHub's `sha256:` asset digest. It is updated manually;
publishing a new release alone does not update the launcher itself. No release is
published by the build script.

## Overview

The timer exposes an HTTP API on port `18081` by default (`timer_port` in configuration). The same executable serves the management UI on `18082` (`ui_discovery_port`). The ports must differ; both are reserved before services start. This reference uses only relative API paths so the UI is not tied to a particular machine.

The management UI obtains saved PC addresses from its own backend, then calls each active timer directly from the browser. Timer endpoints allow cross-origin requests with `Access-Control-Allow-Origin: *` and handle OPTIONS preflight requests.

> **Access:** Both ports listen on all network interfaces, and the API (including billing and workbook download endpoints) has no authentication or encryption. No admin token or login is required. Other devices can connect using this PC's IP address and the configured ports, subject to Windows Firewall and network rules. Anyone who can reach the ports may control timers and read or change billing data; do not expose them directly to the public internet.

## Common request and response format

- Request method: `GET` for status and `POST` for commands
- JSON request content type, when a body is required: `application/json`
- Response content type: `application/json`
- Time values are integer seconds.

Example relative request from browser JavaScript:

```javascript ui-example.js
const response = await fetch("/reset", { method: "POST" });
const result = await response.json();
```

## Endpoints

### Read current status

```http api-request.http
GET /status
```

Returns a consistent snapshot of the current timer and alert state.

Success response (`200 OK`) while running:

```json api-response.json
{
  "remaining_seconds": 42,
  "is_running": true,
  "timer_status": "running",
  "time_up_visible": false,
  "seconds_until_time_up": 42
}
```

When paused with time remaining, `timer_status` is `"paused"` and `seconds_until_time_up` is `null`, because the countdown will not reach zero until resumed. When the remaining time is zero, `seconds_until_time_up` is `0`. `time_up_visible` independently reports whether the full-screen alert is currently displayed.

Browser JavaScript example:

```javascript ui-example.js
const response = await fetch("/status");
const timer = await response.json();
```

#### Status scenarios and UI decisions

In the table below, `99` represents any value greater than zero. These are common combinations the API can return; `/show` can additionally make the alert visible in any countdown state:

| Scenario | `is_running` | `remaining_seconds` | `seconds_until_time_up` | `time_up_visible` | `timer_status` | Suggested UI decision |
|---|---:|---:|---:|---:|---|---|
| Running countdown | `true` | `99` | `99` | `false` | `"running"` | Show the remaining time and an enabled **Pause** action. |
| Paused countdown | `false` | `99` | `null` | `false` | `"paused"` | Show the remaining time and an enabled **Play/Resume** action. Do not display an expected alert time. |
| Reset/cleared | `false` | `0` | `0` | `false` | `"paused"` | Show `00:00`; prompt the operator to set time before starting. |
| Time expired, alert showing | `true` | `0` | `0` | `true` | `"running"` | Show **TIME UP** and provide **Dismiss**, **Reset**, and **Set new time** actions. |
| Time expired, alert dismissed | `true` | `0` | `0` | `false` | `"running"` | Show `00:00` and indicate that the expired alert has been dismissed. Prompt for a new time or reset. |

The last scenario can occur after `/dismiss`, `/play` at zero, or `/set` with zero while the timer is running. A running timer at zero does not count below zero and does not repeatedly reopen the alert.

#### Field rules

The following rules define valid combinations:

1. `timer_status` is derived from `is_running`:
   - `is_running: true` always means `timer_status: "running"`.
   - `is_running: false` always means `timer_status: "paused"`.
2. When `remaining_seconds` is greater than zero:
   - If running, `seconds_until_time_up` equals `remaining_seconds`.
   - If paused, `seconds_until_time_up` is `null` because expiry is not approaching.
3. When `remaining_seconds` is zero, `seconds_until_time_up` is always `0`.
4. `time_up_visible` is independent of countdown state: `/show` can display the alert while paused or while time remains.
5. Reset produces the exact combination: `false`, `0`, `0`, `false`, `"paused"`.

#### Invalid or contradictory combinations

The UI should treat any response violating the rules above as invalid rather than attempting to assign it a new meaning. Examples include:

| Invalid combination | Reason |
|---|---|
| `is_running: true` with `timer_status: "paused"` | The two running-state fields contradict each other. |
| `is_running: false` with `timer_status: "running"` | The two running-state fields contradict each other. |
| Positive `remaining_seconds` with `seconds_until_time_up: 0` | A positive running timer must report the same positive value; a paused timer must report `null`. |
| Zero `remaining_seconds` with `seconds_until_time_up: null` or a positive value | At zero, the time until expiry is always zero. |
| Negative values in either seconds field | Timer values are never negative. |

Because commands and status polling can occur concurrently, a UI may very briefly observe a transitional snapshot while an alert is being dismissed. Poll again before treating one unusual response as a persistent error.

#### Recommended UI state selection

Use the fields in this priority order:

```javascript ui-status-decision.js
if (timer.time_up_visible) {
  showTimeUpState();
} else if (timer.remaining_seconds === 0 && !timer.is_running) {
  showResetState();
} else if (timer.remaining_seconds === 0) {
  showExpiredDismissedState();
} else if (timer.is_running) {
  showRunningState(timer.remaining_seconds);
} else {
  showPausedState(timer.remaining_seconds);
}
```

Do not use `timer_status` as a separate source of truth; it is a readable representation of `is_running`. Likewise, `seconds_until_time_up` is derived from the running state and remaining time.

### Set the exact remaining time

```http api-request.http
POST /set
Content-Type: application/json

{
  "seconds": 300
}
```

Sets the timer to exactly the supplied number of seconds. The value must be a non-negative integer. This operation dismisses the **TIME UP** screen but preserves the current running or paused state.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "remaining_seconds": 300
}
```

Invalid request response (`400 Bad Request`):

```json api-response.json
{
  "status": "error",
  "message": "seconds must be zero or greater"
}
```

The value may alternatively be supplied as a query parameter:

```text api-route.txt
POST /set?seconds=300
```

### Adjust the remaining time

```http api-request.http
POST /tweak
Content-Type: application/json

{
  "amount": 60
}
```

Adds the supplied number of seconds to the current value. Use a negative value to subtract time. The result is clamped to zero and cannot become negative. This operation dismisses the **TIME UP** screen but does not change the running or paused state.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "remaining_seconds": 360
}
```

The value may alternatively be supplied as a query parameter:

```text api-route.txt
POST /tweak?amount=60
POST /tweak?amount=-30
```

### Start or resume

```http api-request.http
POST /play
```

Starts or resumes countdown and dismisses the **TIME UP** screen.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

If the remaining time is zero, `/play` dismisses the alert but the timer remains at zero. Set or add time before playing.

### Pause

```http api-request.http
POST /pause
```

Pauses countdown without changing the remaining time and dismisses the **TIME UP** screen.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

### Toggle running state

```http api-request.http
POST /toggle
```

Switches between running and paused and dismisses the **TIME UP** screen.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

For a UI, prefer `/play` and `/pause` because they are explicit and idempotent. Repeating `/toggle` can accidentally produce the wrong state.

### Reset to zero

```http api-request.http
POST /reset
```

Sets the timer to `00:00`, stops it, and dismisses the **TIME UP** screen. Resetting does not trigger a new alert.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

### Set overlay position

```http api-request.http
POST /position
Content-Type: application/json

{
  "x": 10,
  "y": 10
}
```

Moves the timer overlay to an exact screen position in physical pixels. `x` is measured from the left of the primary screen and `y` from the top. Both values must be non-negative integers. The default position is near the top-left at `(10, 10)`.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "x": 10,
  "y": 10
}
```

### Enable or disable local dragging

```http api-request.http
POST /drag
Content-Type: application/json

{
  "enabled": true
}
```

When enabled, the operator at the timer machine can press and drag the timer overlay. Drag mode temporarily disables click-through behavior. Disable drag mode afterward to restore click-through:

```http api-request.http
POST /drag
Content-Type: application/json

{
  "enabled": false
}
```

Success response (`200 OK`):

```json api-response.json
{
  "status": "success",
  "drag_enabled": false
}
```

The same controls are available from the tray as **Enable dragging** and **Disable dragging (click-through)**. Drag mode is disabled by default.

### Dismiss the TIME UP screen

```http api-request.http
POST /dismiss
```

Hides the full-screen **TIME UP** alert without changing the remaining time or running state.

Success response (`200 OK`):

```json api-response.json
{
  "status": "success"
}
```

## Typical UI operations

### Set five minutes and start

```javascript ui-example.js
await fetch("/set", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ seconds: 5 * 60 })
});
await fetch("/play", { method: "POST" });
```

### Add one minute

```javascript ui-example.js
await fetch("/tweak", {
  method: "POST",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ amount: 60 })
});
```

### Stop and clear the timer

```javascript ui-example.js
await fetch("/reset", { method: "POST" });
```

### Dismiss only the alert

```javascript ui-example.js
await fetch("/dismiss", { method: "POST" });
```

## Operator UI and multiple machines

The frontend loads `GET /api/pcs` from its own manager. Commands use each active
system's `address` as the base URL, including its numeric on-behalf path when present.
Status uses one root `/status` request per host and updates all its active systems.
Requests go directly to the timer host. The former `/api/pcs/{id}/{action}` proxy
routes have been removed. Local timer command activity is logged on the timer host.

```javascript
const pcs = await (await fetch('/api/pcs')).json();
const activePC = pcs.find(pc => !pc.inactive);
if (activePC) {
  const timerID = activePC.address.match(/\/(\d+)$/)?.[1] || '';
  const host = activePC.address.replace(/\/\d+$/, '');
  const all = await (await fetch(host + '/status')).json();
  const status = timerID ? all.on_behalf_of?.find(t => t.id === timerID) : all;
  await fetch(activePC.address + '/set', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({seconds: 300})
  });
}
```

All machines must run the updated timer API for direct browser access. CORS allows
all origins, supported API methods, and request headers without credentials. The
manager's Content Security Policy permits HTTP/HTTPS connections to timer hosts.

## Current API limitations

- Status is retrieved by polling `GET /status`; there are no push updates such as WebSocket or Server-Sent Events.
- There is no authentication or authorization.
- There is no HTTPS support in the application itself.
- Timer state is held in memory and is lost when the process exits.

An operator UI can poll `GET /status` periodically. A real-time push mechanism could be added later if polling is not sufficient.

## Merged application additions

Build the complete package on Windows, including the embedded `ui.html`:

```powershell
go build -ldflags="-H=windowsgui -s -w" -o OverlayTimer.exe .
```

No separate UI executable or deployed HTML file is needed. The JSON config, peer registry,
and activity files live beside the executable.

Alternatively, run `./build.ps1`. The `-s -w` flags strip debug information to avoid
malformed Windows executable headers with Go 1.25 and older MinGW/CGO toolchains.

| Route | Behavior |
|---|---|
| `GET /identity` | Application identity, friendly name, ports, and a `timers` list containing the local and all on-behalf timers. |
| `POST /show` | Display TIME UP without changing time or running state. |
| `POST /hide` | Hide TIME UP; `/dismiss` remains an alias. |
| `GET /status` | Existing local fields plus `name` and an `on_behalf_of` array. |
| `GET /1/status` | Status for configured on-behalf timer 1. |
| `POST /1/{action}` | Set, tweak, play, pause, toggle, reset, show, hide, or dismiss timer 1. |
| `POST /special` | Run a locally configured special command by name for the host's own timer. |
| `POST /1/special` | Run a locally configured special command by name for on-behalf timer 1. |

Other numeric IDs work the same way. Each on-behalf status includes `id` and `name` plus
the standard timer fields. External commands come only from local configuration and
remain asynchronous. Command ordering is not guaranteed. Each timer's activity CSV is
stored in its numeric subdirectory, with the same columns as the primary activity CSV.

See [the identity manifest contract](API.md#identity-manifest) for the advertised timer
list and compatibility behavior. Update hosts and managers, reload the browser, and
run Discover to register separate on-behalf system cards. Older hosts without the
manifest still produce their own root entry; there is no extra status discovery probe.

## Set up on-behalf timers

Use on-behalf mode when **your PC keeps its own timer and also keeps separate
countdowns for other machines**. Your PC owns and runs all those countdowns. Each
on-behalf timer is independent of your main timer and of the other on-behalf timers.

There is no special "on-behalf machine" installation mode. Add an `on_behalf_of`
entry to the configuration on **the PC doing the counting**. The entry's commands
determine what happens on the other machine when time expires or the alert is hidden.

This example uses these addresses; replace them with your real LAN addresses:

| Machine | Address | Responsibility |
|---|---|---|
| Your PC (PC A) | `192.168.1.10` | Runs its own timer and on-behalf timer `1` for PC B. |
| Other machine (PC B) | `192.168.1.20` | In the remote-alert example, runs Overlay Timer to display/hide TIME UP when PC A requests it. |

The countdown for PC B is stored **on PC A**, at
`http://192.168.1.10:18081/1/status`. PC A's own timer remains at
`http://192.168.1.10:18081/status`. PC B's `/status` describes PC B's separate local
timer; it does not mirror the on-behalf countdown on PC A.

### 1. Decide whether the other machine needs to show an alert

- **Only track its time on your PC:** PC B does not need Overlay Timer. On PC A,
  use the configuration below but set `show_command` and `hide_command` to `[]`.
  You can view/control the countdown in PC A's manager. Expiry updates its virtual
  alert state; it does not open a separate overlay on either PC.
- **Show TIME UP on PC B:** use both configurations below. PC A sends HTTP commands
  to Overlay Timer running on PC B. These commands show/hide the full-screen alert;
  they do not send PC A's remaining seconds to PC B's countdown overlay.

### 2. Configure your PC (PC A)

Quit Overlay Timer from the tray before editing. Open `overlay_timer_config.json`
beside **PC A's `OverlayTimer.exe`**. Add or replace its `on_behalf_of` array. Keep
your existing settings for your own timer unless you intend to change them.

Here is a complete example configuration. The commands use Windows PowerShell,
running on PC A, to call PC B's timer API:

```json
{
  "kill_on_time_up": false,
  "apps_to_close_on_time_up": [],
  "force_kill_after_seconds": 2,
  "friendly_name": "My PC - Timer Host",
  "timer_port": 18081,
  "ui_discovery_port": 18082,
  "discovery": {
    "enabled": false,
    "interval_seconds": 300,
    "cidr_ranges": [],
    "connect_timeout_milliseconds": 350,
    "maximum_concurrency": 32
  },
  "on_behalf_of": [
    {
      "id": "1",
      "name": "PC B",
      "show_command": [
        "powershell.exe",
        "-NoProfile",
        "-NonInteractive",
        "-Command",
        "$ErrorActionPreference = 'Stop'; Invoke-RestMethod -Method Post -Uri 'http://192.168.1.20:18081/show' -TimeoutSec 8 | Out-Null"
      ],
      "hide_command": [
        "powershell.exe",
        "-NoProfile",
        "-NonInteractive",
        "-Command",
        "$ErrorActionPreference = 'Stop'; Invoke-RestMethod -Method Post -Uri 'http://192.168.1.20:18081/hide' -TimeoutSec 8 | Out-Null"
      ],
      "command_timeout_seconds": 10
    }
  ]
}
```

Replace `192.168.1.20` in **both commands** with PC B's address. If PC B uses a
different timer port, replace `18081` in both commands too. The URL must point to
PC B's timer API port, not its management UI port.

| Entry field | Meaning |
|---|---|
| `id` | A unique string containing digits, such as `"1"` or `"2"`. It selects the URL prefix on PC A. Invalid or duplicate IDs are ignored at startup. |
| `name` | Label shown in the manager. An empty name becomes `Agent <id>`. |
| `show_command` | Program and arguments to execute on PC A when this timer reaches zero or receives `show`. Use `[]` for no external action. |
| `hide_command` | Program and arguments to execute on PC A for every other standard timer command. Special commands run independently. Use `[]` for no external action. |
| `special_commands` | Optional list of named buttons with `name`, `short_name`, and `executable`. See [special command buttons](#special-command-buttons). |
| `command_timeout_seconds` | Timeout for each command process, not the countdown duration. Values below 1, or omission, use 10 seconds. |

Commands are JSON arrays: the first string is the executable, and subsequent
strings are individual arguments. There is no automatic shell, remote execution,
or substitution of the timer ID/time into arguments. The PowerShell example
explicitly starts a shell and explicitly supplies the destination URL. For custom
programs or scripts, prefer absolute paths and double Windows backslashes in JSON,
for example `"C:\\TimerScripts\\show.ps1"`. Commands run under the account running
Overlay Timer on PC A and must work without interactive prompts.

### 3. Configure the other machine (PC B) for remote alerts

Skip this step if you only want to track time on PC A with empty command arrays.

Keep `FloatingTimerLauncher.exe` and `OverlayTimer.exe` on PC B. Beside PC B's
`OverlayTimer.exe`, use this `overlay_timer_config.json` (or merge the settings
into its existing configuration):

```json
{
  "kill_on_time_up": false,
  "apps_to_close_on_time_up": [],
  "force_kill_after_seconds": 2,
  "friendly_name": "PC B",
  "timer_port": 18081,
  "ui_discovery_port": 18082,
  "discovery": {
    "enabled": false,
    "interval_seconds": 300,
    "cidr_ranges": [],
    "connect_timeout_milliseconds": 350,
    "maximum_concurrency": 32
  },
  "on_behalf_of": []
}
```

PC B does **not** need an entry pointing back to PC A. It only receives ordinary
`POST /show` and `POST /hide` requests. Start the launcher on both PCs after saving
their configurations; configuration changes require an app restart, not a rebuild.
Run PC B's app in the logged-in desktop session where the alert should appear.

Allow PC A to reach PC B's TCP timer port (`18081` here) through Windows Firewall.
Using the same port numbers on different PCs is fine; the timer and UI ports must
differ within each PC. Discovery and Distribute are not required for these hooks.
Keep these unauthenticated APIs on your trusted LAN; no public port forwarding is
needed.

### 4. Verify the connection and use the timers

From PowerShell on **PC A**, check that PC B's API is reachable:

```powershell
Invoke-RestMethod -Uri 'http://192.168.1.20:18081/identity' -TimeoutSec 8
```

The response should identify `service` as `overlay-timer`. Test the remote alert
manually, running the hide command after observing TIME UP on PC B:

```powershell
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.20:18081/show' -TimeoutSec 8
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.20:18081/hide' -TimeoutSec 8
```

Showing TIME UP also minimizes windows on PC B; hiding it restores them. A forced
`/show` does not invoke PC B's configured app-closing behavior.

Open `http://192.168.1.10:18082` in your browser and click **Discover**. PC A and
its on-behalf timer **PC B** appear as separate systems, each with its own card,
selection checkbox, and controls. PC B's card uses `http://192.168.1.10:18081/1`;
you can also use **Add system** with that address. Open its **Details** to set a
duration or adjust remaining time. On mobile its Play/Pause control is a toggle.
PC A's card operates PC A's own timer. Both cards share a single request to
PC A's `/status`; no separate `/1/status` polling is needed. Overlay positioning
and drag controls apply only to the host's own timer and are hidden for PC B.

You can also control PC B's on-behalf countdown through **PC A's** API:

```powershell
# Start a separate five-minute countdown for PC B, owned by PC A.
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.10:18081/1/set?seconds=300'
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.10:18081/1/play'

# Inspect, add one minute, pause, or resume that same timer.
Invoke-RestMethod -Uri 'http://192.168.1.10:18081/1/status'
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.10:18081/1/tweak?amount=60'
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.10:18081/1/pause'
Invoke-RestMethod -Method Post -Uri 'http://192.168.1.10:18081/1/play'
```

To test expiry quickly, set `seconds=10` and play, then wait for PC B's alert.
`POST /1/hide` or `/1/dismiss` hides it without resetting the countdown;
`POST /1/reset` clears that countdown and hides it. Routes without `/1`, such as
`POST /set`, affect PC A's own timer.

### Hook behavior and limits

| Event on PC A's on-behalf timer | Local countdown/alert state | Command executed on PC A |
|---|---|---|
| A running positive countdown reaches zero | Marks the virtual alert visible. | `show_command` |
| `show` | Marks the alert visible without changing time/running state. | `show_command` |
| `set`, `tweak`, `play`, `pause`, `toggle`, `reset`, `hide`, or `dismiss` | Applies that action and clears the virtual alert. | `hide_command`, even if the alert was already hidden. |

The hooks run asynchronously. An API success confirms that PC A accepted the
timer action; it does not confirm that PC B received or displayed the alert.
`time_up_visible` in `/1/status` is PC A's virtual state, not feedback from PC B.
Command completion order is not guaranteed, so rapid show/hide operations can
finish out of order. There is no automatic retry or replay if PC B is offline
when a hook runs. After restoring the connection, send `/1/show` or `/1/hide`
again as appropriate.

PC A must stay running and awake to keep these countdowns. New on-behalf timers
start paused at zero; their times/running states are not restored after restart.
The browser can be closed while counting. In this example, PC B must stay running
and reachable to display the remote alert. Do not run a second independent
countdown on PC B expecting it to stay synchronized with PC A's `/1` timer.

### Add more machines or troubleshoot

For another machine, add another object to **PC A's** `on_behalf_of` array with
`"id": "2"`, its display name, and its own show/hide URLs. Its countdown will be at
`http://192.168.1.10:18081/2/status`. Use one owner for each countdown; neither Add PC
in the manager nor automatic discovery creates `on_behalf_of` entries.

| Problem | What to check |
|---|---|
| No separate card for timer 1 | Update the host and manager executables, restart them, reload the browser, and run Discover. Check that PC A's `/identity` lists `/1` in `timers` and that the system is checked under Select active systems. A removed record must be explicitly added again. |
| `/1/status` returns 404 | Edit the config beside the running PC A executable, use a unique numeric string ID, and restart PC A's app. |
| Configuration seems ignored | Check JSON syntax (no comments or trailing commas). Invalid JSON causes default settings to be used; startup errors are in `overlay_timer-YYYY-MM-DD.log`. |
| Countdown works but PC B shows no alert | First run the direct `/identity` and `/show` checks from PC A. Check the address, timer port, firewall, PC B's app/session, and the hook log. |
| Command fails or times out | Inspect `1/overlay_ui_activity.csv` beside PC A's executable. The hook result records output/errors or `command timed out`; timer `2` uses `2/overlay_ui_activity.csv`. |
| PC A's timer changes instead of PC B's virtual timer | Use the agent's controls or PC A's `/1/...` routes, not PC A's unprefixed routes. |
| PC B's displayed countdown differs | Expected: this setup sends show/hide only. View the authoritative countdown on the system card whose address ends in PC A's `/1`. |

### Special command buttons

Add `special_commands` at the top level of `overlay_timer_config.json` for the host's
own timer, or inside an `on_behalf_of` entry for that timer. Each list belongs only to
that system; the same name can select different programs on different systems.
Merge these fields into your existing configuration, keeping your show/hide hooks:

```json
{
  "special_commands": [
    {
      "name": "Lock PC",
      "short_name": "LOCK",
      "executable": ["rundll32.exe", "user32.dll,LockWorkStation"]
    }
  ],
  "command_timeout_seconds": 10,
  "on_behalf_of": [
    {
      "id": "1",
      "name": "Stage display",
      "special_commands": [
        {
          "name": "switch_off",
          "short_name": "OFF",
          "executable": [
            ".\\extensions\\lgtv\\lgtv_remote.exe",
            "-ip", "192.168.1.4",
            "off"
          ]
        }
      ],
      "command_timeout_seconds": 10
    }
  ]
}
```

Replace the example executable paths and TV IP with your own. This LG example turns
off the **TV**; it is not a dedicated PS5 full-shutdown command. Pair the LG tool first
so it can run without a pairing prompt.

- Desktop buttons display `name`; mobile layouts (720px or narrower) display
  `short_name`, with the full name retained as the tooltip/accessibility label.
  Buttons appear on both the system card and its Details page.
- `name` is also the exact, case-sensitive command identifier. Names may contain
  spaces. Leading/trailing whitespace is removed. Omitted/empty `short_name` falls
  back to `name`. Empty names, control characters, missing executables, and duplicate
  names within a system are ignored and logged at startup.
- `executable` is an array containing the program followed by individual arguments,
  matching the show/hide hook convention. A single path string is also accepted for
  a program without arguments. A string containing a whole shell command is not split.
  To run a PowerShell script, use an array such as
  `["powershell.exe", "-NoProfile", "-NonInteractive", "-File", "C:\\TimerScripts\\off.ps1"]`.
- Programs execute on the machine hosting the timer, under its running user account.
  The working directory is the folder containing `OverlayTimer.exe`. Relative program
  paths containing a directory component (such as `.\\tools\\off.exe`) are resolved
  from that folder; bare program names are looked up using the process PATH.
- The owner's `command_timeout_seconds` limits each process, defaulting to 10 seconds.
  Calls return **202 Accepted** and run asynchronously. The UI shows **requested**;
  completion/failure/timeout is recorded as a `special_command` event in the host's
  `overlay_ui_activity.csv`, or the on-behalf timer's `<id>/overlay_ui_activity.csv`.
  A button is disabled while its HTTP request is pending; later clicks can start
  another process. Commands do not automatically change countdown or alert state
  and do not invoke the show/hide hooks.
- Labels appear in `/identity` and the existing `/status` responses. Executable paths
  and arguments remain local. The UI still uses one aggregate status request per host;
  it does not make separate identity or on-behalf status requests to fetch buttons.

Restart the owning timer app after editing its configuration, and reload the updated
manager UI. Existing systems pick up buttons on their next status poll; run Discover
only if you also added new on-behalf systems. Systems with no commands show no extra buttons.

### Management and registry behavior

- `GET /api/pcs` returns all saved non-deleted PCs, including their local `inactive` preference.
- `POST /api/pcs` adds a PC; `PUT/DELETE /api/pcs/{id}` edits/removes it. Add/edit accepts
  `name`, `address`, and optional `ui_port`; omission preserves the existing UI port.
- `PUT /api/pcs/{id}/active` with `{"active": false}` disables a PC. New and legacy PCs
  are active by default. Selection persists beside the executable in `overlay_ui_pcs.json`.
- Inactive systems only appear in **Select active systems**, not in the dashboard or
  detail list, and cannot be commanded or distributed. Disabling cancels pending commands;
  commands already delivered cannot be undone. A host's shared status request continues
  while any of its timers is active, even if its own timer is unchecked. Discovery skips
  disabled/deleted root hosts unless an active sibling needs them; disabling an on-behalf
  timer never blocks discovery of its host. Rediscovery preserves per-timer selection.
- Each host with active timers has one independent status schedule (default **5 seconds**),
  including while a detail view is open. One root `/status` response updates the host's
  own card and matches on-behalf cards by ID in `on_behalf_of`. No `/1/status` requests
  are sent by the manager. A missing/invalid agent is marked **Unavailable**. If the
  previous request is pending, only that host skips the tick; requests time out after
  four seconds. Slow/offline hosts do not delay others. Background tabs can slow timers.
- Open **Settings** in the header to choose a polling interval of **1–300 whole
  seconds**. It applies to every active PC in this browser. Saving changes updates
  existing schedules immediately without duplicating pending requests. The settings
  are saved in browser storage for this manager's origin; other browsers/devices
  keep their own preferences. No EXE configuration change is needed.
- **Enable false polling**, below the interval setting, is **off by default**.
  When enabled, running countdowns update locally every second between real status
  responses, including on-behalf countdowns in the dashboard and detail view. A
  fresh response replaces the estimate. Paused timers stay fixed; estimates stop
  at zero and never trigger TIME UP or send timer commands. If a status request
  fails, the display is marked offline and the estimate freezes until a successful
  response. Disabling false polling displays the last actual sample. Local ticks
  use elapsed time, so delayed browser callbacks do not accumulate countdown drift.
- The saved list refreshes separately every 15 seconds with a four-second timeout.
  Failed list refreshes preserve the existing PCs and their status schedules.
  **Refresh** starts list and status refreshes together; pending requests are reused.
  Removing, disabling, or changing a system cancels its old command requests. The old
  host's status request and schedule stop once it has no active systems left.
- **Discover** calls `POST /api/discovery/run`; progress is read with
  `GET /api/discovery/status`. It reads `/identity` once per responding host and saves
  a separate URL for every advertised timer, without probing `/status` or distributing
  the list. Results include `hosts_found` and `found` (timers). Local timers are also
  registered at startup, even when automatic discovery is disabled.
- **Distribute** calls `POST /api/discovery/distribute` with `{"ids":["selected-pc-id"]}`.
  The manager sends only those active records to the selected active machines' configured
  UI/discovery ports, once per host even when several selected timers belong to it.
  Results report host successes and per-destination errors. Hosts with no selected
  active timers receive no request. Receiving records never triggers another distribution.
- `GET/POST /api/discovery/peers` exports active records or imports received records.
  Peer imports preserve this manager's selection; remote inactive flags are never imported.

Automatic discovery is disabled by default and in the supplied configuration. Enable
it explicitly with `discovery.enabled: true`; `interval_seconds` defaults to 300. When
enabled, the existing lowest-reachable-IP leader logic is used. Distribution remains
an explicit button action even when automatic discovery is enabled.

Addresses (including an optional numeric timer path such as `/1`) are normalized registry
keys. Timer 1 on two different hosts has different registry IDs. IDs remain stable locally, duplicate addresses
return HTTP 409, and metadata uses newest-edit-wins with a deterministic tie break.
Local deletion tombstones prevent old peer records from restoring removed addresses;
explicit Add system can restore an address. Inactive selection is separate from shared
metadata and survives edits, imports, and restarts. Failed saves roll back memory.

Discovery still probes the configured timer port; differently configured remote timer
ports need manual registration or an existing peer record. A discovery observation
older than 15 minutes is marked stale, without deleting the record.
