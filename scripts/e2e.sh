#!/usr/bin/env bash
# End-to-end acceptance test: builds the provider and a fake Talaria iac server, then drives a
# real TypeScript Pulumi program through up / no-op preview / update / replace / drift
# (refresh) / import / destroy against a local file backend.
# Real mode: set TALARIA_URL and TALARIA_API_KEY (a super-admin-capable key, see the Talaria iac
# README) to run against a real Talaria instead; optional TALARIA_ADMIN_EMAIL/TALARIA_ADMIN_PASSWORD
# make the drift step go through Talaria's own UI API (PUT /api/auth/roles/acl); TALARIA_ORG_NAME, if
# set, is the expected organization name (a tenant-wide key has none). The real server must have an
# `employee` role and no `ci` key; the run rewrites the employee ACL and clears it at the end.
# Needs: go, pulumi, node+npm, curl, jq, network access for `npm install`.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
PORT=${PORT:-$((20000 + RANDOM % 20000))}
REAL=${TALARIA_URL:+1}
FAKE_URL=${TALARIA_URL:-http://127.0.0.1:$PORT}
API_KEY=${TALARIA_API_KEY:-e2e-api-key}
FAKE_PID=
if [[ -n "$REAL" ]]; then F1=dashboards.view F2=analytics.view ORG_EXPECT=${TALARIA_ORG_NAME:+organizationName: \"$TALARIA_ORG_NAME\"}; KEY_RE='^[0-9a-f]{8}-[0-9a-f]{4}-'
else F1=customers.view F2=sales.view ORG_EXPECT='organizationName: "Fake Org"' KEY_RE='^[0-9a-f]{8}-0000-'; fi

cleanup() {
  [[ -n "$FAKE_PID" ]] && kill "$FAKE_PID" 2>/dev/null || true
  [[ -z "${KEEP:-}" ]] && rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n=== %s\n' "$*"; }
ok() { printf 'PASS: %s\n' "$*"; }
die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
# pl runs pulumi non-interactively in the project and stores its output in $OUT.
pl() {
  OUT=$(pulumi --non-interactive --color never "$@" 2>&1) || { printf '%s\n' "$OUT"; die "pulumi $* failed"; }
  printf '%s\n' "$OUT"
}
expect() { # expect <description> <substring>: $OUT must contain it
  [[ "$OUT" == *"$2"* ]] || die "$1: expected output to contain '$2'"
  ok "$1"
}
fake() { curl -fsS -H "Authorization: ApiKey $API_KEY" "$FAKE_URL/api/iac/$1"; }
fake_state() { # fake_state <kind> <key>: server-side state as JSON
  if [[ -n "$REAL" ]]; then fake "resources/$1?key=$2" | jq -c ".items[\"$2\"]"; else curl -fsS "$FAKE_URL/_fake/$1/$2"; fi
}
drift_acl() { # change the employee ACL behind Pulumi's back
  if [[ -z "$REAL" ]]; then curl -fsS -X PATCH -d '{"features":["rogue.feature"]}' "$FAKE_URL/_fake/auth.role_acl/employee" >/dev/null; return; fi
  if [[ -n "${TALARIA_ADMIN_EMAIL:-}" ]]; then
    local jar="$WORK/cj" rid
    curl -fsS -c "$jar" -d "email=$TALARIA_ADMIN_EMAIL&password=${TALARIA_ADMIN_PASSWORD:?}" "$FAKE_URL/api/auth/login" >/dev/null
    rid=$(curl -fsS -b "$jar" "$FAKE_URL/api/auth/roles?search=employee" | jq -r '.items[] | select(.name=="employee") | .id')
    curl -fsS -b "$jar" -X PUT -H 'content-type: application/json' \
      -d "{\"roleId\":\"$rid\",\"isSuperAdmin\":false,\"features\":[\"rogue.feature\"],\"organizations\":null}" "$FAKE_URL/api/auth/roles/acl" >/dev/null
  else
    curl -fsS -X PUT -H "Authorization: ApiKey $API_KEY" -H 'content-type: application/json' \
      -d '{"role":"employee","features":["rogue.feature"],"isSuperAdmin":false,"organizations":null}' "$FAKE_URL/api/iac/resources/auth.role_acl/employee" >/dev/null
  fi
}

step "build provider and fake server"
cd "$ROOT"
mkdir -p "$WORK/bin"
go build -ldflags "-X main.version=0.0.0-e2e" -o "$WORK/bin/pulumi-resource-talaria" ./cmd/pulumi-resource-talaria
[[ -n "$REAL" ]] || go build -o "$WORK/bin/fakeserver" ./cmd/fakeserver
# Isolated plugin cache: install the freshly built binary the way `pulumi plugin install --file` would.
export PULUMI_HOME="$WORK/home" PULUMI_SKIP_UPDATE_CHECK=true
pulumi plugin install resource talaria 0.0.0-e2e --file "$WORK/bin/pulumi-resource-talaria"

step "server on $FAKE_URL ($([[ -n "$REAL" ]] && echo real Talaria || echo fake))"
if [[ -z "$REAL" ]]; then
  "$WORK/bin/fakeserver" -addr "127.0.0.1:$PORT" -api-key "$API_KEY" >"$WORK/fake.log" 2>&1 &
  FAKE_PID=$!
  for _ in $(seq 50); do curl -fs "$FAKE_URL/_fake/auth.role_acl/admin" >/dev/null 2>&1 && break; sleep 0.1; done
fi
fake kinds >"$WORK/kinds.json"
[[ $(jq '.kinds | length' "$WORK/kinds.json") == 3 ]] || die "kinds document should hold 3 kinds"
ok "fake serves the kinds document"

step "pulumi project + SDK via 'pulumi package add'"
export PULUMI_BACKEND_URL="file://$WORK/state" PULUMI_CONFIG_PASSPHRASE=x
mkdir -p "$WORK/state" "$WORK/project"
cp "$ROOT"/scripts/e2e/{Pulumi.yaml,package.json,tsconfig.json,index.ts} "$WORK/project/"
cd "$WORK/project"
pl stack init dev
pl package add "$WORK/bin/pulumi-resource-talaria" "$WORK/kinds.json"
expect "SDK generated for talaria" "Added package talaria"
pl config set talaria:url "$FAKE_URL"
pl config set --secret talaria:apiKey "$API_KEY"
pl config set --path e2e:features "[\"$F1\"]"
pl config set --path e2e:roles '["admin"]'
pl config set e2e:description 'CI key'

step "pulumi up: create RoleAcl + ApiKey, read getScope"
pl up --yes --skip-preview
expect "created resources" "3 created" # stack + 2 talaria resources
expect "getScope invoked through the provider" "${ORG_EXPECT:-tenantName}"
[[ $(fake_state auth.role_acl employee | jq -c .features) == "[\"$F1\"]" ]] || die "server did not receive the ACL"
ok "ACL reached the server"
KEY_ID_1=$(pulumi stack output keyId)
[[ "$KEY_ID_1" =~ $KEY_RE ]] || die "keyId output should be the protocol's id field, got '$KEY_ID_1'"
ok "protocol field 'id' is exposed as apiKeyId ($KEY_ID_1)"
[[ $(pulumi stack output --json | jq -r .secret) == "[secret]" ]] || die "secret output must be a secret"
ok "api key secret is a Pulumi secret"
SECRET_1=$(pulumi stack output --show-secrets secret)
[[ "$SECRET_1" == omk_* ]] || die "secret should hold the creation-time secret"

step "pulumi preview: no changes"
pl preview --expect-no-changes
expect "no diff" "Resources:"
ok "preview is clean"

step "update: change a RoleAcl field -> in-place update"
pl config set --path e2e:features "[\"$F1\",\"$F2\"]"
pl up --yes --skip-preview
expect "ACL updated in place" "1 updated"
[[ $(fake_state auth.role_acl employee | jq -c .features) == "[\"$F1\",\"$F2\"]" ]] || die "update did not reach the server"
ok "update reached the server"

step "update: ApiKey description changes in place and keeps the creation-time secret"
pl config set e2e:description 'CI key v2'
pl up --yes --skip-preview
expect "key updated in place" "1 updated"
[[ $(pulumi stack output keyId) == "$KEY_ID_1" ]] || die "key must not be replaced for a description change"
[[ $(pulumi stack output --show-secrets secret) == "$SECRET_1" ]] || die "secret lost on update (server omits it)"
ok "update kept secret and id"

step "replace: change ApiKey roles -> delete + create"
pl config set --path e2e:roles '["admin","employee"]'
pl preview
expect "preview announces a replacement" "replace"
pl up --yes --skip-preview
expect "key replaced" "1 replaced"
KEY_ID_2=$(pulumi stack output keyId)
[[ "$KEY_ID_2" != "$KEY_ID_1" ]] || die "replace should have produced a new key id"
[[ $(pulumi stack output --show-secrets secret) != "$SECRET_1" ]] || die "replace should have produced a new secret"
ok "key replaced ($KEY_ID_1 -> $KEY_ID_2)"
if [[ -n "$REAL" ]]; then # the key minted through Pulumi must authenticate against the server
  curl -fsS -o /dev/null -H "Authorization: ApiKey $(pulumi stack output --show-secrets secret)" "$FAKE_URL/api/iac/kinds" || die "minted key does not authenticate"
  ok "key minted through Pulumi authenticates against /api/iac/kinds"
fi

step "drift: mutate the server, then pulumi refresh"
drift_acl
pl refresh --yes --diff
expect "refresh reports the ACL changed" "1 updated"
expect "refresh shows the drifted field" "rogue.feature"
[[ $(pulumi stack output --show-secrets secret) != "" ]] || die "refresh dropped the secret (server omits it on get)"
ok "refresh kept the secret the server no longer returns"
pl up --yes --skip-preview
[[ $(fake_state auth.role_acl employee | jq -c .features) == "[\"$F1\",\"$F2\"]" ]] || die "up did not repair drift"
ok "up repaired the drift"

step "import: drop the ACL from state, import it back from the server"
URN=$(pulumi stack export | jq -r '.deployment.resources[] | select(.type == "talaria:auth:RoleAcl") | .urn')
[[ "$URN" == urn:pulumi:* ]] || die "could not find the ACL urn (got '$URN')"
pl state delete --yes "$URN"
pl import --yes --protect=false --generate-code=false talaria:auth:RoleAcl employee-acl employee
expect "ACL imported" "1 imported"
pl preview --expect-no-changes
ok "imported ACL has no diff against the program"

step "destroy"
pl destroy --yes --skip-preview
expect "everything destroyed" "3 deleted"
[[ $(fake "resources/api_keys.api_key?key=ci" | jq -c .items.ci) == null ]] || die "api key still on the server"
[[ $(fake_state auth.role_acl employee | jq -c .features) == '[]' ]] || die "ACL not cleared on the server"
ok "server is clean (key gone, ACL cleared)"

step "ALL E2E STEPS PASSED"
