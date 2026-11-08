// Package transcode builds live ffmpeg encodes for players that cannot take the source file
// directly. A live encode has no file on disk, so it also estimates how many bytes the stream
// will produce for clients that display a file size.
package transcode

import (
	"fmt"
	"math"
	"os/exec"
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
	Codec       string
	Width       int
	Height      int
	FPS         float64 // 0 = keep the source frame rate
	Scale       bool
	Unavailable bool
	Notes       []string
}

// vramNeedMiB estimates the encoder's VRAM for an output size. The figures come from
// measurements on Blackwell: roughly 0.7 GB at 1080p, 0.7-1.1 GB at 4K, and about 1.1 GB
// for HEVC at 8K. AV1 at 8K needs well over 1.8 GB and is not viable on a card with less
// free memory. x264 encodes on the CPU and uses no VRAM.
func vramNeedMiB(codec string, width, height int) int {
	if codec == CodecX264 || width <= 0 || height <= 0 {
		return 0
	}
	mp := float64(width) * float64(height) / 1e6
	switch {
	case height <= 1080:
		return int(450 + mp*120)
	case height <= 2160:
		switch codec {
		case CodecAV1:
			return int(600 + mp*60)
		case CodecHEVC:
			return int(450 + mp*30)
		default:
			return int(500 + mp*55)
		}
	default:
		switch codec {
		case CodecAV1:
			return int(900 + mp*60)
		case CodecHEVC:
			return int(650 + mp*15)
		default:
			return int(650 + mp*30)
		}
	}
}

const vramSafetyMiB = 256

// fitsVRAM reports whether an encode of this size fits in freeVRAMMiB. A free value of 0
// means the free memory is unknown, in which case no judgement is made.
func fitsVRAM(codec string, width, height, freeVRAMMiB int) bool {
	if freeVRAMMiB <= 0 {
		return true
	}
	return vramNeedMiB(codec, width, height)+vramSafetyMiB <= freeVRAMMiB
}

// scaleToHeight fits width x height inside a target height, keeping aspect and even sides.
func scaleToHeight(width, height, targetHeight int) (int, int) {
	if height <= 0 || width <= 0 || height <= targetHeight {
		return width, height
	}
	return even(int(float64(width) * float64(targetHeight) / float64(height))), even(targetHeight)
}

// FreeVRAM returns the free memory of the visible GPU with the most room and its index.
// ffmpeg defaults to GPU 0, so the index is passed to it. Unknown or unavailable values
// are returned as 0, 0.
func FreeVRAM() (freeMiB, gpuIndex int) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=index,memory.free", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0
	}
	best := -1
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			continue
		}
		idx, err1 := strconv.Atoi(strings.TrimSpace(fields[0]))
		free, err2 := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err1 != nil || err2 != nil {
			continue
		}
		if free > best {
			best, gpuIndex, freeMiB = free, idx, free
		}
	}
	if best < 0 {
		return 0, 0
	}
	return freeMiB, gpuIndex
}

// PlanFor works out how a codec handles a source. Sources larger than the codec's limit are
// scaled down to fit; if freeVRAMMiB is known and the encode would not fit, the output is
// scaled down until it does, or marked unavailable. A frame rate the codec cannot sustain is
// limited to 30 fps. Both adjustments are reported in Notes so the client can see why.
// srcFPS may be 0 when unknown; freeVRAMMiB may be 0 when unknown.
func PlanFor(codec string, opts Options, srcWidth, srcHeight int, srcFPS float64, freeVRAMMiB int) Plan {
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

		if !fitsVRAM(codec, width, height, freeVRAMMiB) {
			originalW, originalH := width, height
			fitted := false
			for _, target := range []int{2160, 1440, 1080, 720} {
				w, h := scaleToHeight(originalW, originalH, target)
				if w == originalW && h == originalH {
					continue
				}
				if fitsVRAM(codec, w, h, freeVRAMMiB) {
					width, height = w, h
					fitted = true
					p.Notes = append(p.Notes, fmt.Sprintf("scaled to %dx%d to fit %d MiB free VRAM", w, h, freeVRAMMiB))
					break
				}
			}
			if !fitted {
				p.Unavailable = true
				p.Notes = append(p.Notes, fmt.Sprintf("not enough free VRAM for %s at this size (%d MiB free)", CodecLabel(codec), freeVRAMMiB))
			}
		}

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
func Args(ffmpegPath, path string, seekSeconds float64, opts Options, p Plan, gpuIndex int) []string {
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
	if p.Codec != CodecX264 && gpuIndex >= 0 {
		args = append(args, "-gpu", strconv.Itoa(gpuIndex))
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
