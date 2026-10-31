# Overlay Timer REST API

## Overview

The timer exposes an HTTP API on port `18081` by default (`timer_port` in configuration). The same executable serves the management UI on `18082` (`ui_discovery_port`). The ports must differ; both are reserved before services start. This reference uses only relative API paths so the UI is not tied to a particular machine.

The management UI obtains saved PC addresses from its own backend, then calls each active timer directly from the browser. Timer endpoints allow cross-origin requests with `Access-Control-Allow-Origin: *` and handle OPTIONS preflight requests.

> **Access:** Both ports listen on all network interfaces. The API has no authentication or encryption; use this PC's IP address from other devices. Firewall and network rules still apply. Anyone who can reach the ports may control timers and access billing data, so do not expose them directly to the public internet.

## Common request and response format

- Request method: `GET` for status and `POST` for commands
- JSON request content type, when a body is required: `application/json`
- Response content type: `application/json`
- Time values are integer seconds.
- The embedded dashboard routes all `fetch` calls through its monitored request helper. API clients may use their own HTTP library; incoming service logs still include request ID, method, path, matched route, status, device header (if provided), and duration.
- The dashboard adds `X-Request-ID` and, when known, `X-Device-ID`. A management request forwarded to a peer receives a new request ID and carries the originating ID in `X-Parent-Request-ID`.

### Management health check

The management service on the configured UI port (18082 by default) provides a read-only health endpoint:

```http api-request.http
GET /api/health
```

Success response (`200 OK`):

```json api-response.json
{"status":"ok","service":"overlay-timer"}
```

This endpoint confirms only that the management service is responding. It does not test timer reachability, billing availability, or synchronization status.

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
| `POST /special` | Run a named special command configured for the host's own timer. |
| `POST /1/special` | Run a named special command configured for on-behalf timer 1. |

Other numeric IDs work the same way. Each on-behalf status includes `id` and `name` plus
the standard timer fields. External commands come only from local configuration and
remain asynchronous. Command ordering is not guaranteed. Each timer's activity CSV is
stored in its numeric subdirectory, with the same columns as the primary activity CSV.

### Identity manifest

`GET /identity` on the timer port and `GET /api/node` on the UI port return the same
identity. For a host running its own timer and on-behalf timer 1:

```json
{
  "service": "overlay-timer",
  "name": "PC4",
  "timer_port": 18081,
  "ui_port": 18082,
  "timers": [
    { "id": "", "name": "PC4", "path": "", "special_commands": [] },
    {
      "id": "1",
      "name": "Stage display",
      "path": "/1",
      "special_commands": [{ "name": "switch_off", "short_name": "OFF" }]
    }
  ]
}
```

The empty ID/path denotes the host's own timer. On-behalf IDs are numeric strings;
they need not be consecutive. Each path must equal `/` plus its ID. Discovery ignores
invalid or duplicate entries and registers a separate address for each valid timer.
The original top-level identity fields remain present. Older identities without
`timers` produce only the host's own entry; discovery does not probe `/status` as a
fallback. Update timer hosts and managers to enable manifest discovery and sharing
of on-behalf records. Reload the UI and run Discover after updating.

The browser groups registry addresses by host and timer port. It polls only the root
`/status`, then matches each virtual card's ID against the response's `on_behalf_of`
array. For example, the record `http://192.168.1.15:18081/1` gets its countdown from
`http://192.168.1.15:18081/status` and sends play to
`http://192.168.1.15:18081/1/play`. `/1/status` remains available for direct API use.
On-behalf cards have their own selection and controls; the manager does not duplicate
them inside the root card. Overlay positioning and drag are available only for the
host's own timer. Missing/invalid on-behalf status is displayed as **Unavailable**.

### Special command API

