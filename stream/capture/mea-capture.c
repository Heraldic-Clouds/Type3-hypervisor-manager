/*
 * mea-capture: GPU-resident desktop capture and hardware encoding for the
 * MEA Stream agent.
 *
 *   NvFBC (NVIDIA Capture SDK, X11 backend, CUDA interop, NV12 on the GPU)
 *     -> cuMemcpy2D device-to-device into a registered NVENC input surface
 *     -> NVENC (H.264 / HEVC / AV1, ultra-low-latency tuning, CBR, no B-frames)
 *     -> framed access units on stdout
 *
 * No framebuffer pixel ever crosses into system memory.
 *
 * Build: see Makefile (needs NvFBC.h from the Capture SDK and the MIT-licensed
 * ffnvcodec headers). Libraries are loaded at run time from the NVIDIA driver
 * (libnvidia-fbc.so.1, libcuda.so.1, libnvidia-encode.so.1), so nothing
 * proprietary is linked or redistributed.
 *
 * Output frame: len:u32be | type:u8 | flags:u8 | pts_us:u64be |
 *               capture_us:u32be | encode_us:u32be | data
 *   len counts everything after itself. type 1 = access unit
 *   (flags bit0 keyframe, bit1 = unchanged frame skipped, no data),
 *   type 2 = JSON status.
 * Input: JSON lines on stdin: {"t":"bitrate","kbps":N} {"t":"fps","fps":N}
 *   {"t":"keyframe"} {"t":"suspend"} {"t":"resume"} {"t":"quit"}
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <getopt.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <ffnvcodec/dynlink_loader.h>
#include <NvFBC.h>

#define KEEPALIVE_US 200000 /* 5 fps when the screen does not change */

typedef struct {
    const char *codec;
    int width, height, fps, kbps;
    const char *display;
} Options;

static CudaFunctions *cu;
static NvencFunctions *nvencLib;
static NV_ENCODE_API_FUNCTION_LIST nv = { NV_ENCODE_API_FUNCTION_LIST_VER };
static NVFBC_API_FUNCTION_LIST fbc = { 0 };
static CUcontext cuCtx;

static void logf_(const char *fmt, ...)
{
    va_list ap;
    va_start(ap, fmt);
    fprintf(stderr, "mea-capture: ");
    vfprintf(stderr, fmt, ap);
    fputc('\n', stderr);
    va_end(ap);
}

static uint64_t now_us(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000u + ts.tv_nsec / 1000;
}

static void put32(uint8_t *p, uint32_t v) { p[0] = v >> 24; p[1] = v >> 16; p[2] = v >> 8; p[3] = v; }
static void put64(uint8_t *p, uint64_t v) { put32(p, v >> 32); put32(p + 4, (uint32_t)v); }

static int write_all(const void *buf, size_t n)
{
    const uint8_t *p = buf;
    while (n > 0) {
        ssize_t w = write(STDOUT_FILENO, p, n);
        if (w < 0) {
            if (errno == EINTR)
                continue;
            return -1;
        }
        p += w;
        n -= (size_t)w;
    }
    return 0;
}

static int emit(uint8_t type, uint8_t flags, uint64_t pts, uint32_t cap, uint32_t enc,
                const void *data, uint32_t size)
{
    uint8_t h[22];
    put32(h, 18 + size);
    h[4] = type;
    h[5] = flags;
    put64(h + 6, pts);
    put32(h + 14, cap);
    put32(h + 18, enc);
    if (write_all(h, sizeof h) < 0)
        return -1;
    return size ? write_all(data, size) : 0;
}

static void status(const char *json)
{
    emit(2, 0, 0, 0, 0, json, (uint32_t)strlen(json));
}

/* ---------- CUDA / NVENC ---------- */

static int cuda_init(void)
{
    CUdevice dev;
    if (cuda_load_functions(&cu, NULL) < 0) {
        logf_("cannot load libcuda");
        return -1;
    }
    if (cu->cuInit(0) != CUDA_SUCCESS || cu->cuDeviceGet(&dev, 0) != CUDA_SUCCESS ||
        cu->cuCtxCreate(&cuCtx, CU_CTX_SCHED_BLOCKING_SYNC, dev) != CUDA_SUCCESS) {
        logf_("CUDA context creation failed");
        return -1;
    }
    return 0;
}

