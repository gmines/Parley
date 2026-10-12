#!/bin/sh
# Renders vars.xml from the environment, then runs FreeSWITCH in the foreground.
#
#   PARLEY_MEDIA_IP       IP clients send audio to (advertised in SDP). Required.
#   PARLEY_ESL_PASSWORD   Event Socket password for voice-control. Required.
#   PARLEY_RTP_IP         local IP to bind RTP on (default: PARLEY_MEDIA_IP)
#   PARLEY_SIP_PORT       loopback SIP port Kamailio forwards to (default 5080)
#   PARLEY_RTP_START/END  UDP port range for audio (default 10000-10999)
set -eu
: "${PARLEY_MEDIA_IP:?set PARLEY_MEDIA_IP to the address clients reach this server on}"
: "${PARLEY_ESL_PASSWORD:?set PARLEY_ESL_PASSWORD}"
export PARLEY_RTP_IP="${PARLEY_RTP_IP:-$PARLEY_MEDIA_IP}"
export PARLEY_SIP_PORT="${PARLEY_SIP_PORT:-5080}"
export PARLEY_RTP_START="${PARLEY_RTP_START:-10000}"
export PARLEY_RTP_END="${PARLEY_RTP_END:-10999}"
case "$PARLEY_ESL_PASSWORD" in *[!A-Za-z0-9_-]*) echo "PARLEY_ESL_PASSWORD may only contain letters, digits, '_' and '-'" >&2; exit 1;; esac

CONF=/usr/local/freeswitch/conf
cp -r /usr/share/parley-freeswitch/. "$CONF/"
envsubst '$PARLEY_MEDIA_IP $PARLEY_RTP_IP $PARLEY_SIP_PORT $PARLEY_ESL_PASSWORD $PARLEY_RTP_START $PARLEY_RTP_END' \
  < "$CONF/vars.xml.tmpl" > "$CONF/vars.xml"
rm "$CONF/vars.xml.tmpl"
# Already running as the freeswitch user. -c: foreground, log to stdout; -nonat: no UPnP/NAT-PMP probing.
# Explicit directories: never fall back to a built-in default config.
FS=/usr/local/freeswitch
exec $FS/bin/freeswitch -nonat -c -conf "$CONF" -log $FS/log -db $FS/db -run $FS/run -mod $FS/lib/freeswitch/mod
