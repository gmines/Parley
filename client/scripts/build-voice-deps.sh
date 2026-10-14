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
# Linux needs the ALSA and OpenSSL headers (Fedora: alsa-lib-devel openssl-devel,
# Debian: libasound2-dev libssl-dev).
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
(cd "$WORK/opus" && ./configure ${HOST_FLAG[@]+"${HOST_FLAG[@]}"} --prefix="$PREFIX" --disable-shared --enable-static --with-pic \
   --disable-doc --disable-extra-programs >/dev/null && make -j"$JOBS" >/dev/null && make install >/dev/null)

echo "==> pjproject $PJ_VERSION"
fetch "https://github.com/pjsip/pjproject/archive/refs/tags/$PJ_VERSION.tar.gz" "$PJ_SHA256" "$WORK/pj.tgz"
mkdir "$WORK/pj" && tar xzf "$WORK/pj.tgz" -C "$WORK/pj" --strip-components=1
cat > "$WORK/pj/pjlib/include/pj/config_site.h" <<'CFG'
/* Parley: audio-only SIP client. */
#define PJMEDIA_HAS_VIDEO            0
#define PJMEDIA_HAS_SRTP             1
/* SIP over TLS, with the OS's TLS library and trusted certificates: Secure
 * Transport on macOS, the system OpenSSL on Linux (Schannel on Windows, see
 * build-voice-deps.ps1). configure detects the same; this pins it. */
#undef  PJ_HAS_SSL_SOCK
#define PJ_HAS_SSL_SOCK              1
#undef  PJ_SSL_SOCK_IMP
#if defined(__APPLE__)
#  define PJ_SSL_SOCK_IMP            PJ_SSL_SOCK_IMP_DARWIN
#else
#  define PJ_SSL_SOCK_IMP            PJ_SSL_SOCK_IMP_OPENSSL
#endif
#define PJMEDIA_HAS_OPUS_CODEC       1
#define PJMEDIA_HAS_G722_CODEC       1
/* Wideband conference bridge so Opus isn't squeezed through 8 kHz. */
#define PJSUA_DEFAULT_CLOCK_RATE     48000
#define PJSUA_DEFAULT_AUDIO_FRAME_PTIME 20
#define PJMEDIA_SND_DEFAULT_REC_LATENCY  60
#define PJMEDIA_SND_DEFAULT_PLAY_LATENCY 60
/* Echo cancellation: WebRTC AEC3 (bundled with pjproject, not the WebRTC
 * protocol). The legacy AEC is kept as a fallback, but pjmedia drives it with
 * one band only, so it aborts above 16 kHz (see parley_voice.c). */
#define PJMEDIA_HAS_WEBRTC_AEC       1
#define PJMEDIA_HAS_WEBRTC_AEC3      1
/* The Speex codec is disabled, and its echo canceller lives in the same
 * library: turn it off too (we use the WebRTC AEC). */
#define PJMEDIA_HAS_SPEEX_AEC        0
#define PJSUA_MAX_CALLS              4
/* Outbound-only TLS: no listening socket on the client. */
#define PJSIP_TLS_TRANSPORT_DONT_CREATE_LISTENER 1
#define PJSUA_MAX_ACC                4
CFG
# TLS: on Linux, configure finds the system OpenSSL. On macOS, config_site.h
# picks Secure Transport, and its framework is linked here: configure's own
# check for it compiles with -Werror and fails on the API's deprecation
# warning. --disable-ssl stops it from picking up a Homebrew OpenSSL instead
# (a dylib the app would then depend on).
SSL_FLAG=()
PJ_LIBS="${LIBS:-}"
if [[ "$OS" == darwin ]]; then
  SSL_FLAG=(--disable-ssl)
  PJ_LIBS="$PJ_LIBS -framework Security"
fi
(cd "$WORK/pj" && LIBS="$PJ_LIBS" ./configure ${HOST_FLAG[@]+"${HOST_FLAG[@]}"} --prefix="$PREFIX" --disable-shared ${SSL_FLAG[@]+"${SSL_FLAG[@]}"} \
   --disable-video --disable-sdl --disable-ffmpeg --disable-v4l2 --disable-openh264 \
   --disable-libyuv --disable-vpx --disable-bcg729 \
   --disable-speex-codec --disable-gsm-codec --disable-ilbc-codec --disable-g7221-codec \
   --enable-libwebrtc-aec3 --with-opus="$PREFIX" > "$WORK/pj-configure.log" 2>&1 \
 && make dep > "$WORK/pj-dep.log" 2>&1 && make -j"$JOBS" > "$WORK/pj-make.log" 2>&1 && make install > /dev/null) || {
  # Show why (CI keeps no build directory).
  for log in pj-make.log pj-dep.log pj-configure.log; do
    [[ -s "$WORK/$log" ]] || continue
    echo "pjproject build failed; end of $log:" >&2
    grep -nE 'error|Error' "$WORK/$log" | head -n 20 >&2 || true
    tail -n 30 "$WORK/$log" >&2
    break
  done
  exit 1
}

grep -E 'SSL|opus|alsa|ALSA|Checking sound|AEC3' "$WORK/pj-configure.log" | sed 's/^/   /' || true
# The shim needs TLS (config_site.h says it's there): on Linux, make sure
# configure found OpenSSL to link it with. (voiceprobe --selftest checks
# TLS works on every platform.)
if [[ "$OS" == linux ]]; then
  grep -q 'OpenSSL library found' "$WORK/pj-configure.log" || { echo "pjproject: OpenSSL not found (install libssl-dev / openssl-devel)" >&2; exit 1; }
fi
# The shim relies on AEC3 being in the library (config_site.h says it is).
grep -q 'webrtc-aec3' "$PREFIX/lib/pkgconfig/libpjproject.pc" || { echo "pjproject was built without WebRTC AEC3" >&2; exit 1; }
echo "$WANT" > "$STAMP"
echo "voice deps installed in $PREFIX"
