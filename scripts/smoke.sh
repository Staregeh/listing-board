#!/usr/bin/env bash
# Black-box check of the README against the running compose stack.
#
#   docker compose up -d --build
#   scripts/smoke.sh
#
# Talks to the API over HTTP only, and to Docker to look at /data and to
# kill and restart the worker. Needs curl and jq. Leaves the stack running.
# Uses fresh accounts each run, so it can be repeated.

set -uo pipefail
cd "$(dirname "$0")/.."

BASE=${BASE:-http://localhost:8080}
API=$BASE/api
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
RUN=$(date +%s)$RANDOM
PASS=0
FAIL=0

ok()   { PASS=$((PASS + 1)); printf '  \033[32mok\033[0m    %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m  %s\n' "$1"; [ -n "${2:-}" ] && printf '        %s\n' "$2"; }
step() { printf '\n%s\n' "$1"; }
check() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "got: $2 | want: $3"; fi; }

# req JAR METHOD PATH [curl args...] → prints the status code; body in $TMP/body
req() {
  local jar=$1 method=$2 path=$3
  shift 3
  curl -s -o "$TMP/body" -w '%{http_code}' -b "$TMP/$jar" -c "$TMP/$jar" -X "$method" "$API$path" "$@"
}
body()  { cat "$TMP/body"; }
field() { jq -r "$1" "$TMP/body"; }
sheet() { req "$1" GET "/jobs/$2" >/dev/null; jq -c "$3" "$TMP/body"; }

register() {
  req "$1" POST /register -H 'Content-Type: application/json' \
    -d "{\"email\":\"$1-$RUN@example.com\",\"password\":\"password123\"}"
}
create() {
  req "$1" POST /jobs -H 'Content-Type: application/json' -d "{\"title\":\"$2\",\"address\":\"10 Market Street\"}" >/dev/null
  field .id
}
# upload JAR ID FILE NAME [TYPE] → status
upload() { req "$1" POST "/jobs/$2/file" -F "file=@$3;filename=$4;type=${5:-application/octet-stream}"; }
# wait_for JAR ID JQ-EXPR SECONDS → waits until the expression is true
wait_for() {
  local end=$((SECONDS + $4))
  while [ $SECONDS -lt $end ]; do
    [ "$(sheet "$1" "$2" "$3")" = "true" ] && return 0
    sleep 0.2
  done
  return 1
}
in_api() { docker compose exec -T api sh -c "$1"; }

for i in $(seq 1 30); do curl -sf "$API/health" >/dev/null && break; sleep 1; done

step "Health and accounts"
check "GET /health is 200 ok" "$(curl -s -w ' %{http_code}' "$API/health")" "ok 200"
check "register is 201" "$(register ada)" 201
ADA_ID=$(field .id)
check "register body is {id, email}" "$(jq -c 'keys' "$TMP/body")" '["email","id"]'
check "register sets lb_session" "$(grep -c lb_session "$TMP/ada")" 1
check "GET /me is 200" "$(req ada GET /me)" 200
register grace >/dev/null
check "login is 200" "$(req ada2 POST /login -H 'Content-Type: application/json' -d "{\"email\":\"ada-$RUN@example.com\",\"password\":\"password123\"}")" 200
check "logout is 200" "$(req ada2 POST /logout)" 200
check "GET /me after logout is 401" "$(req ada2 GET /me)" 401
check "GET /jobs without a session is 401" "$(req nobody GET /jobs)" 401
check "401 body is JSON {error}" "$(field '.error | type')" string

step "Creating a sheet"
check "POST /jobs is 201" "$(req ada POST /jobs -H 'Content-Type: application/json' -d '{"title":"Market loft","address":"10 Market Street"}')" 201
LOFT=$(field .id)
check "new sheet is a draft with no runId" "$(jq -c '[.status, .runId, .error, .filename]' "$TMP/body")" '["draft","","",""]'
check "checklist labels and states" "$(jq -c '[.checklist[] | .label + "/" + .state]' "$TMP/body")" \
  '["Reading the brochure/pending","Checking the address/pending","Publishing the sheet/pending"]'
check "start without a brochure is 400" "$(req ada POST "/jobs/$LOFT/start")" 400
check "... with the exact message" "$(field .error)" "Attach a brochure first"

