#!/bin/sh
# nim: run nimroute in foreground, as a service, or show status.
# usage: nim [run|start|stop|restart|status [tail]|status-tail|models [N]|models sync|logs|log [N|tail]|tail|install|uninstall|freepi auth]
# extra args on run/start/restart/install go straight to the binary:
#   nim run -df                 foreground, dump failed requests to json
#   nim restart --dbg-file=/tmp/d   as a service, dumps in /tmp/d
PROXY_DIR="$(cd "$(dirname "$0")" && pwd)"
[ -n "$PROXY_DIR" ] || { echo "nim: cannot resolve the script directory" >&2; exit 1; }
LOGFILE="$PROXY_DIR/nimroute.log"
BINARY="$PROXY_DIR/nimroute"
SVC="com.user.nimroute"
PIDFILE="$PROXY_DIR/.nimroute.pid"
PLIST="$HOME/Library/LaunchAgents/$SVC.plist"
SYSTEMD="$HOME/.config/systemd/user/nimroute.service"
OS="$(uname -s 2>/dev/null)"

find_py() {
  for p in /opt/homebrew/bin/python3 /usr/local/bin/python3 python3; do
    if "$p" -c 'pass' >/dev/null 2>&1; then echo "$p"; return 0; fi
  done
  return 1
}

usage() {
  cat <<'EOF'
nim - run and inspect the nimroute proxy

usage: ./nim <command> [args]

  run [flags]        foreground; extra flags go to the binary
  start [flags]      start the background service
  stop               stop the service
  restart [flags]    stop, then start
  status [tail]      service state, then the dashboard
  status-tail        dashboard, refreshing in place
  logs               recent traffic
  log [N|tail]       last N lines, or follow
  tail               follow the access log
  models [N]         model list with benchmark scores
  models sync        refresh the model catalog
  freepi auth        github device login for the freepi backend
  install [flags]    install the service
  uninstall          remove the service
  help               print this

binary flags:  --config PATH  --port N  --lanes N  --debug
               -df  --dbg-file=DIR   dump failed requests as json
config:        config.yml, hot-reloaded. docs/service.md has the details.
EOF
}

# need_binary: every command that launches the proxy needs a build. without
# this the shell reports "no such file or directory", which reads like a
# broken install rather than an unbuilt one.
need_binary() {
  [ -x "$BINARY" ] && return 0
  echo "nim: no nimroute binary at $BINARY" >&2
  if command -v go >/dev/null 2>&1; then
    echo "     build it first:  go build -o nimroute ." >&2
  else
    echo "     go is not installed (needs Go 1.26+): https://go.dev/dl/" >&2
    echo "     macOS: brew install go   |   Linux: sudo apt install golang-go" >&2
  fi
  exit 1
}

need_py() {
  PY="$(find_py)" || {
    echo "nim: python3 not found, and '$1' needs it" >&2
    echo "     install python3 (macOS: brew install python3; debian: apt install python3)" >&2
    echo "     raw status is still available:  curl -s http://localhost:${PORT:-5419}/status" >&2
    exit 1
  }
}

case "$OS" in
  Darwin) SVC_CMD="launchd";;
  Linux)  command -v systemctl >/dev/null 2>&1 && SVC_CMD="systemd" || SVC_CMD="nohup";;
  *)      SVC_CMD="nohup";;
esac

svc_running() {
  case "$SVC_CMD" in
    launchd) launchctl list "$SVC" 2>/dev/null | grep '"PID"' | grep -qv '= 0' ;;
    systemd) systemctl --user is-active -q nimroute 2>/dev/null ;;
    nohup)   [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null ;;
  esac
}

# port_alive reports whether something is serving on the proxy port. catches
# instances started manually (outside the service manager) so status reads
# correctly and start does not double-bind. curl when present, else a python
# socket connect, else a warning (silently returning false let start
# double-bind).
port_alive() {
  if command -v curl >/dev/null 2>&1; then
    curl -sf -m 2 "http://localhost:${PORT:-5419}/status" >/dev/null 2>&1
    return $?
  fi
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ]; then
    "$PY" -c 'import socket,sys; s=socket.socket(); s.settimeout(2); sys.exit(0 if s.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)' "${PORT:-5419}"
    return $?
  fi
  echo "nim: neither curl nor python3 found, cannot probe port ${PORT:-5419}" >&2
  return 1
}

# svc_start passes any extra args through to the binary: launchd and
# systemd templates carry REPLACE_WITH_ARGS, so `nim restart -df` starts a
# recording service without editing the templates by hand.
svc_start() {
  command -v sed >/dev/null 2>&1 || { echo "nim: sed not found; needed to write the service file" >&2; exit 1; }
  # bsd sed wants \n for a replacement newline, and & is special there, so
  # each arg is xml-escaped then sed-escaped (the &amp; back into \&amp;).
  ARGS_XML=""; ARGS_SPC=""
  for a in "$@"; do
    esc=$(printf '%s' "$a" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/&/\\&/g')
    ARGS_XML="$ARGS_XML\\n    <string>$esc</string>"
    ARGS_SPC="$ARGS_SPC $a"
  done
  case "$SVC_CMD" in
    launchd)
      sed -e "s|REPLACE_WITH_PATH|$PROXY_DIR|g" -e "s|REPLACE_WITH_ARGS|$ARGS_XML|" "$PROXY_DIR/deploy/$SVC.plist" > "$PLIST"
      launchctl bootout "gui/$(id -u)/$SVC" 2>/dev/null
      launchctl bootstrap "gui/$(id -u)" "$PLIST" || launchctl load "$PLIST" ;;
    systemd)
      mkdir -p "$(dirname "$SYSTEMD")"
      sed -e "s|REPLACE_WITH_PATH|$PROXY_DIR|g" -e "s|REPLACE_WITH_ARGS$|$ARGS_SPC|" "$PROXY_DIR/deploy/nimroute.service" > "$SYSTEMD"
      systemctl --user daemon-reload && systemctl --user enable --now nimroute ;;
    nohup)
      cd "$PROXY_DIR" && nohup "$BINARY" "$@" >>"$LOGFILE" 2>&1 &
      (umask 077; echo $! > "$PIDFILE") ;;
  esac
}

