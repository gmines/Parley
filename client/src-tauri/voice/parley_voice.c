/* See parley_voice.h. */
#include "parley_voice.h"

#include <pjsua-lib/pjsua.h>
#include <pjmedia-codec/opus.h>
#include <stdio.h>
#include <string.h>

#define THIS_FILE "parley_voice"

/* Echo canceller and the sound device rate it runs at. AEC3 handles 48 kHz
 * (it splits the signal into bands itself). pjmedia drives the legacy WebRTC
 * AEC with a single band, which aborts above 16 kHz (num_bands assertion), so
 * that one only runs with the sound device at 16 kHz. */
#if defined(PJMEDIA_HAS_WEBRTC_AEC3) && PJMEDIA_HAS_WEBRTC_AEC3 != 0
#define PV_EC_ALGO PJMEDIA_ECHO_WEBRTC_AEC3
#define PV_SND_RATE 48000
#else
#define PV_EC_ALGO PJMEDIA_ECHO_WEBRTC
#define PV_SND_RATE 16000
#endif
#define PV_EC_OPTIONS (PV_EC_ALGO | PJMEDIA_ECHO_USE_NOISE_SUPPRESSOR | PJMEDIA_ECHO_AGGRESSIVENESS_DEFAULT)
#define PV_EC_TAIL_MS 200

#if defined(_MSC_VER)
#define PV_THREAD_LOCAL __declspec(thread)
#else
#define PV_THREAD_LOCAL __thread
#endif

static pv_event_cb g_cb;
static void *g_ctx;
static int g_started;
static pjsua_acc_id g_acc = PJSUA_INVALID_ID;
static pjsua_call_id g_call = PJSUA_INVALID_ID;
static int g_muted, g_deaf;
static pjsua_player_id g_player = PJSUA_INVALID_ID;
static pjsua_recorder_id g_recorder = PJSUA_INVALID_ID;
static int g_has_tls;
static int g_null_audio;
/* Ring tones play on their own output-only stream (pv_tone). */
static pj_pool_t *g_tone_pool;
static pjmedia_port *g_tone_gen;
static pjmedia_snd_port *g_tone_snd;

/* pjsua requires every calling thread to be registered with pjlib. */
static void ensure_thread(void) {
    static PV_THREAD_LOCAL pj_thread_desc desc;
    static PV_THREAD_LOCAL pj_thread_t *thread;
    if (!pj_thread_is_registered()) {
        pj_bzero(desc, sizeof(desc));
        pj_thread_register("parley", desc, &thread);
    }
}

static void emit(int kind, int a, int b, const char *text) {
    if (g_cb) g_cb(g_ctx, kind, a, b, text ? text : "");
}

static void on_log(int level, const char *data, int len) {
    char buf[512];
    int n = len < (int)sizeof(buf) - 1 ? len : (int)sizeof(buf) - 1;
    memcpy(buf, data, n);
    while (n > 0 && (buf[n - 1] == '\n' || buf[n - 1] == '\r')) n--;
    buf[n] = 0;
    emit(PV_EV_LOG, level, 0, buf);
}

/* Audio routing for the current call: mic -> call unless muted,
 * call -> speaker unless deafened, plus the test player/recorder. */
static void route_audio(void) {
    if (g_call == PJSUA_INVALID_ID) return;
    pjsua_call_info ci;
    if (pjsua_call_get_info(g_call, &ci) != PJ_SUCCESS) return;
    pjsua_conf_port_id slot = pjsua_call_get_conf_port(g_call);
    if (slot == PJSUA_INVALID_ID) return;

    pjsua_conf_disconnect(0, slot);
    pjsua_conf_disconnect(slot, 0);
    if (!g_muted) pjsua_conf_connect(0, slot);
    if (!g_deaf) pjsua_conf_connect(slot, 0);
    if (g_player != PJSUA_INVALID_ID) pjsua_conf_connect(pjsua_player_get_conf_port(g_player), slot);
    if (g_recorder != PJSUA_INVALID_ID) pjsua_conf_connect(slot, pjsua_recorder_get_conf_port(g_recorder));
}