step "Brochure rules"
: >"$TMP/empty"
printf 'Two\0bedrooms' >"$TMP/nul"
printf 'caf\xe9' >"$TMP/latin1"
printf '\x89PNG\r\n\x1a\n\0\0' >"$TMP/png"
head -c 1048576 /dev/zero | tr '\0' a >"$TMP/1mib"
head -c 1048577 /dev/zero | tr '\0' a >"$TMP/1mib+1"
printf '%%PDF-1.4\nMarket loft brochure\n' >"$TMP/brochure.pdf"
printf 'Two bedrooms, south facing\n' >"$TMP/notes"
for f in empty nul latin1 png 1mib+1; do
  ID=$(create ada "rejected $f")
  check "$f is 400" "$(upload ada "$ID" "$TMP/$f" brochure.pdf application/pdf)" 400
  check "... and the sheet has no filename" "$(sheet ada "$ID" .filename)" '""'
done
check "nothing rejected was left on disk" "$(in_api "find /data/$ADA_ID -type f 2>/dev/null | wc -l" | tr -d ' ')" 0
check "exactly 1 MiB is 200" "$(upload ada "$(create ada 'at the limit')" "$TMP/1mib" big.txt text/plain)" 200
check "PDF bytes named .txt are accepted" "$(upload ada "$(create ada 'pdf as txt')" "$TMP/brochure.pdf" notes.txt text/plain)" 200
check "text bytes named .pdf are accepted" "$(upload ada "$(create ada 'txt as pdf')" "$TMP/notes" brochure.pdf application/pdf)" 200
TRAV=$(create ada traversal)
check "../../escaped.txt is accepted" "$(upload ada "$TRAV" "$TMP/notes" ../../escaped.txt text/plain)" 200
check "... and kept only as the filename" "$(field .filename)" "../../escaped.txt"
check "no escaped.txt anywhere in the container" "$(in_api 'find / -xdev -name escaped.txt 2>/dev/null | wc -l' | tr -d ' ')" 0
check "every stored file is {DATA_DIR}/{userID}/{jobID}/body" \
  "$(in_api "find /data -type f | grep -cvE '^/data/[0-9a-f]+/[0-9a-f]+/body$' || true" | tr -d ' ')" 0
check "upload to the loft is 200" "$(upload ada "$LOFT" "$TMP/brochure.pdf" brochure.pdf application/pdf)" 200
check "the bytes are at /data/{userID}/{jobID}/body" "$(in_api "cat /data/$ADA_ID/$LOFT/body" | cmp -s - "$TMP/brochure.pdf" && echo same)" same
check "a second upload is 400" "$(upload ada "$LOFT" "$TMP/notes" second.txt text/plain)" 400
check "... and the first brochure stays" "$(sheet ada "$LOFT" .filename)" '"brochure.pdf"'
check "download is 200" "$(req ada GET "/jobs/$LOFT/file")" 200
check "... with the brochure bytes" "$(cmp -s "$TMP/body" "$TMP/brochure.pdf" && echo same)" same

step "Another account sees 404 everywhere"
check "GET sheet" "$(req grace GET "/jobs/$LOFT")" 404
check "upload" "$(upload grace "$LOFT" "$TMP/notes" x.txt)" 404
check "start" "$(req grace POST "/jobs/$LOFT/start")" 404
check "download" "$(req grace GET "/jobs/$LOFT/file")" 404
check "unknown id is 404 too" "$(req ada GET /jobs/does-not-exist)" 404
check "list hides it" "$(req grace GET /jobs >/dev/null; jq --arg id "$LOFT" '[.[] | select(.id == $id)] | length' "$TMP/body")" 0
check "nothing changed on the sheet" "$(sheet ada "$LOFT" '[.status, .filename]')" '["draft","brochure.pdf"]'

step "Start and the worker"
check "start is 200" "$(req ada POST "/jobs/$LOFT/start")" 200
RUN_ID=$(field .runId)
check "... running, a runId, every step pending" \
  "$(jq -c '[.status, (.runId | length > 0), [.checklist[].state]]' "$TMP/body")" '["running",true,["pending","pending","pending"]]'