Define `special_commands` in the host's local configuration, either at the root or
inside an `on_behalf_of` entry. See the [configuration example](README.md#special-command-buttons).
Only `name` and `short_name` are advertised in each identity timer and in each timer
status's `special_commands` array. Executable paths and arguments are never advertised
and cannot be supplied or changed through these routes.

```http
POST /1/special
Content-Type: application/json

{"name":"switch_off"}
```

Use `/special` for the host's own timer. Names are case-sensitive and scoped to the
selected timer; a root request cannot select an on-behalf command. The only request
field is `name`. The response is:

```json
{"status":"accepted","name":"switch_off"}
```

HTTP **202** means the process has been scheduled, not that its action succeeded.
Malformed bodies/extra fields return **400**, unknown commands return **404**, and
methods other than POST return **405** (CORS preflight OPTIONS returns **204**).
Execution uses the executable and arguments from local configuration, with the
owner's `command_timeout_seconds` (default 10 seconds). Processes run asynchronously
on the owning host. Completion, exit failures, and timeouts are recorded as
`special_command` events in that timer's activity CSV. The handler leaves countdown
and alert state unchanged and does not run show/hide hooks.

The UI reads button labels from its existing aggregate root `/status` request and
matches each on-behalf entry by ID. Desktop layouts display `name`; mobile layouts
display `short_name` (falling back to `name` when empty). Both card and Details views
have the buttons. Pending requests disable the matching button in both views, and
offline buttons are disabled until a fresh status response arrives. Existing systems
receive updated labels without rediscovery after their host app restarts.

### Management and registry behavior

- `GET /api/pcs` returns all saved non-deleted PCs, including their local `inactive` preference.
- `POST /api/pcs` adds a PC; `PUT/DELETE /api/pcs/{id}` edits/removes it. Add/edit accepts
  `name`, `address`, and optional `ui_port`; omission preserves the existing UI port.
- `PUT /api/pcs/{id}/active` with `{"active": false}` disables a PC. New and legacy PCs
  are active by default. Selection persists beside the executable in `overlay_ui_pcs.json`.
- Inactive systems only appear in **Select active systems** and cannot be commanded or
  distributed. Disabling cancels pending commands; delivered commands cannot be undone.
  Shared root status polling continues while any sibling timer is active. Disabling a
  virtual timer never blocks its host's discovery; a disabled/deleted root only blocks
  scanning when there are no active siblings. Rediscovery preserves local selection.
- Status polls run once per host every five seconds by default (configurable in browser
  Settings), with a four-second timeout and shared in-flight requests. The saved list
  refreshes every 15 seconds. **Refresh** reloads the list and fetches active host status.
  Removing/disabling the last active timer on a host cancels its status request/schedule.
- **Discover** calls `POST /api/discovery/run`; progress is read with
  `GET /api/discovery/status`. Discovery reads the identity manifest and saves timer
  URLs locally without status probes or distribution. `hosts_found` counts responding
  hosts; `found` counts advertised timers. Local timers are registered at startup too.
- **Distribute** calls `POST /api/discovery/distribute` with `{"ids":["selected-pc-id"]}`.
  The manager sends only those active records to the selected active machines' configured
  UI/discovery ports once per host. `sent` counts successful host deliveries; results
  include per-destination errors. Hosts without selected active timers receive no request.
  Receiving records never triggers another distribution.
- `GET/POST /api/discovery/peers` exports active records or imports received records.
  Peer imports preserve this manager's selection; remote inactive flags are never imported.

Automatic discovery is disabled by default and in the supplied configuration. Enable
it explicitly with `discovery.enabled: true`; `interval_seconds` defaults to 300. When
enabled, the existing lowest-reachable-IP leader logic is used. Distribution remains
an explicit button action even when automatic discovery is enabled.

Addresses, including optional numeric timer paths such as `/1`, are normalized registry
keys. Each host/timer pair has a distinct ID. IDs remain stable locally, duplicate addresses
return HTTP 409, and metadata uses newest-edit-wins with a deterministic tie break.
Local deletion tombstones prevent old peer records from restoring removed addresses;
explicit Add system can restore an address. Inactive selection is separate from shared
metadata and survives edits, imports, and restarts. Failed saves roll back memory.

Discovery still probes the configured timer port; differently configured remote timer
ports need manual registration or an existing peer record. A discovery observation
older than 15 minutes is marked stale, without deleting the record.

## Billing sessions

The manager exposes `GET /api/sessions` for session history and `POST /api/sessions`
to start a billed session. A start request includes `pc_id`, `category`,
`customer_name`, `players`, and `duration_minutes`. The server resolves the selected
system's registered address, reads its existing timer status, sets the purchased
duration, and starts that timer before persisting the session. Numeric path suffixes
such as `/1` are preserved, so a virtual timer is not confused with its host.

For the fixed Paradise Gaming assignments, the server derives the category from the
existing registered PC name: PC 1 and PC 2 are `PC Pro`, PC 3 and PC 4 are `PC Lite`,
and PS5 is `PS5`. A supplied mismatching category is rejected; the dashboard does not
allow a category override. `GET /api/pricing/quote` is the authoritative bidirectional
quote endpoint and accepts either `duration_minutes` or `amount`. Exact 15/30/60-minute
packages retain their listed prices. Other durations use the least-cost combination of
those packages plus a rounded remainder at the 15-minute package rate, with a ₹15
minimum charge. Manual amounts return the longest duration that does not exceed the
entered amount.

The server rejects duplicate open sessions and reports timer errors instead of
recording a failed start as successful. If the timer command or workbook write fails
after the timer has been changed, the server attempts to restore the timer's prior
remaining time, running state, and overlay visibility; a failed restoration is
reported explicitly.

Each device's `billing.xlsx` workbook has a `Sessions` worksheet with the visible
columns `Date`, `Time`, `Customer`, `System`, `Players`, `Start Time`,
`Purchased Minutes`, `Amount Paid`, `Additional Minutes`, `Purchased Amount`,
`Additional Amount`, `Total Amount`, `Cash Received`, `Online Received`, `Amount Due`,
`Payment Status`, `End Time`, and `Session ID`. Date/time cells use Excel date/time values
with `DD-MM-YYYY` and `HH:MM:SS` display formats. New sessions record `Amount Paid` as
zero at start; the initial bill is unpaid until an operator records one or more payments.
Session identifiers and fields needed by the API are stored in row comments rather
than extra worksheet columns. The reporting hub stores idempotent copies imported
from registered PCs. Existing session and payment data is migrated after a workbook
backup; payment history is retained in the associated session metadata. Complete
transaction ledgers are also exported to the `Extensions`, `Payments`, `Snacks`, and
`Menu` worksheets.

### Deferred payment, extensions, and snacks

`POST /api/sessions/{id}/pay` records a payment without requiring the session to be open.
The request is `{ "amount": 100, "method": "cash|online", "request_id": "unique-id" }`.
An amount of zero settles the current balance. Reusing a request ID is idempotent; a
different amount or method with the same ID is rejected. The session exposes
`final_amount`, `amount_paid`, `amount_due`, `cash_paid`, `online_paid`,
`payment_status`, and `payment_history`.

`POST /api/sessions/extend` accepts custom `minutes` or a custom `amount` and a unique
`request_id`. Every successful extension adds one immutable record to `extensions` and
the `Extensions` worksheet. Timer changes and the billing record are committed together;
timer state is rolled back when the billing write fails.

The dashboard uses a deliberately simple quick-charge workflow rather than exposing a
product catalog or inventory screen. `POST /api/sessions/{id}/snacks/charge` accepts
`amount` and `request_id`; each successful charge is stored as an immutable
`Snacks & Beverages` transaction. `POST /api/sessions/{id}/snacks/charge-adjust`
accepts `order_id`, `amount_delta`, and `request_id` for a correction or removal;
the adjustment is appended to the ledger and never overwrites the original charge.
Snack totals are included in `final_amount`, while `gaming_amount` and `snack_amount`
remain separate. The existing menu/order endpoints remain available only for
backward-compatible historical records and are not required by the dashboard UI.

### Local PC ownership and synchronization

Each gaming PC stores the authoritative session in its own `billing.xlsx`. New
installations enable billing by default; existing configurations with
`billing.enabled: false` must be changed to `true` on each PC and restarted.
Every installation persists a stable ID in `billing_device_id.txt`; an optional
`billing.device_id` override must be unique per physical PC and remain unchanged.
The dashboard host is the reporting hub and must have the PCs registered with
their management/UI ports (normally `18082`) reachable.

The dashboard routes remote session starts and changes to the exact owning PC
endpoint. A background pull runs every 30 seconds; the dashboard's **Sync PCs**
button triggers it immediately. Source records remain on their PCs. The hub
upserts snapshots by `(device_id, session_id)` and acknowledges only the matching
`updated_at`, so a failed acknowledgement is safe to retry.

| Method and path | Description |
| --- | --- |
| `POST /api/billing/sync` | Pull pending records from the hub's registered PCs. Returns per-device online, pending, error, and last-successful-sync state plus an overall `complete` flag. |
| `GET /api/billing/report?date=YYYY-MM-DD` | Return synchronized session records, registered system count, session count, distinct named customer count, billed/collected/outstanding amounts, PC-wise totals, and device sync state for the requested date. |
| `GET /internal/api/billing/sync/pending` | Peer API used by the hub to read a device's not-yet-acknowledged session snapshots. |
| `POST /internal/api/billing/sync/ack` | Peer API used by the hub to acknowledge exact session IDs and update timestamps after its workbook write succeeds. |

The report groups each session by its start date in `Asia/Kolkata`, including
sessions that continue past midnight; a session is reported on its start date
only. `session_count` counts session records. `unique_customer_count` counts
non-empty customer names after trimming surrounding whitespace and comparing
case-insensitively; it is not the session count. `billed_amount` sums each
session's `final_amount`; `collected_amount` and the backwards-compatible
`revenue` field sum recorded `amount_paid`; `outstanding` sums
`max(final_amount - amount_paid, 0)`. The report also exposes cash/online
collections, gaming-versus-snack collected revenue, snack sales, and
unpaid/partially-paid/paid session counts. For combined bills, collected gaming
revenue is allocated first and any remaining collected amount is allocated to
snacks; the immutable payment transactions remain separate. PC totals expose the
same billed, collected, gaming, snack, cash, online, and outstanding distinctions.
Repeated synchronization upserts the
same `(device_id, session_id)` and does not add another session to totals.

The report is incomplete if any currently registered PC is offline, reports an
error, or has pending records. The device table reports its last successful
synchronization timestamp; previously synchronized history remains visible while
a report is incomplete. Historical records remain in the hub workbook after a
PC is removed from the current registry.

The peer APIs are unauthenticated and unencrypted. Use only on a trusted private
network and do not expose the management ports to the public internet.
