#!/bin/sh
# Renders the config templates from environment variables, then runs Asterisk
# in the foreground.
#
#   PARLEY_MEDIA_IP       IP clients send audio to (advertised in SDP). Required.
#   PARLEY_ARI_PASSWORD   password for the "parley" ARI user. Required.
#   PARLEY_SIP_PORT       loopback port Kamailio forwards to (default 5080)
#   PARLEY_RTP_START/END  UDP port range for audio (default 10000-10999)
#   PARLEY_ARI_BIND       HTTP/ARI bind address (default 127.0.0.1:8088)
set -eu
: "${PARLEY_MEDIA_IP:?set PARLEY_MEDIA_IP to the address clients reach this server on}"
: "${PARLEY_ARI_PASSWORD:?set PARLEY_ARI_PASSWORD}"
export PARLEY_SIP_PORT="${PARLEY_SIP_PORT:-5080}"
export PARLEY_RTP_START="${PARLEY_RTP_START:-10000}"
export PARLEY_RTP_END="${PARLEY_RTP_END:-10999}"
PARLEY_ARI_BIND="${PARLEY_ARI_BIND:-127.0.0.1:8088}"
export PARLEY_ARI_BIND_ADDR="${PARLEY_ARI_BIND%:*}"
export PARLEY_ARI_BIND_PORT="${PARLEY_ARI_BIND##*:}"

# Opus needs the (optional) transcoder to be mixed; otherwise offer G.722 only.
if [ -f /usr/lib/asterisk/modules/codec_opus.so ]; then
  export PARLEY_CODECS=opus,g722
else
  export PARLEY_CODECS=g722
  echo "codec_opus not installed: calls will use G.722" >&2
fi

# Only *.tmpl files are templates, and only our variables are substituted:
# Asterisk configs use ${...} for their own variables too.
vars='$PARLEY_MEDIA_IP $PARLEY_ARI_PASSWORD $PARLEY_SIP_PORT $PARLEY_RTP_START $PARLEY_RTP_END $PARLEY_ARI_BIND_ADDR $PARLEY_ARI_BIND_PORT $PARLEY_CODECS'
for f in /usr/share/parley-asterisk/*; do
  case "$f" in
    *.tmpl) envsubst "$vars" < "$f" > "/etc/asterisk/$(basename "$f" .tmpl)" ;;
    *) cp "$f" /etc/asterisk/ ;;
  esac
done
exec /usr/sbin/asterisk -f -vvv