static int nvenc_load(void)
{
    uint32_t max = 0;
    if (nvenc_load_functions(&nvencLib, NULL) < 0) {
        logf_("cannot load libnvidia-encode");
        return -1;
    }
    nvencLib->NvEncodeAPIGetMaxSupportedVersion(&max);
    if (((NVENCAPI_MAJOR_VERSION << 4) | NVENCAPI_MINOR_VERSION) > max) {
        logf_("driver supports NVENC API %u.%u, need %d.%d", max >> 4, max & 15,
              NVENCAPI_MAJOR_VERSION, NVENCAPI_MINOR_VERSION);
        return -1;
    }
    if (nvencLib->NvEncodeAPICreateInstance(&nv) != NV_ENC_SUCCESS) {
        logf_("NvEncodeAPICreateInstance failed");
        return -1;
    }
    return 0;
}

static void *nvenc_open(void)
{
    NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS p = { NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS_VER };
    void *enc = NULL;
    p.deviceType = NV_ENC_DEVICE_TYPE_CUDA;
    p.device = cuCtx;
    p.apiVersion = NVENCAPI_VERSION;
    if (nv.nvEncOpenEncodeSessionEx(&p, &enc) != NV_ENC_SUCCESS) {
        logf_("nvEncOpenEncodeSessionEx failed (NVENC session limit or no GPU)");
        return NULL;
    }
    return enc;
}

static int guid_eq(GUID a, GUID b) { return memcmp(&a, &b, sizeof a) == 0; }

static GUID codec_guid(const char *c)
{
    if (!strcmp(c, "h265"))
        return NV_ENC_CODEC_HEVC_GUID;
    if (!strcmp(c, "av1"))
        return NV_ENC_CODEC_AV1_GUID;
    return NV_ENC_CODEC_H264_GUID;
}

/* Prints {"codecs":[...],"fbc":true|false} for media.Native.Codecs(). */
static int probe(void)
{
    char out[256] = "{\"codecs\":[";
    int n = 0, fbcOK = 0;
    void *enc;
    if (cuda_init() < 0 || nvenc_load() < 0 || !(enc = nvenc_open())) {
        puts("{\"codecs\":[]}");
        return 1;
    }
    uint32_t count = 0, got = 0;
    GUID guids[16];
    nv.nvEncGetEncodeGUIDCount(enc, &count);
    nv.nvEncGetEncodeGUIDs(enc, guids, 16, &got);
    const char *names[] = { "h264", "h265", "av1" };
    for (int i = 0; i < 3; i++) {
        for (uint32_t j = 0; j < got; j++) {
            if (guid_eq(guids[j], codec_guid(names[i]))) {
                strcat(out, n++ ? ",\"" : "\"");
                strcat(out, names[i]);
                strcat(out, "\"");
            }
        }
    }
    nv.nvEncDestroyEncoder(enc);

    /* NvFBC must also be usable (driver, licence-eligible GPU, X server). */
    void *lib = dlopen("libnvidia-fbc.so.1", RTLD_NOW);
    PNVFBCCREATEINSTANCE create = lib ? (PNVFBCCREATEINSTANCE)dlsym(lib, "NvFBCCreateInstance") : NULL;
    if (create) {
        fbc.dwVersion = NVFBC_VERSION;
        if (create(&fbc) == NVFBC_SUCCESS) {
            NVFBC_SESSION_HANDLE h;
            NVFBC_CREATE_HANDLE_PARAMS hp = { .dwVersion = NVFBC_CREATE_HANDLE_PARAMS_VER };
            if (fbc.nvFBCCreateHandle(&h, &hp) == NVFBC_SUCCESS) {
                NVFBC_GET_STATUS_PARAMS sp = { .dwVersion = NVFBC_GET_STATUS_PARAMS_VER };
                fbcOK = fbc.nvFBCGetStatus(h, &sp) == NVFBC_SUCCESS && sp.bIsCapturePossible;
                NVFBC_DESTROY_HANDLE_PARAMS dp = { .dwVersion = NVFBC_DESTROY_HANDLE_PARAMS_VER };
                fbc.nvFBCDestroyHandle(h, &dp);
            }
        }
    }
    if (!fbcOK)
        strcpy(out, "{\"codecs\":[");
    strcat(out, "]");
    strcat(out, fbcOK ? ",\"fbc\":true}" : ",\"fbc\":false}");
    puts(out);
    return fbcOK ? 0 : 1;
}

