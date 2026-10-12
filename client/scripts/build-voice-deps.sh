#!/usr/bin/env bash
# Builds the native voice libraries the desktop client links: Opus and
# pjproject (SIP, RTP, SRTP, audio devices, echo cancellation), as static,
# position-independent libraries in client/.deps/<os>-<arch>/.
#
# Every source tarball is pinned by SHA-256. Re-running is a no-op unless the
# versions or options in this file change.
#
#   client/scripts/build-voice-deps.sh            # Linux, macOS (host arch)
#   VOICE_DEPS_ARCH=x86_64 client/scripts/build-voice-deps.sh   # macOS: other arch
#                                                  (universal builds need arm64 + x86_64)
# Windows: client/scripts/build-voice-deps.ps1
#
# Linux needs the ALSA headers (Fedora: alsa-lib-devel, Debian: libasound2-dev).
set -euo pipefail

OPUS_VERSION=1.6.1
OPUS_SHA256=6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1
PJ_VERSION=2.17
PJ_SHA256=065fe06c06788d97c35f563796d59f00ce52fe9558a52d7b490a042a966facce

HERE="$(cd "$(dirname "$0")/.." && pwd)"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
HOST_ARCH="$(uname -m)"
ARCH="${VOICE_DEPS_ARCH:-$HOST_ARCH}"
HOST_FLAG=()
if [[ "$ARCH" != "$HOST_ARCH" ]]; then
  [[ "$OS" == darwin ]] || { echo "cross-arch builds are only supported on macOS" >&2; exit 1; }
  # autoconf spells Apple Silicon "aarch64"
  HOST_FLAG=(--host="$([[ "$ARCH" == arm64 ]] && echo aarch64 || echo "$ARCH")-apple-darwin")
fi
PREFIX="${VOICE_DEPS_PREFIX:-$HERE/.deps/$OS-$ARCH}"
WORK="$PREFIX/src"
STAMP="$PREFIX/.stamp"
WANT="opus=$OPUS_VERSION pjproject=$PJ_VERSION script=$(sha256sum "$0" 2>/dev/null | cut -c1-16 || shasum -a 256 "$0" | cut -c1-16)"
JOBS="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"

SOUND=native
if [[ "$OS" == linux ]] && ! pkg-config --exists alsa; then
  if [[ "${VOICE_ALLOW_NO_SOUND:-}" == 1 ]]; then
    # Test builds only: pjproject falls back to a null (silent) sound device.
    echo "warning: no ALSA headers; building WITHOUT sound devices (VOICE_ALLOW_NO_SOUND=1)" >&2
    SOUND=none
  else
    echo "ALSA headers not found: install alsa-lib-devel (Fedora) or libasound2-dev (Debian/Ubuntu)" >&2
    exit 1
  fi
fi
WANT="$WANT sound=$SOUND"

if [[ -f "$STAMP" && "$(cat "$STAMP")" == "$WANT" ]]; then
  echo "voice deps up to date in $PREFIX"
  exit 0
fi

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
fetch() { # url sha256 dest
  curl -fsSL "$1" -o "$3"
  local got; got="$(sha256 "$3")"
  if [[ "$got" != "$2" ]]; then echo "checksum mismatch for $1: got $got" >&2; exit 1; fi
}

rm -rf "$PREFIX"
mkdir -p "$WORK"
export CFLAGS="-O2 -fPIC" CXXFLAGS="-O2 -fPIC"
if [[ "$OS" == darwin ]]; then
  export MACOSX_DEPLOYMENT_TARGET="${MACOSX_DEPLOYMENT_TARGET:-11.0}"
  export CFLAGS="$CFLAGS -arch $ARCH" CXXFLAGS="$CXXFLAGS -arch $ARCH" LDFLAGS="-arch $ARCH"
fi

echo "==> opus $OPUS_VERSION"
fetch "https://downloads.xiph.org/releases/opus/opus-$OPUS_VERSION.tar.gz" "$OPUS_SHA256" "$WORK/opus.tgz"
mkdir "$WORK/opus" && tar xzf "$WORK/opus.tgz" -C "$WORK/opus" --strip-components=1
(cd "$WORK/opus" && ./configure "${HOST_FLAG[@]}" --prefix="$PREFIX" --disable-shared --enable-static --with-pic \
   --disable-doc --disable-extra-programs >/dev/null && make -j"$JOBS" >/dev/null && make install >/dev/null)

echo "==> pjproject $PJ_VERSION"
fetch "https://github.com/pjsip/pjproject/archive/refs/tags/$PJ_VERSION.tar.gz" "$PJ_SHA256" "$WORK/pj.tgz"
mkdir "$WORK/pj" && tar xzf "$WORK/pj.tgz" -C "$WORK/pj" --strip-components=1
cat > "$WORK/pj/pjlib/include/pj/config_site.h" <<'CFG'
/* Parley: audio-only SIP client. */
#define PJMEDIA_HAS_VIDEO            0
#define PJMEDIA_HAS_SRTP             1
/* No TLS sockets yet (SIP over TLS is phase 4); SRTP has its own crypto. */
#define PJ_HAS_SSL_SOCK              0
#define PJMEDIA_HAS_OPUS_CODEC       1
#define PJMEDIA_HAS_G722_CODEC       1
/* Wideband conference bridge so Opus isn't squeezed through 8 kHz. */
#define PJSUA_DEFAULT_CLOCK_RATE     48000
#define PJSUA_DEFAULT_AUDIO_FRAME_PTIME 20
#define PJMEDIA_SND_DEFAULT_REC_LATENCY  60
#define PJMEDIA_SND_DEFAULT_PLAY_LATENCY 60
/* Echo cancellation: WebRTC AEC (bundled with pjproject, not the WebRTC protocol). */
#define PJMEDIA_HAS_WEBRTC_AEC       1
/* The Speex codec is disabled, and its echo canceller lives in the same
 * library: turn it off too (we use the WebRTC AEC). */
#define PJMEDIA_HAS_SPEEX_AEC        0
#define PJSUA_MAX_CALLS              4
#define PJSUA_MAX_ACC                4
CFG
(cd "$WORK/pj" && ./configure "${HOST_FLAG[@]}" --prefix="$PREFIX" --disable-shared --disable-ssl \
   --disable-video --disable-sdl --disable-ffmpeg --disable-v4l2 --disable-openh264 \
   --disable-libyuv --disable-vpx --disable-bcg729 \
   --disable-speex-codec --disable-gsm-codec --disable-ilbc-codec --disable-g7221-codec \
   --with-opus="$PREFIX" > "$WORK/pj-configure.log" 2>&1 \
 && make dep > /dev/null 2>&1 && make -j"$JOBS" > "$WORK/pj-make.log" 2>&1 && make install > /dev/null)

grep -E 'Using SSL|opus|alsa|ALSA|Checking sound' "$WORK/pj-configure.log" | sed 's/^/   /' || true
echo "$WANT" > "$STAMP"
echo "voice deps installed in $PREFIX"
