package media

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

func needFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
}

func TestFFmpegTestPatternH264(t *testing.T) {
	needFFmpeg(t)
	f := &FFmpeg{Encoder: "x264", TestPattern: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := f.Start(ctx, Config{Codec: "h264", W: 640, H: 360, FPS: 30, Kbps: 1500}); err != nil {
		t.Fatal(err)
	}
	defer f.Stop()
	var first AccessUnit
	select {
	case first = <-f.Frames():
	case <-ctx.Done():
		t.Fatal("no frames")
	}
	if !first.Key {
		t.Fatal("first frame not a keyframe")
	}
	// keyframe must start with SPS and PPS in Annex B
	if !bytes.HasPrefix(first.Data, []byte{0, 0, 0, 1, 0x67}) || !bytes.Contains(first.Data, []byte{0, 0, 0, 1, 0x68}) {
		t.Fatalf("keyframe missing parameter sets: % x", first.Data[:16])
	}
	n := 1
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case <-f.Frames():
			n++
		case <-deadline:
			break loop
		}
	}
	// testsrc2 changes every frame, so mpdecimate must not drop it
	if n < 60 {
		t.Fatalf("only %d frames in ~3 s", n)
	}
	st := f.Stats()
	if st.FPS < 20 || st.Kbps <= 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestFFmpegReconfigureRestarts(t *testing.T) {
	needFFmpeg(t)
	f := &FFmpeg{Encoder: "x264", TestPattern: true}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f.Start(ctx, Config{Codec: "h264", W: 320, H: 240, FPS: 30, Kbps: 1000})
	defer f.Stop()
	<-f.Frames()
	f.SetBitrate(500)
	keys := 0
	deadline := time.After(6 * time.Second)
	for keys < 2 {
		select {
		case au := <-f.Frames():
			if au.Key {
				keys++
			}
		case <-deadline:
			t.Fatal("no keyframe after bitrate restart")
		}
	}
	if f.Stats().Restarts < 2 {
		t.Fatal("encoder not restarted")
	}
}

func TestAudioTestPattern(t *testing.T) {
	needFFmpeg(t)
	a := &Audio{TestPattern: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	n := 0
	timeout := time.After(2 * time.Second)
	for n < 50 {
		select {
		case p := <-a.Packets():
			if len(p.Data) == 0 {
				t.Fatal("empty opus packet")
			}
			n++
		case <-timeout:
			t.Fatalf("only %d opus packets", n)
		}
	}
}

func TestArgsNVENC(t *testing.T) {
	f := &FFmpeg{Encoder: "nvenc"}
	args := f.Args(Config{Codec: "av1", W: 1920, H: 1080, FPS: 60, Kbps: 12000, Display: ":0"})
	s := " " + joinArgs(args) + " "
	for _, want := range []string{" av1_nvenc ", " -tune ull ", " -bf 0 ", " -zerolatency 1 ", " x11grab ", " -bufsize 200k "} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func joinArgs(a []string) string {
	var b bytes.Buffer
	for i, s := range a {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}
