// mea-stream-agent runs inside one Eleet LXD container as the desktop user.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/input"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/agent/media"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/internal/env"
	"github.com/Heraldic-Clouds/Type3-hypervisor-manager/stream/token"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// lazyX11 connects to the desktop's X server on first use and reconnects if
// the session restarts, so the agent can start before the desktop does.
type lazyX11 struct {
	display string
	mu      sync.Mutex
	x       *input.X11
	retry   time.Time
}

func (l *lazyX11) get() (*input.X11, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.x != nil {
		return l.x, nil
	}
	if time.Now().Before(l.retry) {
		return nil, errors.New("x11 not ready")
	}
	x, err := input.OpenX11(l.display)
	if err != nil {
		l.retry = time.Now().Add(2 * time.Second)
		return nil, err
	}
	x.Size()
	l.x = x
	return x, nil
}

func (l *lazyX11) do(f func(*input.X11) error) error {
	x, err := l.get()
	if err != nil {
		return err
	}
	if err := f(x); err != nil {
		l.mu.Lock()
		l.x.Close()
		l.x = nil
		l.mu.Unlock()
		return err
	}
	return nil
}

func (l *lazyX11) Key(k uint32, d bool) error {
	return l.do(func(x *input.X11) error { return x.Key(k, d) })
}
func (l *lazyX11) Button(b uint8, d bool) error {
	return l.do(func(x *input.X11) error { return x.Button(b, d) })
}
func (l *lazyX11) MoveAbs(a, b uint16) error {
	return l.do(func(x *input.X11) error { return x.MoveAbs(a, b) })
}
func (l *lazyX11) MoveRel(a, b int16) error {
	return l.do(func(x *input.X11) error { return x.MoveRel(a, b) })
}
func (l *lazyX11) Wheel(a, b int16) error {
	return l.do(func(x *input.X11) error { return x.Wheel(a, b) })
}
func (l *lazyX11) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.x != nil {
		l.x.Close()
		l.x = nil
	}
	return nil
}

func run(log *slog.Logger) error {
	pub, err := token.ParsePublicKey(env.Secret("MEA_STREAM_PUBLIC_KEY"))
	if err != nil {
		return err
	}
	desktop := env.String("MEA_DESKTOP_ID", "")
	if desktop == "" {
		return errors.New("MEA_DESKTOP_ID is required")
	}
	display := env.String("DISPLAY", ":0")
	encoder := env.String("MEA_ENCODER", "auto") // auto | native | nvenc | x264

	native := &media.Native{Bin: env.String("MEA_CAPTURE_BIN", "mea-capture"), Log: log}
	useNative := encoder == "native" || (encoder == "auto" && native.Available())
	if encoder == "native" && !native.Available() {
		return errors.New("MEA_ENCODER=native but mea-capture cannot open NvFBC/NVENC")
	}
	ffEnc := ""
	if encoder == "nvenc" || encoder == "x264" {
		ffEnc = encoder
	}
	log.Info("video backend", "native", useNative, "ffmpeg", ffEnc)

	var pads input.Pads
	u := &input.UInput{}
	if err := u.Available(); err == nil {
		pads = u
	} else {
		log.Warn("gamepads disabled: /dev/uinput not available", "err", err)
	}
	inj := &input.Injector{Desktop: &lazyX11{display: display}, Pads: pads, Log: log}
	defer inj.Close()

	a := agent.New(agent.Config{
		DesktopID: desktop, PublicKey: pub, Display: display, Log: log, Injector: inj,
		NewVideo: func() media.VideoSource {
			if useNative {
				return &media.Native{Bin: native.Bin, Log: log}
			}
			return &media.FFmpeg{Encoder: ffEnc, Log: log}
		},
		NewAudio: func() media.AudioSource {
			if !env.Bool("MEA_AUDIO", true) {
				return nil
			}
			return &media.Audio{Source: env.String("MEA_AUDIO_SOURCE", ""), Log: log}
		},
	})
	ln, err := net.Listen("tcp", env.String("MEA_AGENT_LISTEN", ":47100"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("agent up", "desktop", desktop, "listen", ln.Addr().String(), "display", display)
	return a.Serve(ctx, ln)
}