static void on_call_state(pjsua_call_id call_id, pjsip_event *e) {
    PJ_UNUSED_ARG(e);
    pjsua_call_info ci;
    if (pjsua_call_get_info(call_id, &ci) != PJ_SUCCESS) return;
    int state;
    switch (ci.state) {
    case PJSIP_INV_STATE_CALLING:      state = PV_CALL_CALLING; break;
    case PJSIP_INV_STATE_EARLY:
    case PJSIP_INV_STATE_CONNECTING:   state = PV_CALL_CONNECTING; break;
    case PJSIP_INV_STATE_CONFIRMED:    state = PV_CALL_CONFIRMED; break;
    case PJSIP_INV_STATE_DISCONNECTED: state = PV_CALL_DISCONNECTED; break;
    default:                           state = PV_CALL_IDLE; break;
    }
    char reason[128];
    pj_ansi_snprintf(reason, sizeof(reason), "%.*s", (int)ci.last_status_text.slen, ci.last_status_text.ptr);
    if (state == PV_CALL_DISCONNECTED && call_id == g_call) {
        g_call = PJSUA_INVALID_ID;
        if (g_acc != PJSUA_INVALID_ID) {
            pjsua_acc_del(g_acc);
            g_acc = PJSUA_INVALID_ID;
        }
    }
    emit(PV_EV_CALL_STATE, state, ci.last_status, reason);
}

static void on_call_media_state(pjsua_call_id call_id) {
    pjsua_call_info ci;
    if (pjsua_call_get_info(call_id, &ci) != PJ_SUCCESS) return;
    int active = ci.media_status == PJSUA_CALL_MEDIA_ACTIVE;
    if (active) route_audio();
    emit(PV_EV_MEDIA, active, 0, NULL);
}

/* SIP over TLS to Kamailio, verifying its certificate against the OS's
 * trusted CAs and the server's name (outbound only: see config_site.h).
 * macOS and Windows use the system trust store; OpenSSL on Linux has to be
 * told where the distribution keeps its CA bundle. */
static pj_status_t create_tls_transport(void) {
    pjsua_transport_config cfg;
    pjsua_transport_config_default(&cfg);
    cfg.port = 0;
    cfg.tls_setting.verify_server = PJ_TRUE;
    cfg.tls_setting.timeout.sec = 10;
#if PJ_SSL_SOCK_IMP == PJ_SSL_SOCK_IMP_OPENSSL
    static const char *const bundles[] = {
        "/etc/ssl/certs/ca-certificates.crt", /* Debian, Ubuntu, Arch, Gentoo */
        "/etc/pki/tls/certs/ca-bundle.crt",   /* Fedora, RHEL */
        "/etc/ssl/ca-bundle.pem",             /* openSUSE */
        "/etc/ssl/cert.pem",                  /* Alpine, Void */
    };
    for (unsigned i = 0; i < PJ_ARRAY_SIZE(bundles); i++) {
        if (pj_file_exists(bundles[i])) {
            cfg.tls_setting.ca_list_file = pj_str((char *)bundles[i]);
            break;
        }
    }
    if (cfg.tls_setting.ca_list_file.slen == 0) {
        if (!pj_file_exists("/etc/ssl/certs")) {
            PJ_LOG(1, ("parley", "no trusted CA certificates found: SIP over TLS disabled"));
            return PJ_ENOTFOUND;
        }
        cfg.tls_setting.ca_list_path = pj_str("/etc/ssl/certs");
    }
#endif
    pj_status_t st = pjsua_transport_create(PJSIP_TRANSPORT_TLS, &cfg, NULL);
    if (st != PJ_SUCCESS) PJ_LOG(1, ("parley", "SIP over TLS unavailable (error %d)", st));
    return st;
}

