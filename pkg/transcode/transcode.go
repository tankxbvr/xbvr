// Package transcode builds live ffmpeg encodes for players that cannot take the source file
// directly. A live encode has no file on disk, so it also estimates how many bytes the stream
// will produce for clients that display a file size.
package transcode

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	EncoderNVENC = "nvenc"
	EncoderX264  = "x264"
)

// Options describe a live encode.
type Options struct {
	Encoder     string // "nvenc" (default) or "x264"
	BitrateMbps int    // target video bitrate
	AudioKbps   int    // target audio bitrate
	MaxHeight   int    // cap the output height; 0 keeps the source resolution
}

func (o Options) normalized() Options {
	enc := strings.ToLower(strings.TrimSpace(o.Encoder))
	if enc != EncoderX264 {
		enc = EncoderNVENC
	}
	if o.BitrateMbps <= 0 {
		o.BitrateMbps = 20
	}
	if o.AudioKbps <= 0 {
		o.AudioKbps = 128
	}
	if o.MaxHeight < 0 {
		o.MaxHeight = 0
	}
	o.Encoder = enc
	return o
}

// EncoderLabel is the short name shown to clients: NVENC or x264.
func (o Options) EncoderLabel() string {
	if o.normalized().Encoder == EncoderX264 {
		return "x264"
	}
	return "NVENC"
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

// Dimensions returns the output height and width after the optional height cap. Encoders
// require even dimensions.
func (o Options) Dimensions(srcHeight, srcWidth int) (height, width int) {
	o = o.normalized()
	height, width = srcHeight, srcWidth
	if height <= 0 || width <= 0 {
		return
	}
	if o.MaxHeight > 0 && height > o.MaxHeight {
		width = int(float64(srcWidth) * float64(o.MaxHeight) / float64(srcHeight))
		height = o.MaxHeight
	}
	return even(height), even(width)
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
func Args(ffmpegPath, path string, seekSeconds float64, opts Options, srcHeight, srcWidth int) []string {
	o := opts.normalized()
	height, width := o.Dimensions(srcHeight, srcWidth)
	videoKbps := o.BitrateMbps * 1000

	args := []string{ffmpegPath, "-hide_banner", "-loglevel", "error", "-nostdin"}
	if seekSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(seekSeconds, 'f', 3, 64))
	}
	args = append(args, "-re", "-i", path)

	switch o.Encoder {
	case EncoderX264:
		args = append(args, "-c:v", "libx264", "-preset", "veryfast")
	default:
		args = append(args, "-c:v", "h264_nvenc", "-preset", "p4", "-rc", "vbr", "-cq", "23")
	}

	args = append(args,
		"-b:v", fmt.Sprintf("%dk", videoKbps),
		"-maxrate", fmt.Sprintf("%dk", videoKbps),
		"-bufsize", fmt.Sprintf("%dk", videoKbps*2),
	)
	if o.MaxHeight > 0 && srcHeight > o.MaxHeight {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", width, height))
	}
	args = append(args,
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", o.AudioKbps), "-ac", "2",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-f", "mp4", "pipe:1",
	)
	return args
}