typedef struct {
    void *enc;
    NV_ENC_INITIALIZE_PARAMS init;
    NV_ENC_CONFIG cfg;
    NV_ENC_OUTPUT_PTR bitstream;
    CUdeviceptr surface;
    size_t pitch;
    NV_ENC_REGISTERED_PTR registered;
    int width, height;
} Encoder;

static void set_rate(Encoder *e, int kbps, int fps)
{
    NV_ENC_RC_PARAMS *rc = &e->cfg.rcParams;
    rc->rateControlMode = NV_ENC_PARAMS_RC_CBR;
    rc->averageBitRate = (uint32_t)kbps * 1000u;
    rc->maxBitRate = rc->averageBitRate;
    /* single-frame VBV: every frame fits in one frame interval on the wire */
    rc->vbvBufferSize = rc->averageBitRate / (uint32_t)fps;
    rc->vbvInitialDelay = rc->vbvBufferSize;
    rc->zeroReorderDelay = 1;
    rc->enableLookahead = 0;
    rc->lowDelayKeyFrameScale = 1;
    rc->multiPass = NV_ENC_MULTI_PASS_DISABLED;
    e->init.frameRateNum = (uint32_t)fps;
    e->init.frameRateDen = 1;
}

static int encoder_init(Encoder *e, const Options *o)
{
    memset(e, 0, sizeof *e);
    if (!(e->enc = nvenc_open()))
        return -1;
    GUID codec = codec_guid(o->codec);
    NV_ENC_PRESET_CONFIG preset = { NV_ENC_PRESET_CONFIG_VER, { NV_ENC_CONFIG_VER } };
    if (nv.nvEncGetEncodePresetConfigEx(e->enc, codec, NV_ENC_PRESET_P1_GUID,
                                        NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY, &preset) != NV_ENC_SUCCESS) {
        logf_("codec %s not supported by this GPU", o->codec);
        return -1;
    }
    e->cfg = preset.presetCfg;
    e->cfg.version = NV_ENC_CONFIG_VER;
    e->cfg.gopLength = NVENC_INFINITE_GOPLENGTH; /* IDR only on request */
    e->cfg.frameIntervalP = 1;                    /* no B-frames */
    if (guid_eq(codec, NV_ENC_CODEC_H264_GUID)) {
        /* constrained baseline matches the SDP the gateway negotiates */
        e->cfg.profileGUID = NV_ENC_H264_PROFILE_BASELINE_GUID;
        e->cfg.encodeCodecConfig.h264Config.idrPeriod = NVENC_INFINITE_GOPLENGTH;
        e->cfg.encodeCodecConfig.h264Config.repeatSPSPPS = 1;
    } else if (guid_eq(codec, NV_ENC_CODEC_HEVC_GUID)) {
        e->cfg.encodeCodecConfig.hevcConfig.idrPeriod = NVENC_INFINITE_GOPLENGTH;
        e->cfg.encodeCodecConfig.hevcConfig.repeatSPSPPS = 1;
    } else {
        e->cfg.encodeCodecConfig.av1Config.idrPeriod = NVENC_INFINITE_GOPLENGTH;
        e->cfg.encodeCodecConfig.av1Config.repeatSeqHdr = 1;
    }
    e->init.version = NV_ENC_INITIALIZE_PARAMS_VER;
    e->init.encodeGUID = codec;
    e->init.presetGUID = NV_ENC_PRESET_P1_GUID;
    e->init.tuningInfo = NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY;
    e->init.encodeWidth = e->init.darWidth = e->init.maxEncodeWidth = (uint32_t)o->width;
    e->init.encodeHeight = e->init.darHeight = e->init.maxEncodeHeight = (uint32_t)o->height;
    e->init.enablePTD = 1;
    e->init.encodeConfig = &e->cfg;
    set_rate(e, o->kbps, o->fps);
    if (nv.nvEncInitializeEncoder(e->enc, &e->init) != NV_ENC_SUCCESS) {
        logf_("nvEncInitializeEncoder failed");
        return -1;
    }
    NV_ENC_CREATE_BITSTREAM_BUFFER bb = { NV_ENC_CREATE_BITSTREAM_BUFFER_VER };
    if (nv.nvEncCreateBitstreamBuffer(e->enc, &bb) != NV_ENC_SUCCESS)
        return -1;
    e->bitstream = bb.bitstreamBuffer;

    /* Our own pitched NV12 surface, registered with NVENC once. */
    e->width = o->width;
    e->height = o->height;
    if (cu->cuMemAllocPitch(&e->surface, &e->pitch, (size_t)o->width, (size_t)o->height * 3 / 2, 16) != CUDA_SUCCESS)
        return -1;
    NV_ENC_REGISTER_RESOURCE rr = { NV_ENC_REGISTER_RESOURCE_VER };
    rr.resourceType = NV_ENC_INPUT_RESOURCE_TYPE_CUDADEVICEPTR;
    rr.resourceToRegister = (void *)e->surface;
    rr.width = (uint32_t)o->width;
    rr.height = (uint32_t)o->height;
    rr.pitch = (uint32_t)e->pitch;
    rr.bufferFormat = NV_ENC_BUFFER_FORMAT_NV12;
    rr.bufferUsage = NV_ENC_INPUT_IMAGE;
    if (nv.nvEncRegisterResource(e->enc, &rr) != NV_ENC_SUCCESS) {
        logf_("nvEncRegisterResource failed");
        return -1;
    }
    e->registered = rr.registeredResource;
    return 0;
}