check "start again is 200 with the same runId" "$(req ada POST "/jobs/$LOFT/start") $(field .runId)" "200 $RUN_ID"
OTHER=$(create ada other)
upload ada "$OTHER" "$TMP/notes" other.txt >/dev/null
check "starting another sheet while one runs is 409" "$(req ada POST "/jobs/$OTHER/start")" 409
check "... and it stays a draft" "$(sheet ada "$OTHER" .status)" '"draft"'
if wait_for ada "$LOFT" '.checklist[0].state == "done"' 5; then ok "worker marks the first step done"; else bad "worker marks the first step done"; fi
docker compose kill worker >/dev/null 2>&1
BEFORE=$(sheet ada "$LOFT" '[.status, .runId, [.checklist[].state]]')
sleep 3
check "killing the worker leaves the sheet running, unchanged" "$(sheet ada "$LOFT" '[.status, .runId, [.checklist[].state]]')" "$BEFORE"
docker compose up -d worker >/dev/null 2>&1
if wait_for ada "$LOFT" '.status == "success"' 15; then ok "restarted worker finishes the sheet"; else bad "restarted worker finishes the sheet" "$(sheet ada "$LOFT" .)"; fi
check "... with the same runId and every step done" "$(sheet ada "$LOFT" '[.runId, [.checklist[].state]]')" "[\"$RUN_ID\",[\"done\",\"done\",\"done\"]]"
check "a finished sheet can't be started again" "$(req ada POST "/jobs/$LOFT/start")" 409

step "Step timing (STEP_PAUSE 2s)"
TIMED=$(create ada timed)
upload ada "$TIMED" "$TMP/notes" t.txt >/dev/null
req ada POST "/jobs/$TIMED/start" >/dev/null
T0=$SECONDS
wait_for ada "$TIMED" '.status == "success"' 15
ELAPSED=$((SECONDS - T0))
if [ $ELAPSED -ge 3 ] && [ $ELAPSED -le 8 ]; then ok "three steps took ${ELAPSED}s (two pauses of 2s)"; else bad "three steps took ${ELAPSED}s, want about 4s"; fi

step "Missing brochure"
docker compose stop worker >/dev/null 2>&1
LOST=$(create ada lost)
upload ada "$LOST" "$TMP/notes" lost.txt >/dev/null
req ada POST "/jobs/$LOST/start" >/dev/null
in_api "rm /data/$ADA_ID/$LOST/body"
docker compose up -d worker >/dev/null 2>&1
if wait_for ada "$LOST" '.status == "failed"' 15; then ok "sheet fails"; else bad "sheet fails" "$(sheet ada "$LOST" .)"; fi
check "... with error exactly 'Brochure is missing'" "$(sheet ada "$LOST" .error)" '"Brochure is missing"'
check "start on a failed sheet is 409" "$(req ada POST "/jobs/$LOST/start")" 409
check "... and it stays failed" "$(sheet ada "$LOST" .status)" '"failed"'

step "Racing starts"
docker compose stop worker >/dev/null 2>&1
R1=$(create ada race1); upload ada "$R1" "$TMP/notes" r.txt >/dev/null
R2=$(create ada race2); upload ada "$R2" "$TMP/notes" r.txt >/dev/null
for i in $(seq 1 10); do
  ID=$R1; [ $((i % 2)) -eq 0 ] && ID=$R2
  curl -s -o /dev/null -w '%{http_code}\n' -b "$TMP/ada" -X POST "$API/jobs/$ID/start" >>"$TMP/race" &
done
wait
RUNNING=$(req ada GET /jobs >/dev/null; jq '[.[] | select(.status == "running")] | length' "$TMP/body")
check "10 parallel starts on two sheets leave exactly one running" "$RUNNING" 1
check "... and every response was 200 or 409" "$(grep -cvE '^(200|409)$' "$TMP/race")" 0
docker compose up -d worker >/dev/null 2>&1

step "Worker waits for the database"
docker compose stop worker postgres >/dev/null 2>&1
docker compose up -d --no-deps worker >/dev/null 2>&1
sleep 4
docker compose start postgres >/dev/null 2>&1
sleep 8
LOGS=$(docker compose logs --since 20s worker 2>&1)
if grep -q "database not ready" <<<"$LOGS" && grep -q "worker started" <<<"$LOGS"; then
  ok "worker retried until Postgres was up, then started"
else
  bad "worker retried until Postgres was up, then started" "$(tail -3 <<<"$LOGS")"
fi
for i in $(seq 1 30); do curl -sf "$API/health" >/dev/null && break; sleep 1; done

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
