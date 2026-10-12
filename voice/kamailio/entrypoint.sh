#!/bin/sh
# Renders kamailio.cfg from the template (only our variables are substituted;
# Kamailio's own $var(...) syntax is left alone) and runs Kamailio.
set -eu
: "${PARLEY_SIP_SECRET:?set PARLEY_SIP_SECRET (shared with voice-control)}"
: "${PARLEY_SIP_ADVERTISE:?set PARLEY_SIP_ADVERTISE to the IP clients reach this server on}"
export PARLEY_SIP_ASTERISK="${PARLEY_SIP_ASTERISK:-sip:127.0.0.1:5080}"
case "$PARLEY_SIP_SECRET" in *'"'*|*'\'*) echo "PARLEY_SIP_SECRET must not contain quotes or backslashes" >&2; exit 1;; esac
envsubst '$PARLEY_SIP_SECRET $PARLEY_SIP_ADVERTISE $PARLEY_SIP_ASTERISK' \
  < /etc/kamailio/kamailio.cfg.tmpl > /tmp/kamailio.cfg
exec kamailio -DD -E -f /tmp/kamailio.cfg "$@"
