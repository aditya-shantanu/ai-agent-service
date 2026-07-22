#!/usr/bin/env bash
# Interactive Hermes platform console: pick a target (local kind / GKE),
# then loop through a menu — create/list/inspect/suspend/resume/delete
# agents, chat with one, open its dashboard, run the test suites, deploy,
# load provider keys. Each action runs the underlying make target /
# gateway API call / kubectl for you, then returns to the menu.
#
#   scripts/console.sh
#
# Works on stock macOS bash 3.2. Tokens created/rotated in this session are
# remembered (in-memory only) so chat/dashboard "just work" for them.
set -u

NS="${NS:-hermes-users}"
KIND_CTX="${KIND_CTX:-kind-hermes-svc}"
# GKE context is derived from your project/zone/cluster (kubectl's
# gke_<project>_<zone>_<cluster> convention) or set GKE_CTX directly.
GKE_CTX="${GKE_CTX:-gke_${GCP_PROJECT:-}_${GKE_ZONE:-us-central1-a}_${GKE_CLUSTER:-hermes-svc}}"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
CURL="curl -s -m 150"

CTX="" TARGET_NAME="" GW="" PF_PID="" ADMIN="" LB_URL=""
TOKEN_FILE="$(mktemp)"     # "user token" lines, this session only
trap 'kill $PF_PID 2>/dev/null; rm -f "$TOKEN_FILE"' EXIT

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
err()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }
ask()  { local v; read -r -p "$1" v; echo "$v"; }

k() { kubectl --context "$CTX" -n "$NS" "$@"; }

remember_token() { echo "$1 $2" >> "$TOKEN_FILE"; }
token_of() { awk -v u="$1" '$1==u {t=$2} END {print t}' "$TOKEN_FILE"; }

# ---------------------------------------------------------------- target ---
connect_gateway() {
  kill "$PF_PID" 2>/dev/null; PF_PID=""
  if ! k get deploy hermes-gateway >/dev/null 2>&1; then
    err "hermes-gateway not deployed in $CTX/$NS — use 'deploy' from the menu."
    ADMIN=""; GW=""; return 0
  fi
  ADMIN=$(k get secret hermes-gateway-admin -o jsonpath='{.data.admin-token}' | base64 -d)
  local port
  port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()')
  k port-forward svc/hermes-gateway "$port":8080 >/dev/null 2>&1 &
  PF_PID=$!
  sleep 3
  GW="localhost:$port"
  local lb_ip lb_port
  lb_ip=$(k get svc hermes-gateway -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
  lb_port=$(k get svc hermes-gateway -o jsonpath='{.spec.ports[0].port}' 2>/dev/null)
  LB_URL=""; [ -n "$lb_ip" ] && LB_URL="http://$lb_ip:$lb_port"
  return 0
}

choose_target() {
  echo "Targets:"
  kubectl config get-contexts -o name 2>/dev/null | grep -qx "$KIND_CTX" && echo "  1) local — kind ($KIND_CTX)"
  kubectl config get-contexts -o name 2>/dev/null | grep -qx "$GKE_CTX"  && echo "  2) gke   — production ($GKE_CTX)"
  echo "  3) other — enter a kubectl context name"
  case "$(ask 'Choice [1/2/3]: ')" in
    1) CTX="$KIND_CTX"; TARGET_NAME="local (kind)" ;;
    2) CTX="$GKE_CTX";  TARGET_NAME="GKE" ;;
    3) CTX="$(ask 'context: ')"; TARGET_NAME="$CTX" ;;
    *) err "invalid choice"; return 1 ;;
  esac
  kubectl config get-contexts -o name | grep -qx "$CTX" || { err "context '$CTX' not found"; CTX=""; return 1; }
  connect_gateway
}

need_gw() { [ -n "$GW" ] && [ -n "$ADMIN" ] || { err "no gateway connection (deploy first, or switch target)"; return 1; }; }
A() { echo "Authorization: Bearer $ADMIN"; }

