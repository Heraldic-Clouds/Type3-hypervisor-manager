# mea-capture needs two header sets, neither of which is committed here:
#
#   NvFBC.h        NVIDIA Capture SDK 9.x (download from developer.nvidia.com/capture-sdk,
#                  requires accepting NVIDIA's licence; the header itself is MIT)
#   ffnvcodec/*.h  FFmpeg nv-codec-headers 12.1+ (MIT; Ubuntu: libffmpeg-nvenc-dev)
#
#   make CAPTURE_SDK=/opt/nvidia/Capture_Linux_v9.0.0
#
# Nothing NVIDIA-proprietary is linked: libnvidia-fbc, libcuda and
# libnvidia-encode are dlopen()ed from the installed driver at run time.

CAPTURE_SDK ?= /opt/nvidia/Capture_Linux_v9.0.0
FFNV_INC    ?= /usr/include
CFLAGS      ?= -O2 -g
CFLAGS      += -std=c11 -Wall -Wextra -Wno-missing-field-initializers -Wno-unused-parameter \
               -I$(CAPTURE_SDK)/NvFBC/inc -I$(FFNV_INC)
LDLIBS      += -ldl

mea-capture: mea-capture.c
	$(CC) $(CFLAGS) -o $@ $< $(LDLIBS)

install: mea-capture
	install -D -m 0755 mea-capture $(DESTDIR)/usr/local/bin/mea-capture

clean:
	rm -f mea-capture

.PHONY: install clean
