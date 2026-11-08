package api

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/emicklei/go-restful/v3"

	"github.com/xbapps/xbvr/pkg/config"
	"github.com/xbapps/xbvr/pkg/models"
	"github.com/xbapps/xbvr/pkg/transcode"
)

func transcodeOptions() transcode.Options {
	c := config.Config.Interfaces.Heresphere
	return transcode.Options{
		BitrateMbps: c.TranscodeBitrateMbps,
		AudioKbps:   c.TranscodeAudioKbps,
		MaxHeight:   c.TranscodeMaxHeight,
	}
}

// heresphereTranscodeMedia describes the live transcode streams as media entries, one per
// enabled codec. A live stream has no file, so its size is the estimated byte count at the
// configured bitrates.
func heresphereTranscodeMedia(req *restful.Request, file models.File, prefix, dnt string) []HeresphereMedia {
	c := config.Config.Interfaces.Heresphere
	if !c.TranscodeEnabled || file.VideoDuration <= 0 {
		return nil
	}
	codecs := transcode.ParseCodecs(c.TranscodeCodecs)
	if len(codecs) == 0 {
		codecs = []string{transcode.CodecH264}
	}
	opts := transcodeOptions()
	size := opts.VirtualSize(file.VideoDuration)
	if size <= 0 {
		return nil
	}

	var media []HeresphereMedia
	for _, codec := range codecs {
		plan := transcode.PlanFor(codec, opts, file.VideoWidth, file.VideoHeight, file.VideoAvgFrameRateVal)
		height, width := plan.Height, plan.Width
		if height <= 0 {
			height = file.VideoHeight
		}
		if width <= 0 {
			width = file.VideoWidth
		}
		resolution := strconv.Itoa(height)
		if file.VideoProjection == "360_tb" {
			resolution = strconv.Itoa(height / 2)
		}
		name := fmt.Sprintf("%s %s %vp - %v", prefix, transcode.CodecLabel(codec), resolution, humanize.Bytes(uint64(size)))
		if len(plan.Notes) > 0 {
			name += " (" + strings.Join(plan.Notes, "; ") + ")"
		}
		params := "?codec=" + url.QueryEscape(codec)
		if dnt != "" {
			params += "&dnt=true"
		}
		media = append(media, HeresphereMedia{
			Name: name,
			Sources: []HeresphereSource{{
				Resolution: StringOrInt(resolution),
				Height:     height,
				Width:      width,
				Size:       size,
				URL:        fmt.Sprintf("%v://%v/api/dms/transcode/%v%v", getProto(req), req.Request.Host, file.ID, params),
			}},
		})
	}
	return media
}
