# Approved Changes

## 2026-10-14 13:20 +05:30 — Restore the current billing dashboard

- Completed the billing dashboard wiring for the existing Excel-backed API: price list rendering, active-system current bill display and actions, and session-history filtering and columns.
- Enabled the existing billing feature in `overlay_timer_config.json`; the existing backend continues to store billing data in `billing.xlsx`.
- Moved the billing-enabled JSON configuration from the accidentally edited `ui_polling_test.cjs` into `overlay_timer_config.json` and restored the test file's JavaScript header.
- Rebuilt and restarted the embedded-UI application; runtime verification exposed a workbook-initialization failure, recorded below.

## 2026-10-14 13:24 +05:30 — Fix Excel workbook initialization

- Corrected the Excelize temporary workbook filename so it retains an `.xlsx` extension, allowing the enabled billing store to create its workbook.
- Approved follow-up verification against an isolated workbook to avoid adding test sessions to operational billing data.

## 2026-10-14 13:30 +05:30 — Verify billing UI and persistence

- `build.ps1` successfully rebuilt `OverlayTimer.exe` and `FloatingTimerLauncher.exe`.
- `go test -buildvcs=false .` passed; `node --test ui_polling_test.cjs` passed all 23 tests.
- Isolated API verification returned all 36 prices, created a PC Pro session at INR 20, re-read it from the workbook-backed session API, and persisted payment and close actions. The temporary test workbook and test session were removed afterward.
- The rebuilt app is running as PID 9808 on port 18082. A no-cache request returns the updated billing UI; pricing, summary, and session APIs respond successfully, billing is enabled, and the clean operational `billing.xlsx` was created beside the executable.

## 2026-10-14 13:59:51 — Connect session billing to system timers

- **Status:** Implementation in progress; verification pending.
- **Files modified:** `billing.go`, `ui.html`, `API.md`, `CHANGE.md`.
- **Reason:** Session Start previously wrote a billing row without starting the selected gaming system's timer; the workbook also exposed redundant columns and sheets.
- **Implementation details:** Started the exact eight-column workbook migration with row-comment metadata and legacy-payment preservation; wired session creation to the selected timer's existing status, set, and play endpoints, including rollback on failure; updated the UI to capture time server-side and prevent repeated starts; documented the API behavior. New sessions record the full rate-card amount as paid at start, as approved.
- **Tests performed and actual results:** No tests run yet. Editor diagnostics identified `excelize.NewSheet` return handling and a formatting suggestion; both were corrected before test execution.
- **Known issues or pending work:** Add focused migration/timer tests, verify workbook date/time formatting and rollback, run the Go/UI suites and `build.ps1`, then migrate and verify the operational workbook with a timestamped backup and restart the application.

## 2026-10-14 14:08:50 — Verify workbook migration and timer coordination

- **Status:** Automated tests passed; build and live-app verification pending.
- **Files modified:** `billing.go`, `billing_test.go`, `ui.html`.
- **Reason:** Verify legacy-data preservation, exact workbook layout, selected timer routing, duplicate prevention, and failure compensation before touching the operational workbook.
- **Implementation details:** Added isolated Excel migration and HTTP timer tests, including systems sharing one host with distinct `/1` paths. Validated both timer-command and workbook-save rollback paths.
- **Tests performed and actual results:** `go test -buildvcs=false ./...` passed for the main project and launcher; `node --test ui_polling_test.cjs` passed 23/23; Problems diagnostics reported no errors; `git diff --check` passed. A test initially exposed second-rounding in Excel display; time-only values now store whole-second precision, and the tests pass.
- **Known issues or pending work:** Build both executables, verify an isolated running application including expiry behavior, back up and migrate the operational workbook, restart the production executable, and commit approved source/documentation changes.

## 2026-10-14 14:20:14 — Build safely; defer production restart

- **Status:** Go/UI tests and isolated build passed; production runtime verification pending.
- **Files modified:** `CHANGE.md` only for this verification update.
- **Reason:** Validate the approved timer/billing changes without interrupting an open operational billing session.
- **Implementation details:** Ran the existing `build.ps1` in an isolated source copy, excluding the operational workbook and backups. Both executables were produced in the temporary copy, which was removed afterward. The production executable, process, and workbook were not changed.
- **Tests performed and actual results:** `go test -buildvcs=false ./...` passed for both Go packages; `node --test ui_polling_test.cjs` passed 23/23; isolated `build.ps1` built `OverlayTimer.exe` and `FloatingTimerLauncher.exe`; `git diff --check` passed.
- **Known issues or pending work:** The production manager still has one open billing session; its timer is paused at 0 seconds and the timeout overlay is hidden. Per the user's direction, defer the workbook migration, production restart, live API/UI check, and selected-system timer/expiry test until the open session is closed. No production workbook records were changed.

## 2026-10-14 14:21:53 — Remove the unused SQLite dependency

- **Status:** Excel-only dependency cleanup and repeat automated verification passed; production runtime verification remains pending.
- **Files modified:** `go.mod`, `go.sum`, `CHANGE.md`.
- **Reason:** The module manifest still listed the unused SQLite driver despite the approved Excel-only billing design.
- **Implementation details:** Ran `go mod tidy`; Excelize is a direct dependency and the unused SQLite driver/checksums are no longer present. Historical `CHANGELOG.md` entries were preserved.
- **Tests performed and actual results:** After cleanup, `go test -buildvcs=false ./...` passed for both Go packages; `node --test ui_polling_test.cjs` passed 23/23; isolated `build.ps1` built both executables; `git diff --check` passed; a source/manifest search found no SQLite references.
- **Known issues or pending work:** The open production billing session remains untouched. Production workbook migration, restart, live UI/API and timer-expiry verification are deferred until that session is closed. The source commit will not claim those runtime checks passed.

## 2026-10-14 13:33:00 - Restore browser regression test

- **Files modified:** `ui_polling_test.cjs`, `CHANGE.md`.
- **Reason:** Remove invalid JSON prepended to the JavaScript test file.
- **Implementation details:** Restored the `node --test` source header; no production timer, workbook, or overlay code changed.
- **Tests performed and actual results:** `node --test ui_polling_test.cjs` passed 23/23; `go test -buildvcs=false .` passed.
- **Known issues or pending work:** Native Fyne overlay expiry was not visually tested on a real gaming PC. Generated workbook artifacts are not committed.

## 2026-10-14 13:38:00 - Required Customer Name Validation

- **Files modified:** `billing.go`, `ui.html`, `CHANGE.md`.
- **Reason:** Prevent blank or whitespace-only customer names from starting timers or creating billing sessions.
- **Implementation details:** Frontend trims the field, shows the required message, focuses and highlights the input, and clears it only after confirmed success. Backend uses the same trim and message, preserving direct API protection before timer or workbook operations.
- **Tests performed and actual results:** `go test -buildvcs=false .` passed; `node --test ui_polling_test.cjs` passed 23/23; `git diff --check` passed.
- **Known issues or pending work:** The existing automated timer tests use controlled HTTP endpoints. A live native overlay visual check on a gaming PC remains outstanding.