static void configure_codecs(void) {
    pj_str_t id;
    /* Opus first, G.722 as fallback, nothing else. */
    pjsua_codec_info codecs[32];
    unsigned count = PJ_ARRAY_SIZE(codecs);
    if (pjsua_enum_codecs(codecs, &count) == PJ_SUCCESS) {
        for (unsigned i = 0; i < count; i++) pjsua_codec_set_priority(&codecs[i].codec_id, 0);
    }
    pjsua_codec_set_priority(pj_cstr(&id, "opus/48000/2"), 255);
    pjsua_codec_set_priority(pj_cstr(&id, "G722/16000/1"), 128);

    /* Opus tuned like the server: in-band FEC both ways, sized for ~10% loss,
     * VBR, no DTX. Setting plc advertises useinbandfec=1, so the server sends
     * FEC; our encoder enables FEC because the server advertises it too. */
    pjmedia_codec_param param;
    if (pjsua_codec_get_param(pj_cstr(&id, "opus/48000/2"), &param) == PJ_SUCCESS) {
        pjmedia_codec_opus_config cfg;
        pjmedia_codec_opus_get_config(&cfg);
        cfg.packet_loss = 10;
        cfg.complexity = 10;
        cfg.cbr = PJ_FALSE;
        param.setting.plc = 1;
        param.setting.vad = 0;
        pjmedia_codec_opus_set_default_param(&cfg, &param);
        pjsua_codec_set_param(pj_cstr(&id, "opus/48000/2"), &param);
    }
}

int pv_init(const pv_options *opt, pv_event_cb cb, void *ctx) {
    if (g_started) return 0;
    g_cb = cb;
    g_ctx = ctx;
    /* Apply the log level before pjsua_create(), which logs on its own. */
    pj_log_set_level(opt ? opt->log_level : 3);
    if (pjsua_create() != PJ_SUCCESS) return -1;
    ensure_thread();

    pjsua_config cfg;
    pjsua_config_default(&cfg);
    cfg.cb.on_call_state = &on_call_state;
    cfg.cb.on_call_media_state = &on_call_media_state;
    cfg.max_calls = 4;
    if (opt && opt->user_agent) cfg.user_agent = pj_str((char *)opt->user_agent);

    pjsua_logging_config log_cfg;
    pjsua_logging_config_default(&log_cfg);
    log_cfg.level = opt ? opt->log_level : 3;
    log_cfg.console_level = log_cfg.level;
    log_cfg.cb = &on_log;

    pjsua_media_config media;
    pjsua_media_config_default(&media);
    media.clock_rate = 48000;
    media.snd_clock_rate = PV_SND_RATE;
    media.channel_count = 1;
    media.no_vad = PJ_TRUE;
    /* Echo cancellation with noise suppression (see PV_EC_ALGO). */
    media.ec_tail_len = PV_EC_TAIL_MS;
    media.ec_options = PV_EC_OPTIONS;
    /* Adaptive jitter buffer: start small, grow with network jitter. */
    media.jb_init = -1;
    media.jb_min_pre = -1;
    media.jb_max_pre = -1;
    media.jb_max = -1;
    media.enable_ice = PJ_FALSE; /* phase 4 */

    if (pjsua_init(&cfg, &log_cfg, &media) != PJ_SUCCESS) {
        pjsua_destroy();
        return -2;
    }
    /* Outbound only: an ephemeral TCP port for signaling to Kamailio. */
    pjsua_transport_config tcfg;
    pjsua_transport_config_default(&tcfg);
    tcfg.port = 0;
    if (pjsua_transport_create(PJSIP_TRANSPORT_TCP, &tcfg, NULL) != PJ_SUCCESS) {
        pjsua_destroy();
        return -3;
    }
    /* TLS is preferred, but servers before 0.4 only speak plain TCP: a
     * missing TLS transport only rules out TLS (see pv_has_tls). */
    g_has_tls = create_tls_transport() == PJ_SUCCESS;
    g_null_audio = opt && opt->null_audio;
    if (g_null_audio) pjsua_set_null_snd_dev();
    if (pjsua_start() != PJ_SUCCESS) {
        pjsua_destroy();
        return -4;
    }
    configure_codecs();
    g_started = 1;
    return 0;
}