static int encoder_reconfigure(Encoder *e, int kbps, int fps)
{
    set_rate(e, kbps, fps);
    NV_ENC_RECONFIGURE_PARAMS r = { NV_ENC_RECONFIGURE_PARAMS_VER };
    r.reInitEncodeParams = e->init;
    r.reInitEncodeParams.encodeConfig = &e->cfg;
    if (nv.nvEncReconfigureEncoder(e->enc, &r) != NV_ENC_SUCCESS) {
        logf_("reconfigure to %d kbps / %d fps failed", kbps, fps);
        return -1;
    }
    return 0;
}

/* Copies the NvFBC NV12 frame (tightly packed) into the encoder surface and
 * encodes it. Returns bytes written or -1. */
static int encode(Encoder *e, CUdeviceptr src, uint64_t pts, int idr, uint32_t capUS)
{
    uint64_t t0 = now_us();
    CUDA_MEMCPY2D cp = { 0 };
    cp.srcMemoryType = CU_MEMORYTYPE_DEVICE;
    cp.srcDevice = src;
    cp.srcPitch = (size_t)e->width;
    cp.dstMemoryType = CU_MEMORYTYPE_DEVICE;
    cp.dstDevice = e->surface;
    cp.dstPitch = e->pitch;
    cp.WidthInBytes = (size_t)e->width;
    cp.Height = (size_t)e->height * 3 / 2;
    if (cu->cuMemcpy2D(&cp) != CUDA_SUCCESS)
        return -1;

    NV_ENC_MAP_INPUT_RESOURCE map = { NV_ENC_MAP_INPUT_RESOURCE_VER };
    map.registeredResource = e->registered;
    if (nv.nvEncMapInputResource(e->enc, &map) != NV_ENC_SUCCESS)
        return -1;
    NV_ENC_PIC_PARAMS pic = { NV_ENC_PIC_PARAMS_VER };
    pic.inputWidth = (uint32_t)e->width;
    pic.inputHeight = (uint32_t)e->height;
    pic.inputPitch = (uint32_t)e->pitch;
    pic.inputBuffer = map.mappedResource;
    pic.bufferFmt = map.mappedBufferFmt;
    pic.outputBitstream = e->bitstream;
    pic.pictureStruct = NV_ENC_PIC_STRUCT_FRAME;
    pic.inputTimeStamp = pts;
    if (idr)
        pic.encodePicFlags = NV_ENC_PIC_FLAG_FORCEIDR | NV_ENC_PIC_FLAG_OUTPUT_SPSPPS;
    NVENCSTATUS st = nv.nvEncEncodePicture(e->enc, &pic);
    int ret = -1;
    if (st == NV_ENC_SUCCESS) {
        NV_ENC_LOCK_BITSTREAM lock = { NV_ENC_LOCK_BITSTREAM_VER };
        lock.outputBitstream = e->bitstream;
        if (nv.nvEncLockBitstream(e->enc, &lock) == NV_ENC_SUCCESS) {
            int key = lock.pictureType == NV_ENC_PIC_TYPE_IDR || lock.pictureType == NV_ENC_PIC_TYPE_I;
            uint32_t encUS = (uint32_t)(now_us() - t0);
            ret = emit(1, key ? 1 : 0, pts, capUS, encUS, lock.bitstreamBufferPtr, lock.bitstreamSizeInBytes) < 0
                      ? -2 : (int)lock.bitstreamSizeInBytes;
            nv.nvEncUnlockBitstream(e->enc, e->bitstream);
        }
    } else {
        logf_("nvEncEncodePicture: %d", st);
    }
    nv.nvEncUnmapInputResource(e->enc, map.mappedResource);
    return ret;
}

