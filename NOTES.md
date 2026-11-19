# Notes

## A suggestion I rejected

The starter's `POST /start` marked the sheet `success` and ticked every step inside the handler. When I asked Claude to make Start follow the README, it kept that shape: it added the `runId` and 409 checks but still finished the sheet in the handler. I rejected that. The README says the handler never sets `success` or `failed`, and the worker is what moves the checklist. Now Start only moves a draft to `running` with a new `runId` and every step `pending`. I checked this with `scripts/smoke.sh` against the compose stack. Start answers `running` with every step pending. After the worker marks the first step done, the script kills it, and the sheet stays `running` and unchanged. Once the worker is restarted, it finishes the sheet with the same `runId`.

## How it was checked

- `go test ./...` (no Postgres needed): API tests on an in-memory store, including the two rewritten tests, plus worker and brochure-rule tests. They pass with `-race`, on Go 1.23 (in `golang:1.23-alpine`) and on a newer local Go.
- `internal/store` tests run the real SQL against Postgres when `TEST_DATABASE_URL` is set and are skipped otherwise:

  ```bash
  TEST_DATABASE_URL='postgres://board:board@localhost:54329/board?sslmode=disable' go test ./internal/store/
  ```

- `scripts/smoke.sh` checks the running compose stack from outside: 64 checks over HTTP, with Docker used to look inside `/data` and to kill and restart the worker. It covers every rule in the README.
- The page was driven in headless Chrome. The checklist moved from ○○○ to ●●● and `success` with a single page load, and a sheet whose brochure was deleted showed `Brochure is missing`.
- I broke the worker on purpose: no brochure check, a pause after the last step, redoing a done step, not failing. Each mutation made a test fail.
- I also removed the migration lock. Concurrent startup migrations then failed every time with a duplicate `pg_type` key, which is why the lock is there.

## Decisions where the README is silent

| Case | Choice | Why |
| --- | --- | --- |
| `POST /start` on a `success` or `failed` sheet | 409, sheet unchanged | It can't start again, and "a failed sheet stays failed". |
| A draft with no brochure while another sheet is running | 400 `Attach a brochure first` | What is wrong with this sheet is the more useful answer. |
| A file of exactly 1 MiB | Accepted | The README rejects files *larger than* 1 MiB. |
| Brochure check in the worker | Before every step | A file that disappears mid-run fails the sheet at the next step. Steps already done stay done. |
| After the third step | Saved, then `success` right away, with no pause | The pause is before the *next* step, and there is none. |
| `filename` | The client's name, unchanged, even `../../escaped.txt` | The original is kept in the field and never used as a path. |

## How it works

- **Ownership.** Every sheet lookup takes the owner's id. Someone else's sheet is the same `ErrNotFound` as a missing one, so all four id routes answer 404 from one place.
- **Start.**
  - Start is one conditional `UPDATE … WHERE status = 'draft' AND filename <> ''`.
  - The partial unique index `jobs_one_running_per_user` gives the 409, so racing starts can't both win.
  - When a concurrent start got there first, the handler re-reads the sheet and answers from it. Parallel starts of one sheet all return the same `runId`.
- **Upload.**
  - The file part is read with a one-byte overshoot and the whole body is capped. An oversized file is never buffered or written.
  - The type is checked from the bytes.
  - The file is written to a temporary name and renamed to `body` only after the conditional `AttachFile` succeeds, so rejected or losing uploads leave nothing behind.
- **Worker.**
  - All state is in Postgres.
  - `SaveProgress` writes only while the sheet is still running with the same `runId` and the same checklist it read. A done step is never done again, even if two workers race.
  - A killed worker leaves the sheet `running`. The next process continues from the first pending step.
- **Page.** It polls the running sheet every second and stops at `success` or `failed`.

## Known limits

- If renaming the temp file fails after the sheet has claimed the upload, the sheet names a brochure that is not on disk. The worker then reports `Brochure is missing`, so the failure is visible rather than silent.
- Polling is simple and enough for one running sheet per account. Server-sent events would avoid it at the cost of more moving parts.
