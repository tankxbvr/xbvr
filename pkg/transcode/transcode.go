// Package transcode builds live ffmpeg encodes for players that cannot take the source file
// directly. A live encode has no file on disk, so it also estimates how many bytes the stream
// will produce for clients that display a file size.
package transcode

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Codecs a client can be offered. The first three use NVENC; x264 is a CPU fallback.
const (
	CodecH264 = "h264"
	CodecHEVC = "hevc"
	CodecAV1  = "av1"
	CodecX264 = "x264"
)

var codecOrder = []string{CodecH264, CodecHEVC, CodecAV1, CodecX264}

// Codecs returns the supported codec names in display order.
func Codecs() []string { return append([]string(nil), codecOrder...) }

// IsCodec reports whether name is a supported codec.
func IsCodec(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case CodecH264, CodecHEVC, CodecAV1, CodecX264:
		return true
	}
	return false
}

// ParseCodecs normalises and deduplicates a codec list, dropping unknown names.
func ParseCodecs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range in {
		name = strings.ToLower(strings.TrimSpace(name))
		if !IsCodec(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// CodecLabel is the name shown to clients.
func CodecLabel(codec string) string {
	switch strings.ToLower(codec) {
	case CodecHEVC:
		return "HEVC (NVENC)"
	case CodecAV1:
		return "AV1 (NVENC)"
	case CodecX264:
		return "H.264 (x264)"
	default:
		return "H.264 (NVENC)"
	}
}

// Options describe a live encode.
type Options struct {
	BitrateMbps int // target video bitrate
	AudioKbps   int // target audio bitrate
	MaxHeight   int // cap the output height; 0 keeps the source resolution
}

func (o Options) normalized() Options {
	if o.BitrateMbps <= 0 {
		o.BitrateMbps = 20
	}
	if o.AudioKbps <= 0 {
		o.AudioKbps = 128
	}
	if o.MaxHeight < 0 {
		o.MaxHeight = 0
	}
	return o
}

// capability is what one codec can encode. NVENC's H.264 encoder is limited to 4096x4096;
// HEVC and AV1 reach 8192x8192. PixelBudget is a conservative pixels-per-second estimate for
// a fast preset, used to decide whether the source frame rate fits.
type capability struct {
	maxWidth, maxHeight int
	pixelBudget         float64
}

var capabilities = map[string]capability{
	CodecH264: {4096, 4096, 2.0e9},
	CodecHEVC: {8192, 8192, 2.0e9},
	CodecAV1:  {8192, 8192, 1.9e9},
	CodecX264: {8192, 8192, 0.9e9},
}

// Plan is the resolved output for one codec: the size it will encode at, the frame rate it
// will cap to, and any user-facing notes about the adjustments.
type Plan struct {
	Codec  string
	Width  int
	Height int
	FPS    float64 // 0 = keep the source frame rate
	Scale  bool
	Notes  []string
}

// PlanFor works out how a codec handles a source. Sources larger than the codec's limit are
// scaled down to fit; a frame rate the codec cannot sustain is limited to 30 fps, which is
// reported in Notes so the client can see why. srcFPS may be 0 when unknown.
func PlanFor(codec string, opts Options, srcWidth, srcHeight int, srcFPS float64) Plan {
	o := opts.normalized()
	if !IsCodec(codec) {
		codec = CodecH264
	}
	cap := capabilities[codec]
	p := Plan{Codec: codec}

	width, height := srcWidth, srcHeight
	if width > 0 && height > 0 {
		if o.MaxHeight > 0 && height > o.MaxHeight {
			width = int(float64(width) * float64(o.MaxHeight) / float64(height))
			height = o.MaxHeight
			p.Notes = append(p.Notes, fmt.Sprintf("height capped at %dp", o.MaxHeight))
		}
		if cap.maxWidth > 0 && (width > cap.maxWidth || height > cap.maxHeight) {
			factor := math.Min(float64(cap.maxWidth)/float64(width), float64(cap.maxHeight)/float64(height))
			fromW, fromH := width, height
			width = int(float64(width) * factor)
			height = int(float64(height) * factor)
			p.Notes = append(p.Notes, fmt.Sprintf("scaled from %dx%d; %s tops out at %dx%d",
				fromW, fromH, CodecLabel(codec), cap.maxWidth, cap.maxHeight))
		}
		width, height = even(width), even(height)
		p.Width, p.Height = width, height
		p.Scale = width != srcWidth || height != srcHeight
	}

	target := math.Round(srcFPS*100) / 100
	if target > 60 {
		target = 60
		p.Notes = append(p.Notes, "limited to 60 fps")
	}
	if target > 0 && width > 0 && height > 0 {
		maxFPS := cap.pixelBudget / float64(width*height)
		if target > maxFPS && target > 30 {
			p.Notes = append(p.Notes, fmt.Sprintf("%.0f fps is too much for %s at %dx%d; using 30 fps",
				srcFPS, CodecLabel(codec), width, height))
			target = 30
		} else if target > maxFPS {
			p.Notes = append(p.Notes, fmt.Sprintf("%s may not sustain %.0f fps at %dx%d",
				CodecLabel(codec), target, width, height))
		}
	}
	if target > 0 && target != math.Round(srcFPS*100)/100 {
		p.FPS = target
	}
	return p
}

// preset picks a preset fast enough for the output pixel rate. Large frames need the fastest
// presets to hold 60 fps; smaller ones can afford better quality.
func (p Plan) preset() string {
	fps := p.FPS
	if fps <= 0 {
		fps = 30
	}
	rate := float64(p.Width) * float64(p.Height) * fps
	switch {
	case rate > 1.2e9:
		return "p1"
	case rate > 0.8e9:
		return "p2"
	case rate > 0.4e9:
		return "p3"
	default:
		return "p4"
	}
}

// VirtualSize estimates the bytes a live encode of durationSeconds will produce at the
// configured bitrates.
func (o Options) VirtualSize(durationSeconds float64) int64 {
	o = o.normalized()
	if durationSeconds <= 0 {
		return 0
	}
	bitsPerSecond := float64(o.BitrateMbps)*1_000_000 + float64(o.AudioKbps)*1000
	return int64(durationSeconds * bitsPerSecond / 8)
}

func even(n int) int {
	if n > 0 && n%2 == 1 {
		return n - 1
	}
	return n
}

// Args returns the ffmpeg arguments that stream path as fragmented MP4 on stdout, starting at
// seekSeconds. -re paces the encode at playback speed, so a long file is not encoded ahead of
// the player.
func Args(ffmpegPath, path string, seekSeconds float64, opts Options, p Plan) []string {
	o := opts.normalized()
	videoKbps := o.BitrateMbps * 1000

	args := []string{ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin"}
	if seekSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(seekSeconds, 'f', 3, 64))
	}
	args = append(args, "-re", "-i", path)

	switch p.Codec {
	case CodecX264:
		args = append(args, "-c:v", "libx264", "-preset", "veryfast")
	case CodecHEVC:
		args = append(args, "-c:v", "hevc_nvenc", "-preset", p.preset(), "-rc", "vbr", "-cq", "23")
	case CodecAV1:
		args = append(args, "-c:v", "av1_nvenc", "-preset", p.preset(), "-rc", "vbr", "-cq", "23")
	default:
		args = append(args, "-c:v", "h264_nvenc", "-preset", p.preset(), "-rc", "vbr", "-cq", "23")
	}

	args = append(args,
		"-b:v", fmt.Sprintf("%dk", videoKbps),
		"-maxrate", fmt.Sprintf("%dk", videoKbps),
		"-bufsize", fmt.Sprintf("%dk", videoKbps*2),
	)
	if p.Scale && p.Width > 0 && p.Height > 0 {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", p.Width, p.Height))
	}
	if p.FPS > 0 {
		args = append(args, "-r", strconv.FormatFloat(p.FPS, 'f', 3, 64))
	}
	args = append(args,
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", o.AudioKbps), "-ac", "2",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return args
}
