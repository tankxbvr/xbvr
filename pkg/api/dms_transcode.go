package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/emicklei/go-restful/v3"

	"github.com/xbapps/xbvr/pkg/config"
	"github.com/xbapps/xbvr/pkg/models"
	"github.com/xbapps/xbvr/pkg/session"
	"github.com/xbapps/xbvr/pkg/tasks"
	"github.com/xbapps/xbvr/pkg/transcode"
)

// getTranscode streams a live ffmpeg encode. Clients that show a file size are given the
// estimated size of the stream rather than a size on disk, since the encode has no file.
func (i DMSResource) getTranscode(req *restful.Request, resp *restful.Response) {
	if !config.Config.Interfaces.Heresphere.TranscodeEnabled {
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	id, err := strconv.Atoi(req.PathParameter("file-id"))
	if err != nil {
		resp.WriteHeader(http.StatusBadRequest)
		return
	}

	db, _ := models.GetDB()
	defer db.Close()
	var f models.File
	if err := db.Preload("Volume").First(&f, id).Error; err != nil || f.Volume.Type != "local" {
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	if _, err := os.Stat(f.GetPath()); err != nil {
		resp.WriteHeader(http.StatusNotFound)
		return
	}

	doNotTrack := req.QueryParameter("dnt")
	setDeoPlayerHost(req)
	session.TrackSessionFromFile(f, doNotTrack)

	opts := transcodeOptions()
	total := opts.VirtualSize(f.VideoDuration)
	if total <= 0 {
		total = f.Size
	}

	start, end, hasRange := parseByteRange(req.Request.Header.Get("Range"), total)
	seek := 0.0
	var limit int64
	h := resp.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")
	if hasRange {
		if total > 0 && f.VideoDuration > 0 {
			seek = f.VideoDuration * float64(start) / float64(total)
			if seek >= f.VideoDuration {
				seek = f.VideoDuration - 0.1
			}
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		// An open-ended range runs to the end of the stream; a bounded one stops after the
		// requested bytes. The virtual size maps a byte offset to an approximate time.
		if end < total-1 {
			limit = end - start + 1
			h.Set("Content-Length", strconv.FormatInt(limit, 10))
		}
		resp.WriteHeader(http.StatusPartialContent)
	} else {
		resp.WriteHeader(http.StatusOK)
	}

	args := transcode.Args(ffmpegBin(), f.GetPath(), seek, opts, f.VideoHeight, f.VideoWidth)
	cmd := exec.CommandContext(req.Request.Context(), args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Errorf("transcode %d: %v", f.ID, err)
		return
	}
	if err := cmd.Start(); err != nil {
		log.Errorf("transcode %d: starting ffmpeg: %v", f.ID, err)
		return
	}

	writer := &flushWriter{w: resp.ResponseWriter}
	if limit > 0 {
		io.CopyN(writer, stdout, limit)
		// The rest of the encode is no longer wanted; stop ffmpeg rather than block on it.
		cmd.Process.Kill()
	} else {
		io.Copy(writer, stdout)
	}
	if err := cmd.Wait(); err != nil && req.Request.Context().Err() == nil && limit == 0 {
		log.Warnf("transcode %d: ffmpeg: %v: %s", f.ID, err, strings.TrimSpace(stderr.String()))
	}
}

// ffmpegBin prefers the bundled binary and falls back to PATH, where a distribution ffmpeg is
// usually the build that carries GPU encoders.
func ffmpegBin() string {
	bundled := tasks.GetBinPath("ffmpeg")
	if _, err := os.Stat(bundled); err == nil {
		return bundled
	}
	return "ffmpeg"
}

// flushWriter pushes each chunk to the client so playback starts without a server buffer delay.
type flushWriter struct {
	w http.ResponseWriter
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fl, ok := fw.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// parseByteRange parses one HTTP byte range against total. ok is false when there is no usable
// range header, in which case the caller serves the whole stream.
func parseByteRange(header string, total int64) (start, end int64, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" || total <= 0 || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if i := strings.Index(spec, ","); i >= 0 {
		spec = spec[:i]
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		start = total - suffix
		if start < 0 {
			start = 0
		}
		return start, total - 1, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	if parts[1] == "" {
		return start, total - 1, true
	}
	end, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= total {
		end = total - 1
	}
	return start, end, true
}
