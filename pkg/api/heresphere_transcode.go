package api

import (
	"fmt"
	"strconv"

	"github.com/dustin/go-humanize"
	"github.com/emicklei/go-restful/v3"

	"github.com/xbapps/xbvr/pkg/config"
	"github.com/xbapps/xbvr/pkg/models"
	"github.com/xbapps/xbvr/pkg/transcode"
)

func transcodeOptions() transcode.Options {
	c := config.Config.Interfaces.Heresphere
	return transcode.Options{
		Encoder:     c.TranscodeEncoder,
		BitrateMbps: c.TranscodeBitrateMbps,
		AudioKbps:   c.TranscodeAudioKbps,
		MaxHeight:   c.TranscodeMaxHeight,
	}
}

// heresphereTranscodeMedia describes the live transcode stream as a media entry. A live stream
// has no file, so its size is the estimated byte count at the configured bitrates.
func heresphereTranscodeMedia(req *restful.Request, file models.File, prefix, dnt string) *HeresphereMedia {
	c := config.Config.Interfaces.Heresphere
	if !c.TranscodeEnabled || file.VideoDuration <= 0 {
		return nil
	}
	opts := transcodeOptions()
	size := opts.VirtualSize(file.VideoDuration)
	if size <= 0 {
		return nil
	}
	height, width := opts.Dimensions(file.VideoHeight, file.VideoWidth)
	resolution := strconv.Itoa(height)
	if file.VideoProjection == "360_tb" {
		resolution = strconv.Itoa(height / 2)
	}
	return &HeresphereMedia{
		Name: fmt.Sprintf("%s %s %vp - %v", prefix, opts.EncoderLabel(), resolution, humanize.Bytes(uint64(size))),
		Sources: []HeresphereSource{{
			Resolution: StringOrInt(resolution),
			Height:     height,
			Width:      width,
			Size:       size,
			URL:        fmt.Sprintf("%v://%v/api/dms/transcode/%v%v", getProto(req), req.Request.Host, file.ID, dnt),
		}},
	}
}