static void tone_stop(void) {
    if (g_tone_snd) {
        pjmedia_snd_port_disconnect(g_tone_snd);
        pjmedia_snd_port_destroy(g_tone_snd);
        g_tone_snd = NULL;
    }
    if (g_tone_gen) {
        pjmedia_port_destroy(g_tone_gen);
        g_tone_gen = NULL;
    }
    if (g_tone_pool) {
        pj_pool_release(g_tone_pool);
        g_tone_pool = NULL;
    }
}

void pv_shutdown(void) {
    if (!g_started) return;
    ensure_thread();
    tone_stop();
    pjsua_call_hangup_all();
    pjsua_destroy();
    g_started = 0;
    g_call = PJSUA_INVALID_ID;
    g_acc = PJSUA_INVALID_ID;
}

int pv_has_tls(void) {
    return g_has_tls;
}

int pv_join(const pv_join_params *p) {
    if (!g_started) return -1;
    ensure_thread();
    if (g_call != PJSUA_INVALID_ID) return -2; /* already in a call */
    if (g_acc != PJSUA_INVALID_ID) {
        pjsua_acc_del(g_acc);
        g_acc = PJSUA_INVALID_ID;
    }

    char id[256], proxy[256];
    pj_ansi_snprintf(id, sizeof(id), "sip:%s@%s", p->identity, p->realm);
    pj_ansi_snprintf(proxy, sizeof(proxy), "%s;lr", p->proxy);

    pjsua_acc_config acc;
    pjsua_acc_config_default(&acc);
    acc.id = pj_str(id);
    acc.register_on_acc_add = PJ_FALSE; /* never register: outbound calls only */
    acc.proxy_cnt = 1;
    acc.proxy[0] = pj_str(proxy);
    acc.cred_count = 1;
    acc.cred_info[0].realm = pj_str((char *)p->realm);
    acc.cred_info[0].scheme = pj_str("digest");
    acc.cred_info[0].username = pj_str((char *)p->username);
    acc.cred_info[0].data_type = PJSIP_CRED_DATA_PLAIN_PASSWD;
    acc.cred_info[0].data = pj_str((char *)p->password);
    acc.use_srtp = PJMEDIA_SRTP_MANDATORY;
    /* The SDP carries the SRTP keys: over a TLS proxy, refuse to send them
     * any other way. Servers before 0.4 hand out a plain TCP proxy. */
    acc.srtp_secure_signaling = strstr(p->proxy, "transport=tls") ? 1 : 0;
    acc.allow_contact_rewrite = PJ_TRUE;
    /* RTCP on the same port as RTP (RFC 5761): one NAT mapping instead of
     * two, and the server can't mis-latch a separate RTCP port. */
    acc.enable_rtcp_mux = PJ_TRUE;

    if (pjsua_acc_add(&acc, PJ_FALSE, &g_acc) != PJ_SUCCESS) return -3;

    pjsua_call_setting opt;
    pjsua_call_setting_default(&opt);
    opt.aud_cnt = 1;
    opt.vid_cnt = 0;
    pj_str_t uri = pj_str((char *)p->room_uri);
    if (pjsua_call_make_call(g_acc, &uri, &opt, NULL, NULL, &g_call) != PJ_SUCCESS) {
        pjsua_acc_del(g_acc);
        g_acc = PJSUA_INVALID_ID;
        g_call = PJSUA_INVALID_ID;
        return -4;
    }
    return 0;
}

void pv_leave(void) {
    if (!g_started) return;
    ensure_thread();
    if (g_call != PJSUA_INVALID_ID) pjsua_call_hangup(g_call, 0, NULL, NULL);
}

void pv_set_mute(int muted) {
    if (!g_started) return;
    ensure_thread();
    g_muted = muted;
    route_audio();
}

void pv_set_deaf(int deaf) {
    if (!g_started) return;
    ensure_thread();
    g_deaf = deaf;
    route_audio();
}