svc_stop() {
  case "$SVC_CMD" in
    launchd)
      # bootout both labels: pre-rename instances still run as
      # com.user.nvidia-nim-proxy, current as com.user.nimroute.
      launchctl bootout "gui/$(id -u)/$SVC" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null
      launchctl bootout "gui/$(id -u)/com.user.nvidia-nim-proxy" 2>/dev/null
      ;;
    systemd) systemctl --user disable --now nimroute 2>/dev/null ;;
    nohup)
      if [ -f "$PIDFILE" ]; then
        pid="$(cat "$PIDFILE")"
        case "$pid" in ''|*[!0-9]*) ;;
          *) kill "$pid" 2>/dev/null ;;
        esac
        rm -f "$PIDFILE"
      fi ;;
  esac
}

pretty_status() {
  if svc_running || port_alive; then
    echo "nimroute: RUNNING ($SVC_CMD)"
  else
    echo "nimroute: STOPPED"
    [ -x "$BINARY" ] || echo "  not built yet:  go build -o nimroute ."
    return 1
  fi
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ]; then
    "$PY" "$PROXY_DIR/nimstatus.py" status 2>/dev/null || { echo ""; curl -s "http://localhost:${PORT:-5419}/status"; echo ""; }
  elif command -v curl >/dev/null 2>&1; then
    echo ""; curl -s "http://localhost:${PORT:-5419}/status"; echo ""
  fi
}

# status_tail refreshes in place (rich Live) instead of clearing.
status_tail() {
  PY="$(find_py)" || PY=""
  if [ -n "$PY" ]; then
    "$PY" "$PROXY_DIR/nimstatus.py" tail-status
  else
    trap 'exit 0' INT TERM
    while true; do
      clear 2>/dev/null || printf '\033c'
      pretty_status
      sleep 2
    done
  fi
}

case "${1:-status}" in
  run)     shift; need_binary; exec "$BINARY" "$@" ;;
  start)   shift; need_binary; if port_alive; then pretty_status; exit 0; fi; svc_start "$@"; sleep 2; pretty_status ;;
  stop)    svc_stop; echo "nimroute stopped" ;;
  restart) shift; need_binary; svc_stop; sleep 1; svc_start "$@"; sleep 2; pretty_status ;;
  status)  pretty_status; st=$?; if [ "${2:-}" = "tail" ]; then status_tail; else exit $st; fi ;;
  status-tail) status_tail ;;
  logs)    pretty_status; PY="$(find_py)" || PY=""; [ -n "$PY" ] && "$PY" "$PROXY_DIR/nimstatus.py" logs 10 || { [ -f "$LOGFILE" ] && { echo ""; echo "  Access log (last 10):"; tail -10 "$LOGFILE" | sed 's/^/    /'; }; } ;;
  log)
    case "$2" in
      ""|tail|f) if [ -f "$LOGFILE" ]; then tail -f "$LOGFILE"; else echo "no log yet ($LOGFILE)"; fi ;;
      [0-9]*) [ -f "$LOGFILE" ] && { echo ""; echo "  Access log (last $2):"; tail -n "$2" "$LOGFILE" | sed 's/^/    /'; } || echo "no log yet ($LOGFILE)" ;;
      *) echo "nim: 'log' takes a line count or 'tail', got '$2'" >&2; echo "     usage: ./nim log [N|tail]" >&2; exit 1 ;;
    esac ;;
  tail)    if [ -f "$LOGFILE" ]; then tail -f "$LOGFILE"; else echo "no log yet ($LOGFILE)"; fi ;;
  freepi)
    case "$2" in
      auth) need_py "freepi auth"; exec "$PY" "$PROXY_DIR/scripts/freepi_login.py" --into-config "$PROXY_DIR/config.yml" ;;
      "")   echo "nim: 'freepi' needs a subcommand" >&2; echo "     usage: ./nim freepi auth" >&2; exit 1 ;;
      *)    echo "nim: unknown freepi subcommand '$2'" >&2; echo "     usage: ./nim freepi auth" >&2; exit 1 ;;
    esac ;;
  models)
    case "$2" in
      "")       need_py "models"; exec "$PY" "$PROXY_DIR/nimstatus.py" models 0 ;;
      [0-9]*)   need_py "models"; exec "$PY" "$PROXY_DIR/nimstatus.py" models "$2" ;;
      sync)     need_binary; shift 2; exec "$BINARY" models sync --config "$PROXY_DIR/config.yml" "$@" ;;
      *)        echo "nim: unknown models subcommand '$2'" >&2; echo "     usage: ./nim models [N] | ./nim models sync" >&2; exit 1 ;;
    esac ;;
  install)
    need_binary
    port_alive && { pretty_status; exit 0; }
    svc_start; sleep 2; pretty_status
    echo "installed ($SVC_CMD). see deploy/ for system-wide setup." ;;
  uninstall) svc_stop; rm -f "$PLIST" "$SYSTEMD"; echo "nimroute uninstalled" ;;
  help|-h|--help) usage; exit 0 ;;
  *) echo "nim: unknown command '${1}'" >&2; echo "" >&2; usage >&2; exit 1 ;;
esac
