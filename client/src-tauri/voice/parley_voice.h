/*
 * Parley's narrow C interface to pjsua (pjproject's SIP client library).
 * Rust talks only to these functions, never to pjsua's own (large, version-
 * specific) structs. One call at a time; all functions are thread-safe and
 * may be called from any thread.
 */
#ifndef PARLEY_VOICE_H
#define PARLEY_VOICE_H

#ifdef __cplusplus
extern "C" {
#endif

enum pv_event_kind {
    PV_EV_LOG = 0,        /* text = log line, a = level */
    PV_EV_CALL_STATE = 1, /* a = pv_call_state, b = last SIP status code, text = reason */
    PV_EV_MEDIA = 2,      /* a = 1 media active, 0 inactive/error */
};

enum pv_call_state {
    PV_CALL_IDLE = 0,
    PV_CALL_CALLING = 1,
    PV_CALL_CONNECTING = 2,
    PV_CALL_CONFIRMED = 3,
    PV_CALL_DISCONNECTED = 4,
};

typedef void (*pv_event_cb)(void *ctx, int kind, int a, int b, const char *text);

typedef struct pv_options {
    int log_level;      /* 0-6; 3 = info */
    int null_audio;     /* 1 = no sound device (tests) */
    const char *user_agent;
} pv_options;

typedef struct pv_join_params {
    const char *room_uri;  /* sip:<room>@<domain> */
    const char *proxy;     /* sip:<host>:<port>;transport=tcp */
    const char *realm;
    const char *username;
    const char *password;
    const char *identity;  /* our SIP From user */
} pv_join_params;

typedef struct pv_stats {
    char codec[32];
    unsigned clock_rate;
    unsigned rtt_ms;            /* last RTT from RTCP, 0 if unknown */
    unsigned rx_jitter_ms;      /* mean jitter of received audio */
    unsigned rx_packets, rx_lost;
    unsigned tx_packets, tx_lost; /* tx_lost as reported by the remote */
    unsigned duration_s;
    int srtp;                   /* 1 if media is SRTP-encrypted */
} pv_stats;

typedef struct pv_device {
    char name[128];
    int inputs, outputs;
} pv_device;

/* Starts the SIP stack. Must be called once before anything else. */
int pv_init(const pv_options *opt, pv_event_cb cb, void *ctx);
/* Stops everything (hangs up). */
void pv_shutdown(void);

/* Joins a room (makes the call). 0 on success; the outcome arrives as events. */
int pv_join(const pv_join_params *p);
/* Leaves the current call, if any. */
void pv_leave(void);

void pv_set_mute(int muted);  /* stop sending microphone audio */
void pv_set_deaf(int deaf);   /* stop playing received audio */

/* Fills stats for the current call; non-zero if there is none. */
int pv_get_stats(pv_stats *out);

/* Audio devices: count, then details by index (0 <= i < count). */
int pv_device_count(void);
int pv_device_info(int index, pv_device *out);
/* Selects devices by index; -1 means the system default. */
int pv_set_devices(int capture, int playback);

/* Test hooks: loop a WAV file into the call / record what we receive. */
int pv_test_play(const char *wav_path);
int pv_test_record(const char *wav_path);

/* Runs the echo canceller pjsua uses, with the same rate and options, over a
 * couple of seconds of synthetic audio (a sound device never needs to open).
 * Writes the algorithm and rate to `algo`. 0 = ok. Needs pv_init. */
int pv_ec_selftest(char *algo, int algo_len);

#ifdef __cplusplus
}
#endif
#endif