int pv_get_stats(pv_stats *out) {
    if (!g_started || g_call == PJSUA_INVALID_ID) return -1;
    ensure_thread();
    pj_bzero(out, sizeof(*out));
    pjsua_call_info ci;
    if (pjsua_call_get_info(g_call, &ci) != PJ_SUCCESS) return -2;
    out->duration_s = (unsigned)ci.connect_duration.sec;

    pjsua_stream_info si;
    if (pjsua_call_get_stream_info(g_call, 0, &si) == PJ_SUCCESS && si.type == PJMEDIA_TYPE_AUDIO) {
        pj_ansi_snprintf(out->codec, sizeof(out->codec), "%.*s",
                         (int)si.info.aud.fmt.encoding_name.slen, si.info.aud.fmt.encoding_name.ptr);
        out->clock_rate = si.info.aud.fmt.clock_rate;
    }
    pjsua_stream_stat st;
    if (pjsua_call_get_stream_stat(g_call, 0, &st) == PJ_SUCCESS) {
        /* pjmedia reports microseconds; round up so a measured sub-millisecond
         * value (LAN) shows as 1 ms rather than "unknown" (0). */
        out->rtt_ms = st.rtcp.rtt.last ? (st.rtcp.rtt.last + 999) / 1000 : 0;
        out->rx_jitter_ms = (st.rtcp.rx.jitter.mean + 999) / 1000;
        out->rx_packets = st.rtcp.rx.pkt;
        out->rx_lost = st.rtcp.rx.loss;
        out->tx_packets = st.rtcp.tx.pkt;
        out->tx_lost = st.rtcp.tx.loss;
    }
    pjmedia_transport_info ti;
    pjmedia_transport_info_init(&ti);
    if (pjsua_call_get_med_transport_info(g_call, 0, &ti) == PJ_SUCCESS) {
        for (unsigned i = 0; i < ti.specific_info_cnt; i++) {
            if (ti.spc_info[i].type == PJMEDIA_TRANSPORT_TYPE_SRTP) {
                pjmedia_srtp_info *srtp = (pjmedia_srtp_info *)ti.spc_info[i].buffer;
                out->srtp = srtp->active ? 1 : 0;
            }
        }
    }
    return 0;
}

int pv_device_count(void) {
    if (!g_started) return 0;
    ensure_thread();
    return (int)pjmedia_aud_dev_count();
}

int pv_device_info(int index, pv_device *out) {
    if (!g_started) return -1;
    ensure_thread();
    pjmedia_aud_dev_info info;
    if (pjmedia_aud_dev_get_info(index, &info) != PJ_SUCCESS) return -2;
    pj_ansi_snprintf(out->name, sizeof(out->name), "%s", info.name);
    out->inputs = (int)info.input_count;
    out->outputs = (int)info.output_count;
    return 0;
}

int pv_set_devices(int capture, int playback) {
    if (!g_started) return -1;
    ensure_thread();
    if (capture < 0) capture = PJMEDIA_AUD_DEFAULT_CAPTURE_DEV;
    if (playback < 0) playback = PJMEDIA_AUD_DEFAULT_PLAYBACK_DEV;
    return pjsua_set_snd_dev(capture, playback) == PJ_SUCCESS ? 0 : -2;
}

/* Ring tones, generated (no audio files) and played on an output-only stream
 * of their own: ringing doesn't open the microphone, and it doesn't touch
 * the call's audio. */
#define TONE_RATE 16000
#define TONE_FRAME (TONE_RATE / 50)

