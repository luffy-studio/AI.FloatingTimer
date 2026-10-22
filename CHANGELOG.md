x# Changelog

## 2026-10-11 14:46 +05:30

- Added compact +15 min, +30 min, and +1 hr timer extension buttons to each dashboard timer card. Each uses the existing per-system tweak action and preserves the timer's running or paused state.

## 2026-10-14 12:44 +05:30 - Persistent Billing and Dashboard Foundation (In Progress)

- **Files modified:** `billing.go`, `pricing.go`, `main.go`, `ui.go`, `ui.html`, `go.mod`, `go.sum`, `CHANGELOG.md`.
- **Reason:** Introduce an isolated, central-admin SQLite foundation for session history and billing, plus the initial dashboard structure needed to present it without changing the established timer system.
- **Implementation:**
  - Added centralized Paradise Gaming pricing data for PC Pro, PC Lite, PS5, Racing Lite, and Racing Pro, including player-count and available-duration prices.
  - Added an optional SQLite billing store with schema versioning, session and session-event records, parameterized queries, transactional session/payment operations, history queries, daily payment summary, and consistent backup support.
  - Added optional `billing` configuration to the existing application configuration. Billing remains disabled by default; when enabled, its database location is stable and configurable on the admin PC.
  - Registered new, separate billing and session API routes on the existing management server. Existing timer, discovery, overlay, and client-communication routes were not changed.
  - Added the initial light-themed dashboard markup and styles for Paradise Gaming branding, system metrics, New Session, Current Bill, Price List, Session History, and a disabled-billing notice. The related browser-side billing workflow is not complete yet.
  - Added `github.com/mattn/go-sqlite3 v1.14.52` as the SQLite driver. The project environment reports `CGO_ENABLED=1` with GCC available.
- **Database schema/migrations:** Initial schema version `1` creates `sessions` and `session_events` tables with supporting indexes. Historical pricing and billing snapshots are stored per session so future price edits do not alter recorded amounts.
- **Existing behavior preserved:** Existing timer algorithms, timer endpoints and request formats, timeout overlay behavior, PC discovery, peer synchronization, distribution, saved PC records, ports, and client communication protocol remain unchanged. Billing is opt-in and separate from the timer lifecycle until the dashboard integration is completed.
- **Baseline tests performed:**
  - `node --test ui_polling_test.cjs`: passed, 23 of 23 tests.
  - `go build -o OverlayTimer.test.exe .`: failed before implementation verification because Git/VCS status discovery exited with status 128. A `-buildvcs=false` build is pending.
  - `go test -buildvcs=false -v ./cmd/launcher`: failed due to pre-existing Windows sandbox file-rename access-denied errors during launcher update tests.
  - `go test ./...`, `go test -buildvcs=false -v .`, and `go build -buildvcs=false ...`: started but not completed; no pass result is claimed.
- **Status:** In progress. JavaScript billing integration, focused billing tests, complete post-change build/test verification, backup documentation, and final operational validation remain outstanding.

## 2026-10-14 13:02 +05:30 - Excel Billing Storage Conversion (In Progress)

- **Files modified:** `billing.go`, `go.mod`, `go.sum`, `CHANGELOG.md`.
- **Reason:** Replace the unfinished SQLite billing persistence layer to meet the Excel-only billing storage requirement.
- **Changes:** Added `github.com/xuri/excelize/v2 v2.11.0`; replaced SQLite-specific persistence with an admin-PC `billing.xlsx` workbook containing `Sessions`, `Payments`, and `Daily Summary` sheets; added workbook validation, in-process write serialization, pre-write backups, temporary save/replacement, session validation, duplicate open-session prevention, searchable session retrieval, protected workbook download, and optional billing bearer-token authorization.
- **Existing functionality preserved:** Timer algorithms, timer endpoints, PC discovery, timeout overlay, existing PC records, ports, and client timer protocol were not changed.
- **Tests:** `gofmt -w billing.go` completed. `go test -buildvcs=false .` passed (`ok overlay-timer 0.583s`).
- **Known limitations:** Browser-side billing controls and complete payment, time-extension, close-session, date-filter, and daily-summary mutation flows remain under implementation. The API is unauthenticated, including billing routes; restrict access to trusted networks or use an authenticated HTTPS proxy.
- **Status:** In progress.

## 2026-10-14 13:18 +05:30 - Unified Excel Billing Dashboard

- **Files modified:** `billing.go`, `ui.html`, `CHANGELOG.md`.
- **Reason:** Connect the existing dashboard billing panels to persisted Excel session history without changing timer or discovery behavior.
- **Changes:** Added persisted session extension, payment, and close operations; `Payments` rows; regenerated `Daily Summary` data; protected session-action routes; price loading, billing-session creation, history display/search/payment filter, today-collected header metric, and paid/close history actions in the embedded dashboard.
- **Existing functionality preserved:** Existing timer controls and their routes, polling behavior, discovery, PC storage, timeout overlay, and network protocol are unchanged. Billing operations use separate Excel-backed endpoints.
- **Tests:** `gofmt -w billing.go` completed; `go test -buildvcs=false .` passed (`ok overlay-timer 0.616s`); `node --test ui_polling_test.cjs` passed (23 of 23).
- **Known limitations:** The visible Excel export button cannot attach the bearer-token header when a remote billing token is configured; download therefore works from the admin machine by default. Per-card active customer/bill summaries, date/system filters, and direct Add Time in the active-bill panel remain incomplete.
- **Status:** In progress.

## 2026-10-14 13:31 +05:30 - Audit Log Location

- New task-level entries are now maintained in `CHANGE.md` as requested. Existing `CHANGELOG.md` history is preserved unchanged.
