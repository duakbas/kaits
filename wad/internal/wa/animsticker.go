package wa

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Animated stickers, turned into something this phone can actually animate.
//
// A still sticker becomes a PNG (see webp.go). An animated one cannot: WebP
// animation is a sequence of VP8 frames in ANMF chunks, x/image decodes neither
// animations nor anything but a single still frame, and there is no pure-Go
// decoder for it worth trusting with a message stream.
//
// But this browser does animate GIFs — it is a 2016 engine, and GIF is from
// 1989. So the conversion goes through ffmpeg, which is the one tool that
// reliably reads animated WebP, and the daemon is the right place for it: it
// has the bytes, it has a real CPU, and the phone has neither.
//
// What comes out is deliberately small. A sticker is 512x512 at whatever frame
// rate the author chose, and a GIF of that is several hundred kilobytes — every
// frame of which this phone decodes into memory, on a device that kills
// whichever background app is largest. 160 pixels and 12 frames a second is
// about a tenth of that and indistinguishable in a bubble.
//
// Transparency survives, but only just: GIF has one transparent colour rather
// than an alpha channel, so a soft edge becomes a hard one. On a dark UI that
// reads as intended. The alternative — compositing onto the app's background —
// would break the moment anything behind it changed.

const (
	// gifMaxSide is the longest edge of the converted GIF. Bubbles show
	// stickers at 120 CSS pixels, so 160 leaves room for the viewer without
	// paying for a frame nobody looks at closely.
	gifMaxSide = 160
	// gifFPS: WhatsApp stickers are commonly 15-25fps. Twelve keeps motion
	// smooth and drops a third to a half of the frames.
	gifFPS = 12
	// gifTimeout bounds a conversion. ffmpeg on a pathological input can run
	// for a long time, and this happens inline on an HTTP request.
	gifTimeout = 20 * time.Second
	// gifMaxBytes refuses a result too big to be worth sending. A sticker that
	// converts to more than this is one the phone would struggle to decode.
	gifMaxBytes = 1 << 20
)

// ffmpegOnce caches whether ffmpeg exists, since the answer cannot change
// while the process runs and the lookup happens per sticker otherwise.
var (
	ffmpegOnce sync.Once
	ffmpegPath string
)

// HaveFFmpeg reports whether animated stickers can be converted at all. When
// they can't, the caller falls back to the still preview WhatsApp ships inside
// every sticker message — a static sticker rather than a broken one.
func HaveFFmpeg() bool {
	ffmpegOnce.Do(func() {
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = p
		}
	})
	return ffmpegPath != ""
}

// AnimatedStickerGIF converts an animated WebP to a small looping GIF.
//
// Returns ok=false for anything it cannot convert, and the caller is expected
// to fall back rather than fail.
func AnimatedStickerGIF(ctx context.Context, webp []byte) (gif []byte, ok bool) {
	if !HaveFFmpeg() || len(webp) == 0 {
		return nil, false
	}

	dir, err := os.MkdirTemp("", "wadsticker")
	if err != nil {
		return nil, false
	}
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, "in.webp")
	out := filepath.Join(dir, "out.gif")
	if err := os.WriteFile(in, webp, 0600); err != nil {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(ctx, gifTimeout)
	defer cancel()

	// Two passes in one graph: palettegen looks at the whole clip so colours
	// don't shift between frames, and reserve_transparent keeps a slot for the
	// transparent colour rather than spending all 256 on the picture.
	filter := "[0:v]fps=" + itoa(gifFPS) +
		",scale=" + itoa(gifMaxSide) + ":" + itoa(gifMaxSide) +
		":force_original_aspect_ratio=decrease:flags=lanczos,split[a][b];" +
		"[a]palettegen=reserve_transparent=1[p];" +
		"[b][p]paletteuse=alpha_threshold=128"

	cmd := exec.CommandContext(ctx, ffmpegPath,
		"-hide_banner", "-loglevel", "error",
		"-y", "-i", in,
		"-filter_complex", filter,
		"-loop", "0",
		out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Expected for a still WebP handed here by mistake, or a file ffmpeg
		// doesn't like. Logged at all because a systematic failure — a broken
		// ffmpeg build, say — would otherwise look like "stickers don't move".
		log.Printf("sticker gif: %v: %s", err, trimLog(stderr.String()))
		return nil, false
	}

	data, err := os.ReadFile(out)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	if len(data) > gifMaxBytes {
		log.Printf("sticker gif: %d bytes is too big to send, falling back to the still", len(data))
		return nil, false
	}
	return data, true
}

func trimLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