int pv_tone(int kind, int playback) {
    if (!g_started) return -1;
    ensure_thread();
    tone_stop();
    if (kind == PV_TONE_NONE) return 0;
    if (g_null_audio) return -2;

    /* Ringback (caller): the North American 440+480 Hz, 2 s on, 4 s off.
     * Incoming: two short rising chirps, twice, every 3 s. */
    static const pjmedia_tone_desc ringback[] = {{440, 480, 2000, 4000, 7000, 0}};
    static const pjmedia_tone_desc ring[] = {
        {660, 0, 220, 30, 0, 0}, {880, 0, 220, 400, 0, 0},
        {660, 0, 220, 30, 0, 0}, {880, 0, 220, 1880, 0, 0},
    };
    const pjmedia_tone_desc *tones = kind == PV_TONE_RINGBACK ? ringback : ring;
    unsigned count = kind == PV_TONE_RINGBACK ? PJ_ARRAY_SIZE(ringback) : PJ_ARRAY_SIZE(ring);

    if (playback < 0) {
        int cap;
        if (pjsua_get_snd_dev(&cap, &playback) != PJ_SUCCESS || playback < PJMEDIA_AUD_DEFAULT_PLAYBACK_DEV)
            playback = PJMEDIA_AUD_DEFAULT_PLAYBACK_DEV;
    }
    g_tone_pool = pjsua_pool_create("tone", 1024, 1024);
    if (!g_tone_pool) return -3;
    if (pjmedia_tonegen_create(g_tone_pool, TONE_RATE, 1, TONE_FRAME, 16, 0, &g_tone_gen) != PJ_SUCCESS ||
        pjmedia_tonegen_play(g_tone_gen, count, tones, PJMEDIA_TONEGEN_LOOP) != PJ_SUCCESS ||
        pjmedia_snd_port_create_player(g_tone_pool, playback, TONE_RATE, 1, TONE_FRAME, 16, 0, &g_tone_snd) != PJ_SUCCESS ||
        pjmedia_snd_port_connect(g_tone_snd, g_tone_gen) != PJ_SUCCESS) {
        tone_stop();
        return -4;
    }
    return 0;
}

int pv_test_play(const char *wav_path) {
    if (!g_started) return -1;
    ensure_thread();
    pj_str_t path = pj_str((char *)wav_path);
    if (pjsua_player_create(&path, 0, &g_player) != PJ_SUCCESS) return -2;
    route_audio();
    return 0;
}

int pv_test_record(const char *wav_path) {
    if (!g_started) return -1;
    ensure_thread();
    pj_str_t path = pj_str((char *)wav_path);
    if (pjsua_recorder_create(&path, 0, NULL, 0, 0, &g_recorder) != PJ_SUCCESS) return -2;
    route_audio();
    return 0;
}

int pv_ec_selftest(char *algo, int algo_len) {
    if (!g_started) return -1;
    ensure_thread();
    /* Same settings pjsua uses on the sound port: 20 ms mono frames. */
    const unsigned rate = PV_SND_RATE, spf = rate / 50;
    pj_pool_t *pool = pjsua_pool_create("ectest", 4000, 4000);
    if (!pool) return -2;
    pjmedia_echo_state *ec = NULL;
    int rc = 0;
    if (pjmedia_echo_create2(pool, rate, 1, spf, PV_EC_TAIL_MS, 0, PV_EC_OPTIONS, &ec) != PJ_SUCCESS) {
        rc = -3;
    } else {
        pj_int16_t *play = pj_pool_calloc(pool, spf, sizeof(pj_int16_t));
        pj_int16_t *rec = pj_pool_calloc(pool, spf, sizeof(pj_int16_t));
        /* Two seconds of a tone and its echo: enough frames to reach the
         * band processing that aborted before. */
        for (unsigned f = 0; f < 100 && rc == 0; f++) {
            for (unsigned i = 0; i < spf; i++) {
                play[i] = (pj_int16_t)(((f * spf + i) * 440 * 2 / rate) % 2 ? 4000 : -4000);
                rec[i] = (pj_int16_t)(play[i] / 4);
            }
            if (pjmedia_echo_cancel(ec, rec, play, 0, NULL) != PJ_SUCCESS) rc = -4;
        }
        pjmedia_echo_destroy(ec);
    }
    pj_pool_release(pool);
    if (algo && algo_len > 0) {
        snprintf(algo, (size_t)algo_len, "%s @ %u Hz", PV_EC_ALGO == PJMEDIA_ECHO_WEBRTC ? "WebRTC AEC (legacy)" : "WebRTC AEC3", rate);
    }
    return rc;
}