static void encoder_destroy(Encoder *e)
{
    if (!e->enc)
        return;
    NV_ENC_PIC_PARAMS eos = { NV_ENC_PIC_PARAMS_VER };
    eos.encodePicFlags = NV_ENC_PIC_FLAG_EOS;
    nv.nvEncEncodePicture(e->enc, &eos);
    if (e->registered)
        nv.nvEncUnregisterResource(e->enc, e->registered);
    if (e->bitstream)
        nv.nvEncDestroyBitstreamBuffer(e->enc, e->bitstream);
    nv.nvEncDestroyEncoder(e->enc);
    if (e->surface)
        cu->cuMemFree(e->surface);
    memset(e, 0, sizeof *e);
}

/* ---------- NvFBC ---------- */

typedef struct {
    NVFBC_SESSION_HANDLE h;
    int open;
} Capture;

static const char *fbc_err(Capture *c)
{
    return fbc.nvFBCGetLastErrorStr ? fbc.nvFBCGetLastErrorStr(c->h) : "?";
}

static int capture_open(Capture *c, const Options *o)
{
    void *lib = dlopen("libnvidia-fbc.so.1", RTLD_NOW);
    if (!lib) {
        logf_("libnvidia-fbc.so.1 not found (driver without NvFBC)");
        return -1;
    }
    PNVFBCCREATEINSTANCE create = (PNVFBCCREATEINSTANCE)dlsym(lib, "NvFBCCreateInstance");
    fbc.dwVersion = NVFBC_VERSION;
    if (!create || create(&fbc) != NVFBC_SUCCESS) {
        logf_("NvFBCCreateInstance failed (driver/SDK version mismatch?)");
        return -1;
    }
    NVFBC_CREATE_HANDLE_PARAMS hp = { .dwVersion = NVFBC_CREATE_HANDLE_PARAMS_VER };
    hp.eBackend = NVFBC_BACKEND_X11; /* the container's own X server only */
    if (fbc.nvFBCCreateHandle(&c->h, &hp) != NVFBC_SUCCESS) {
        logf_("nvFBCCreateHandle: %s", fbc_err(c));
        return -1;
    }
    NVFBC_GET_STATUS_PARAMS sp = { .dwVersion = NVFBC_GET_STATUS_PARAMS_VER };
    if (fbc.nvFBCGetStatus(c->h, &sp) != NVFBC_SUCCESS || !sp.bIsCapturePossible || !sp.bCanCreateNow) {
        logf_("capture not possible: %s", fbc_err(c));
        return -1;
    }
    NVFBC_CREATE_CAPTURE_SESSION_PARAMS cp = { .dwVersion = NVFBC_CREATE_CAPTURE_SESSION_PARAMS_VER };
    cp.eCaptureType = NVFBC_CAPTURE_SHARED_CUDA;
    cp.eTrackingType = NVFBC_TRACKING_SCREEN;
    cp.bWithCursor = NVFBC_TRUE;
    cp.frameSize.w = (uint32_t)o->width; /* NvFBC scales on the GPU */
    cp.frameSize.h = (uint32_t)o->height;
    cp.dwSamplingRateMs = (uint32_t)(1000 / o->fps);
    if (fbc.nvFBCCreateCaptureSession(c->h, &cp) != NVFBC_SUCCESS) {
        logf_("nvFBCCreateCaptureSession: %s", fbc_err(c));
        return -1;
    }
    NVFBC_TOCUDA_SETUP_PARAMS su = { .dwVersion = NVFBC_TOCUDA_SETUP_PARAMS_VER };
    su.eBufferFormat = NVFBC_BUFFER_FORMAT_NV12;
    if (fbc.nvFBCToCudaSetUp(c->h, &su) != NVFBC_SUCCESS) {
        logf_("nvFBCToCudaSetUp: %s", fbc_err(c));
        return -1;
    }
    c->open = 1;
    return 0;
}