# --------------------------------------------------------------- actions ---
do_create() {
  need_gw || return
  local count uid resp code body token sb state
  count=$(ask 'How many agents? [1]: '); count="${count:-1}"
  [[ "$count" =~ ^[0-9]+$ ]] || { err "not a number"; return; }
  local dash_user dash_pass
  dash_user=$(k get secret hermes-platform-secrets -o jsonpath='{.data.dashboard-username}' | base64 -d)
  dash_pass=$(k get secret hermes-platform-secrets -o jsonpath='{.data.dashboard-password}' | base64 -d)
  for i in $(seq 1 "$count"); do
    uid=$(ask "  user id #$i (e.g. alice): ")
    [[ "$uid" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]] || { err "  invalid id (lowercase dns-1123, max 40)"; continue; }
    resp=$($CURL -w '\n%{http_code}' -H "$(A)" -X POST "http://$GW/api/v1/users" -d "{\"userId\":\"$uid\"}")
    code=$(echo "$resp" | tail -1); body=$(echo "$resp" | sed '$d')
    case "$code" in
      201)
        token=$(echo "$body" | jq -r .token); sb=$(echo "$body" | jq -r .sandboxName)
        remember_token "$uid" "$token"
        echo "  created: $uid (sandbox $sb)"
        echo "    token (SAVE IT — shown once): $token"
        echo "    dashboard: http://$GW/u/$uid/?token=$token"
        [ -n "$LB_URL" ] && echo "    public:    $LB_URL/u/$uid/?token=$token"
        ;;
      200)
        echo "  $uid already exists — token not re-shown."
        case "$(ask '    rotate its token? [y/N]: ')" in
          y|Y) token=$($CURL -H "$(A)" -X POST "http://$GW/api/v1/users/$uid/token" | jq -r .token)
               remember_token "$uid" "$token"
               echo "    new token: $token" ;;
        esac ;;
      *) err "  create failed ($code): $body" ;;
    esac
  done
  echo "  dashboard login (all agents): $dash_user / $dash_pass"
}

do_list() {
  need_gw || return
  $CURL -H "$(A)" "http://$GW/api/v1/users" \
    | jq -r '.users // . | if length==0 then "  (no agents)" else
        (["USER","STATE","SANDBOX","EXEMPT"], (.[] | [.userId, .state, .sandboxName, (.suspendExempt|tostring)])) | @tsv end' \
    | column -t | sed 's/^/  /'
}

do_details() {
  need_gw || return
  local uid; uid=$(ask 'user id: ')
  $CURL -H "$(A)" "http://$GW/api/v1/users/$uid" | jq .
}

do_suspend() {
  need_gw || return
  local uid; uid=$(ask 'user id to suspend: ')
  $CURL -H "$(A)" -X POST "http://$GW/api/v1/users/$uid/suspend" | jq -c .
}

do_resume() {
  need_gw || return
  local uid; uid=$(ask 'user id to resume: ')
  echo "  resuming (cold resume can take ~20-24s on GKE)..."
  $CURL -H "$(A)" -X POST "http://$GW/api/v1/users/$uid/resume" | jq -c .
}

do_rotate() {
  need_gw || return
  local uid tok; uid=$(ask 'user id: ')
  tok=$($CURL -H "$(A)" -X POST "http://$GW/api/v1/users/$uid/token" | jq -r '.token // empty')
  if [ -n "$tok" ]; then remember_token "$uid" "$tok"; echo "  new token: $tok"; else err "  rotate failed"; fi
}

get_user_token() {  # $1=uid -> echoes token or empty
  local tok; tok=$(token_of "$1")
  if [ -z "$tok" ]; then
    echo "  (no token from this session for '$1')" >&2
    tok=$(ask '  paste token (or press enter to rotate): ')
    if [ -z "$tok" ]; then
      tok=$($CURL -H "$(A)" -X POST "http://$GW/api/v1/users/$1/token" | jq -r '.token // empty')
      [ -n "$tok" ] && { remember_token "$1" "$tok"; echo "  rotated; new token: $tok" >&2; }
    fi
  fi
  echo "$tok"
}

do_chat() {
  need_gw || return
  local uid tok msg reply
  uid=$(ask 'user id: ')
  tok=$(get_user_token "$uid"); [ -n "$tok" ] || { err "no token"; return; }
  echo "  chatting with '$uid' — empty message returns to menu. First reply after idle pays the cold wake."
  while true; do
    msg=$(ask "  you> ")
    [ -n "$msg" ] || break
    reply=$($CURL -H "Authorization: Bearer $tok" "http://$GW/u/$uid/v1/chat/completions" \
      -d "$(jq -nc --arg m "$msg" '{model:"hermes-agent",messages:[{role:"user",content:$m}]}')" \
      | jq -r '.choices[0].message.content // ("ERROR: " + (.|tostring))')
    printf '  %s> %s\n' "$uid" "$reply"
  done
}

