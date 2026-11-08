package transcode

import (
	"strings"
	"testing"
)

func TestVirtualSize(t *testing.T) {
	o := Options{BitrateMbps: 20, AudioKbps: 128}
	// 60 s at 20 Mbps + 128 kbps = 150,960,000 bytes.
	if got := o.VirtualSize(60); got != 150_960_000 {
		t.Errorf("VirtualSize(60) = %d, want 150960000", got)
	}
	if got := o.VirtualSize(0); got != 0 {
		t.Errorf("VirtualSize(0) = %d, want 0", got)
	}
}

func TestDimensions(t *testing.T) {
	o := Options{MaxHeight: 1080}
	h, w := o.Dimensions(1920, 3840)
	if h != 1080 || w != 2160 {
		t.Errorf("Dimensions = %dx%d, want 1080x2160", h, w)
	}

	o = Options{}
	if h, w := o.Dimensions(1921, 3841); h != 1920 || w != 3840 {
		t.Errorf("dimensions should be even, got %dx%d", h, w)
	}

	o = Options{MaxHeight: 2160}
	if h, w := o.Dimensions(1920, 3840); h != 1920 || w != 3840 {
		t.Errorf("a cap above the source must not scale, got %dx%d", h, w)
	}
}

func TestArgsNVENC(t *testing.T) {
	args := strings.Join(Args("ffmpeg", "/videos/scene.mp4", 42.5, Options{BitrateMbps: 20, AudioKbps: 128}, 1920, 3840), " ")
	for _, want := range []string{"-ss 42.500", "-re", "h264_nvenc", "-b:v 20000k", "-b:a 128k", "+frag_keyframe+empty_moov+default_base_moof", "pipe:1"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
}

func TestArgsX264Scales(t *testing.T) {
	args := strings.Join(Args("ffmpeg", "/videos/scene.mp4", 0, Options{Encoder: "x264", MaxHeight: 1080}, 1920, 3840), " ")
	if !strings.Contains(args, "libx264") {
		t.Errorf("x264 encoder not selected: %s", args)
	}
	if !strings.Contains(args, "scale=2160:1080") {
		t.Errorf("scale filter missing: %s", args)
	}
	if strings.Contains(args, "-ss") {
		t.Errorf("no seek was requested: %s", args)
	}
}