static void capture_close(Capture *c)
{
    if (!c->open)
        return;
    NVFBC_DESTROY_CAPTURE_SESSION_PARAMS d = { .dwVersion = NVFBC_DESTROY_CAPTURE_SESSION_PARAMS_VER };
    fbc.nvFBCDestroyCaptureSession(c->h, &d);
    NVFBC_DESTROY_HANDLE_PARAMS dh = { .dwVersion = NVFBC_DESTROY_HANDLE_PARAMS_VER };
    fbc.nvFBCDestroyHandle(c->h, &dh);
    c->open = 0;
}

/* ---------- control ---------- */

typedef struct {
    int kbps, fps, keyframe, suspended, quit, changed;
} Control;

static int json_int(const char *line, const char *key, int *out)
{
    char pat[32];
    snprintf(pat, sizeof pat, "\"%s\":", key);
    const char *p = strstr(line, pat);
    if (!p)
        return 0;
    *out = atoi(p + strlen(pat));
    return 1;
}

static void read_control(Control *c)
{
    static char buf[4096];
    static size_t len;
    struct pollfd pfd = { STDIN_FILENO, POLLIN, 0 };
    while (poll(&pfd, 1, 0) > 0) {
        if (pfd.revents & (POLLHUP | POLLERR) && !(pfd.revents & POLLIN)) {
            c->quit = 1; /* the agent went away */
            return;
        }
        ssize_t n = read(STDIN_FILENO, buf + len, sizeof buf - 1 - len);
        if (n <= 0) {
            c->quit = 1;
            return;
        }
        len += (size_t)n;
        buf[len] = 0;
        char *nl;
        while ((nl = strchr(buf, '\n'))) {
            *nl = 0;
            int v;
            if (strstr(buf, "\"bitrate\"") && json_int(buf, "kbps", &v) && v >= 200 && v <= 100000) {
                c->kbps = v;
                c->changed = 1;
            } else if (strstr(buf, "\"fps\"") && json_int(buf, "fps", &v) && v >= 5 && v <= 240) {
                c->fps = v;
                c->changed = 1;
            } else if (strstr(buf, "\"keyframe\"")) {
                c->keyframe = 1;
            } else if (strstr(buf, "\"suspend\"")) {
                c->suspended = 1;
            } else if (strstr(buf, "\"resume\"")) {
                c->suspended = 0;
                c->keyframe = 1;
            } else if (strstr(buf, "\"quit\"")) {
                c->quit = 1;
            }
            len -= (size_t)(nl + 1 - buf);
            memmove(buf, nl + 1, len + 1);
        }
        if (len >= sizeof buf - 1)
            len = 0; /* oversized line: drop */
    }
}

static void sleep_until(uint64_t t)
{
    uint64_t n = now_us();
    if (t > n) {
        struct timespec ts = { (time_t)((t - n) / 1000000), (long)((t - n) % 1000000) * 1000 };
        nanosleep(&ts, NULL);
    }
}

