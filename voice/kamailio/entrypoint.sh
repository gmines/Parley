#!/bin/sh
# Renders kamailio.cfg from the template (only our variables are substituted;
# Kamailio's own $var(...) syntax is left alone) and runs Kamailio.
#
# Environment:
#   PARLEY_SIP_SECRET        shared with voice-control (HMAC key). Required.
#   PARLEY_SIP_TLS_CERT_DIR  directory with tls.crt and tls.key (e.g. a mounted
#                            cert-manager Secret): enables SIP over TLS.
#   PARLEY_SIP_TLS_ADVERTISE hostname[:port] clients reach TLS on; must match the
#                            certificate. Default: the certificate's first name.
#   PARLEY_SIP_TLS_PORT      default 5061
#   PARLEY_SIP_PLAIN         "yes" (default) / "no": plain SIP on 5060, for
#                            clients without TLS (before 0.4)
#   PARLEY_SIP_ADVERTISE     IP or name clients reach plain SIP on (required with plain SIP)
#   PARLEY_SIP_BIND          address to listen on, default 0.0.0.0
#   PARLEY_SIP_MEDIA         where FreeSWITCH listens, default "sip:127.0.0.1:5080"
set -eu
: "${PARLEY_SIP_SECRET:?set PARLEY_SIP_SECRET (shared with voice-control)}"
case "$PARLEY_SIP_SECRET" in *'"'*|*'\'*) echo "PARLEY_SIP_SECRET must not contain quotes or backslashes" >&2; exit 1;; esac
export PARLEY_SIP_MEDIA="${PARLEY_SIP_MEDIA:-sip:127.0.0.1:5080}"
export PARLEY_SIP_BIND="${PARLEY_SIP_BIND:-0.0.0.0}"
export PARLEY_SIP_TLS_PORT="${PARLEY_SIP_TLS_PORT:-5061}"
export PARLEY_SIP_ADVERTISE="${PARLEY_SIP_ADVERTISE:-}"
export PARLEY_SIP_TLS_ADVERTISE="${PARLEY_SIP_TLS_ADVERTISE:-}"
TLS_DIR="${PARLEY_SIP_TLS_CERT_DIR:-}"
defines=""

if [ "${PARLEY_SIP_PLAIN:-yes}" = yes ]; then
  : "${PARLEY_SIP_ADVERTISE:?set PARLEY_SIP_ADVERTISE to the IP clients reach this server on}"
  defines="$defines -A WITH_PLAIN"
fi

if [ -n "$TLS_DIR" ]; then
  for f in tls.crt tls.key; do
    [ -r "$TLS_DIR/$f" ] || { echo "$TLS_DIR/$f is missing or unreadable" >&2; exit 1; }
  done
  if [ -z "$PARLEY_SIP_TLS_ADVERTISE" ]; then
    PARLEY_SIP_TLS_ADVERTISE="$(openssl x509 -in "$TLS_DIR/tls.crt" -noout -ext subjectAltName \
      | tr ',' '\n' | sed -n 's/^ *DNS://p' | head -n1)"
    [ -n "$PARLEY_SIP_TLS_ADVERTISE" ] || { echo "set PARLEY_SIP_TLS_ADVERTISE (no DNS name in the certificate)" >&2; exit 1; }
  fi
  case "$PARLEY_SIP_TLS_ADVERTISE" in *:*) ;; *) PARLEY_SIP_TLS_ADVERTISE="$PARLEY_SIP_TLS_ADVERTISE:$PARLEY_SIP_TLS_PORT";; esac
  cat > /tmp/kamailio-tls.cfg <<CFG
[server:default]
method = TLSv1.2+
verify_certificate = no
require_certificate = no
certificate = $TLS_DIR/tls.crt
private_key = $TLS_DIR/tls.key

[client:default]
method = TLSv1.2+
verify_certificate = no
require_certificate = no
CFG
  defines="$defines -A WITH_TLS"
fi
[ -n "$defines" ] || { echo "nothing to listen on: enable TLS (PARLEY_SIP_TLS_CERT_DIR) or plain SIP" >&2; exit 1; }

envsubst '$PARLEY_SIP_SECRET $PARLEY_SIP_ADVERTISE $PARLEY_SIP_MEDIA $PARLEY_SIP_BIND $PARLEY_SIP_TLS_PORT $PARLEY_SIP_TLS_ADVERTISE' \
  < /etc/kamailio/kamailio.cfg.tmpl > /tmp/kamailio.cfg

# shellcheck disable=SC2086
if [ -z "$TLS_DIR" ]; then
  exec kamailio -DD -E -f /tmp/kamailio.cfg $defines "$@"
fi

# With TLS, this shell stays as the parent: it reloads a renewed certificate
# (cert-manager replaces the Secret's files) without restarting Kamailio, so
# calls in progress keep their connections. Kamailio waits for all its
# children on shutdown, so the watcher must not be one of them.
kamailio -DD -E -f /tmp/kamailio.cfg $defines "$@" &
kpid=$!
(
  sum() { cat "$TLS_DIR/tls.crt" "$TLS_DIR/tls.key" 2>/dev/null | cksum; }
  last="$(sum)"
  while sleep 60; do
    now="$(sum)"
    if [ "$now" != "$last" ] && kamcmd -s unix:/tmp/kamailio_ctl tls.reload >/dev/null; then
      echo "TLS certificate reloaded" >&2
      last="$now"
    fi
  done
) &
wpid=$!
trap 'kill -TERM "$kpid" 2>/dev/null' TERM INT
status=0
while :; do
  wait "$kpid" || status=$?
  kill -0 "$kpid" 2>/dev/null || break
done
kill "$wpid" 2>/dev/null || true
exit "$status"
