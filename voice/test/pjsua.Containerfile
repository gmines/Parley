# Test tool only: pjsua (pjproject's command-line SIP client) built with the
# same pjproject version and codecs the desktop app uses, for exercising the
# voice stack without the app. See voice/test/README.md.
FROM debian:bookworm-slim AS build
ARG PJ_VERSION=2.17
ARG PJ_SHA256=065fe06c06788d97c35f563796d59f00ce52fe9558a52d7b490a042a966facce
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl pkg-config libopus-dev libssl-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN curl -fsSL https://github.com/pjsip/pjproject/archive/refs/tags/${PJ_VERSION}.tar.gz -o pj.tgz \
    && echo "${PJ_SHA256}  pj.tgz" | sha256sum -c - && tar xzf pj.tgz --strip-components=1
RUN printf '#define PJMEDIA_HAS_VIDEO 0\n#define PJMEDIA_HAS_SRTP 1\n' > pjlib/include/pj/config_site.h \
    && ./configure --disable-video --disable-sound --disable-libyuv --disable-libwebrtc \
         --with-opus --with-ssl >/dev/null \
    && make dep >/dev/null && make -j"$(nproc)" >/dev/null \
    && cp pjsip-apps/bin/pjsua-* /usr/local/bin/pjsua

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends libopus0 libssl3 ca-certificates sox \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/bin/pjsua /usr/local/bin/pjsua
ENTRYPOINT ["pjsua"]