static int run(Options *o)
{
    Encoder enc;
    Capture cap = { 0 };
    Control ctl = { .kbps = o->kbps, .fps = o->fps, .keyframe = 1 };
    if (o->display && *o->display)
        setenv("DISPLAY", o->display, 1);
    if (cuda_init() < 0 || nvenc_load() < 0)
        return 1;
    if (capture_open(&cap, o) < 0)
        return 2;
    if (encoder_init(&enc, o) < 0)
        return 3;
    status("{\"state\":\"running\",\"backend\":\"nvfbc-x11-cuda\",\"encoder\":\"nvenc\"}");

    uint64_t start = now_us(), next = start, lastSent = 0;
    while (!ctl.quit) {
        read_control(&ctl);
        if (ctl.changed) {
            encoder_reconfigure(&enc, ctl.kbps, ctl.fps);
            ctl.changed = 0;
        }
        if (ctl.suspended) {
            usleep(50000);
            next = now_us();
            continue;
        }
        uint64_t interval = 1000000u / (uint64_t)ctl.fps;
        sleep_until(next);
        next += interval;
        if (now_us() > next + interval)
            next = now_us(); /* fell behind: do not burst */

        uint64_t t0 = now_us();
        CUdeviceptr frame = 0;
        NVFBC_FRAME_GRAB_INFO info = { 0 };
        NVFBC_TOCUDA_GRAB_FRAME_PARAMS gp = { .dwVersion = NVFBC_TOCUDA_GRAB_FRAME_PARAMS_VER };
        gp.dwFlags = NVFBC_TOCUDA_GRAB_FLAGS_NOWAIT;
        gp.pCUDADeviceBuffer = &frame;
        gp.pFrameGrabInfo = &info;
        NVFBCSTATUS st = fbc.nvFBCToCudaGrabFrame(cap.h, &gp);
        if (st == NVFBC_ERR_MUST_RECREATE) {
            logf_("display mode changed, recreating capture session");
            capture_close(&cap);
            if (capture_open(&cap, o) < 0)
                return 2;
            ctl.keyframe = 1;
            continue;
        }
        if (st != NVFBC_SUCCESS) {
            logf_("grab failed: %s", fbc_err(&cap));
            return 2;
        }
        uint32_t capUS = (uint32_t)(now_us() - t0);
        uint64_t pts = now_us() - start;
        int due = ctl.keyframe || info.bIsNewFrame || pts - lastSent >= KEEPALIVE_US;
        if (!due) {
            emit(1, 2, pts, capUS, 0, NULL, 0); /* unchanged: skipped */
            continue;
        }
        int r = encode(&enc, frame, pts, ctl.keyframe, capUS);
        if (r == -2)
            break; /* stdout closed */
        if (r < 0) {
            logf_("encode failed, reinitialising NVENC");
            encoder_destroy(&enc);
            o->kbps = ctl.kbps;
            o->fps = ctl.fps;
            if (encoder_init(&enc, o) < 0)
                return 3;
            ctl.keyframe = 1;
            continue;
        }
        ctl.keyframe = 0;
        lastSent = pts;
    }
    encoder_destroy(&enc);
    capture_close(&cap);
    return 0;
}

int main(int argc, char **argv)
{
    Options o = { "h264", 1920, 1080, 60, 8000, NULL };
    static struct option opts[] = {
        { "codec", required_argument, 0, 'c' }, { "width", required_argument, 0, 'w' },
        { "height", required_argument, 0, 'H' }, { "fps", required_argument, 0, 'f' },
        { "kbps", required_argument, 0, 'k' }, { "display", required_argument, 0, 'd' },
        { "probe", no_argument, 0, 'p' }, { 0, 0, 0, 0 },
    };
    int c;
    signal(SIGPIPE, SIG_IGN);
    while ((c = getopt_long(argc, argv, "", opts, NULL)) != -1) {
        switch (c) {
        case 'c': o.codec = optarg; break;
        case 'w': o.width = atoi(optarg) & ~1; break;
        case 'H': o.height = atoi(optarg) & ~1; break;
        case 'f': o.fps = atoi(optarg); break;
        case 'k': o.kbps = atoi(optarg); break;
        case 'd': o.display = optarg; break;
        case 'p': return probe();
        default: return 64;
        }
    }
    if (o.width < 64 || o.height < 64 || o.width > 7680 || o.height > 4320 || o.fps < 5 || o.fps > 240 ||
        o.kbps < 200 || (strcmp(o.codec, "h264") && strcmp(o.codec, "h265") && strcmp(o.codec, "av1"))) {
        logf_("invalid arguments");
        return 64;
    }
    return run(&o);
}
