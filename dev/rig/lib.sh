# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Sourced by every dev/rig script. Loads the machine's rig.env, which lives
# outside the repo, and sets up the state directory, which does too: the
# checkout holds the scripts and nothing a run produces or needs to keep
# secret. See dev/rig/README.md.

set -euo pipefail

rig_repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
rig_env="${SWITCHBOARD_RIG_ENV:-${XDG_CONFIG_HOME:-$HOME/.config}/switchboard-rig/rig.env}"

rig_die() {
  echo "rig: $*" >&2
  exit 2
}

if [[ ! -f $rig_env ]]; then
  cat >&2 <<EOF
rig: no config at $rig_env

  mkdir -p $(dirname "$rig_env") && chmod 700 $(dirname "$rig_env")
  cp $rig_repo/dev/rig/rig.env.example $rig_env

then fill it in. dev/rig/README.md walks through it.
EOF
  exit 2
fi
# shellcheck source=/dev/null
source "$rig_env"

RIG_STATE="${RIG_STATE:-${XDG_STATE_HOME:-$HOME/.local/state}/switchboard-rig}"
RIG_DAEMON_PORT="${RIG_DAEMON_PORT:-7777}"
RIG_CHAT_LISTEN_PORT="${RIG_CHAT_LISTEN_PORT:-8081}"

# The state directory holds the daemon token, the session database (real
# conversation text) and logs that carry message bodies, so nobody else on
# the machine reads it.
mkdir -p "$RIG_STATE/bin" "$RIG_STATE/logs"
chmod 700 "$RIG_STATE"

rig_daemon_url="http://127.0.0.1:$RIG_DAEMON_PORT"

# rig_bin NAME prints the path of a binary dev/rig/build put in the state
# directory, or stops with the command that would put it there.
rig_bin() {
  local p="$RIG_STATE/bin/$1"
  [[ -x $p ]] || rig_die "no $p yet: run dev/rig/build $1"
  printf '%s\n' "$p"
}

# rig_token exports the daemon bearer token both sides share. It is made
# once per machine and kept, so restarting the daemon or a gateway never
# leaves the two holding different values. It guards a loopback daemon
# with throwaway data, so it never needs to travel between machines.
rig_token() {
  local f="$RIG_STATE/daemon-token"
  if [[ ! -s $f ]]; then
    # Written aside and linked into place, which fails if another script
    # got there first: two starting at once still end up sharing one.
    local tmp="$f.$$"
    (umask 077 && printf 'tok_rig_%s\n' "$(od -An -tx1 -N18 /dev/urandom | tr -d ' \n')" >"$tmp")
    ln "$tmp" "$f" 2>/dev/null || true
    rm -f "$tmp"
  fi
  SWITCHBOARD_DAEMON_TOKEN="$(<"$f")"
  export SWITCHBOARD_DAEMON_TOKEN
}

# rig_secret_file VAR checks the file a rig.env variable names: it has to
# exist and has to be readable by its owner only. A token file anyone on
# the machine can read is refused rather than warned about.
rig_secret_file() {
  local var="$1" f="${!1:-}"
  [[ -n $f ]] || rig_die "$var is not set in $rig_env"
  [[ -f $f ]] || rig_die "$var names $f, which does not exist"
  local perms
  # -L: a symlink's own mode is always open; what matters is the target's.
  perms="$(ls -lL "$f" | cut -c5-10)"
  if [[ $perms != "------" ]]; then
    rig_die "$f is readable by others ($(ls -lL "$f" | cut -c1-10)): chmod 600 $f"
  fi
}

# rig_log NAME tees the rest of the script's output, and whatever it execs,
# to a fresh log under the state directory. tee ignores SIGINT (-i): Ctrl-C
# reaches the whole foreground group, and a tee that died with it would
# turn the binary's first shutdown log line into a SIGPIPE, cutting its
# graceful exit short.
rig_log() {
  local log
  log="$RIG_STATE/logs/$1-$(date +%Y%m%dT%H%M%S).log"
  echo "rig: logging to $log" >&2
  exec > >(tee -i -a "$log") 2>&1
}

# rig_procs lists the rig's running processes: those whose executable (the
# first word of the command line) is a binary in the state directory.
# Matching that path, not a name, keeps it off the wrapper shells, off
# anything else on the machine called switchboard, and off commands that
# merely mention the path (a build writing to it, a tail reading a log).
# -ww: procps otherwise cuts lines at $COLUMNS when it is set.
rig_procs() {
  ps -ww -eo pid=,args= | awk -v p="$RIG_STATE/bin/" 'index($2, p) == 1'
}

# rig_resolve_ref prints the commit a core-agent branch, tag or SHA names.
# An annotated tag's own line is the tag object; its ^{} line is the commit.
rig_resolve_ref() {
  local repo="$1" ref="$2"
  if [[ $ref =~ ^[0-9a-f]{40}$ ]]; then
    printf '%s\n' "$ref"
    return
  fi
  git ls-remote "$repo" "refs/heads/$ref" "refs/tags/$ref" "refs/tags/$ref^{}" |
    awk '$2 ~ /\^\{\}$/ {peeled = $1} {if (first == "") first = $1}
         END {print (peeled != "" ? peeled : first)}'
}