do_dashboard() {
  need_gw || return
  local uid tok url
  uid=$(ask 'user id: ')
  tok=$(get_user_token "$uid"); [ -n "$tok" ] || { err "no token"; return; }
  url="http://$GW/u/$uid/?token=$tok"
  [ -n "$LB_URL" ] && url="$LB_URL/u/$uid/?token=$tok"
  echo "  opening $url"
  echo "  login: $(k get secret hermes-platform-secrets -o jsonpath='{.data.dashboard-username}' | base64 -d) / $(k get secret hermes-platform-secrets -o jsonpath='{.data.dashboard-password}' | base64 -d)"
  open "$url" 2>/dev/null || echo "  (open the URL manually)"
}

do_telegram() {
  need_gw || return
  local uid; uid=$(ask 'user id: ')
  case "$(ask 'install or remove bot token? [i/r]: ')" in
    i) local bt au
       bt=$(ask '  bot token (from @BotFather): ')
       au=$(ask '  allowed telegram user ids (comma-sep, optional): ')
       $CURL -H "$(A)" -X PUT "http://$GW/api/v1/users/$uid/telegram-token" \
         -d "$(jq -nc --arg t "$bt" --arg a "$au" '{token:$t} + (if $a != "" then {allowedUsers:$a} else {} end)')" | jq -c .
       echo "  note: this user is now suspend-exempt (bot stays online)." ;;
    r) $CURL -H "$(A)" -X DELETE "http://$GW/api/v1/users/$uid/telegram-token" | jq -c . ;;
    *) err "  i or r" ;;
  esac
}

do_delete() {
  need_gw || return
  local uid; uid=$(ask 'user id to DELETE: ')
  bold "  This is irreversible: deletes the agent, its sandbox AND its PVC (all memory/sessions)."
  case "$(ask "  type '$uid' to confirm: ")" in
    "$uid") $CURL -H "$(A)" -X DELETE "http://$GW/api/v1/users/$uid" -o /dev/null -w '  HTTP %{http_code}\n' ;;
    *) echo "  aborted" ;;
  esac
}

do_status() {
  say "pods"
  k get pods -o custom-columns='NAME:.metadata.name,RUNTIME:.spec.runtimeClassName,STATUS:.status.phase,NODE:.spec.nodeName' | sed 's/^/  /'
  say "sandboxes"
  k get sandboxes 2>/dev/null | sed 's/^/  /' || echo "  (none)"
  say "warm pool"
  k get sandboxwarmpool 2>/dev/null | sed 's/^/  /' || echo "  (none)"
}

run_make() {  # $1 = make target; switches kubectl current-context first
  echo "  (switching kubectl current-context to $CTX for this)"
  kubectl config use-context "$CTX" >/dev/null
  ( cd "$REPO_DIR" && make "$1" )
}

do_deploy() {
  if [ "$CTX" = "$KIND_CTX" ]; then
    bold "Running 'make dev' (kind cluster + helm deploy)..."
    run_make dev
  else
    bold "Running 'make deploy-gke' (terraform + images + ESO + helm — several minutes)..."
    case "$(ask 'proceed? [y/N]: ')" in y|Y) run_make deploy-gke ;; *) echo "  aborted"; return ;; esac
  fi
  connect_gateway
}

# ------------------------------------------------------------------ menu ---
choose_target || exit 1

while true; do
  echo
  bold "Hermes console — target: ${TARGET_NAME:-?} | ns: $NS | gateway: ${GW:-not connected}${LB_URL:+ | public: $LB_URL}"
  cat <<'EOF'
   1) create agent(s)         7) open dashboard in browser
   2) list agents             8) telegram bot token (install/remove)
   3) agent details           9) DELETE agent (irreversible)
   4) suspend agent          10) platform status (pods/sandboxes/pool)
   5) resume agent           11) run e2e suite (11 checks)
   6) chat with an agent     12) simulate concurrent users
  13) deploy / update platform    14) load LLM provider key (.env)
   t) switch target                q) quit
EOF
  case "$(ask 'console> ')" in
    1)  do_create ;;
    2)  do_list ;;
    3)  do_details ;;
    4)  do_suspend ;;
    5)  do_resume ;;
    6)  do_chat ;;
    7)  do_dashboard ;;
    8)  do_telegram ;;
    9)  do_delete ;;
    10) do_status ;;
    11) run_make e2e ;;
    12) run_make simulate-users ;;
    13) do_deploy ;;
    14) run_make set-provider-key ;;
    t)  choose_target || true ;;
    q)  exit 0 ;;
    *)  err "unknown choice" ;;
  esac
done
