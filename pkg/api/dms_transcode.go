package api

import (
	"bytes"
	"errors"
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

	enabled := transcode.ParseCodecs(config.Config.Interfaces.Heresphere.TranscodeCodecs)
	if len(enabled) == 0 {
		enabled = []string{transcode.CodecH264}
	}
	codec := strings.ToLower(strings.TrimSpace(req.QueryParameter("codec")))
	if codec == "" {
		codec = enabled[0]
	}
	allowed := false
	for _, c := range enabled {
		if c == codec {
			allowed = true
			break
		}
	}
	if !allowed {
		resp.WriteHeader(http.StatusNotFound)
		return
	}

	opts := transcodeOptions()
	hw := transcode.RefreshVRAM(transcode.Detect(ffmpegBin()))
	plan := transcode.PlanForEncoder(codec, hw, opts, f.VideoWidth, f.VideoHeight, f.VideoAvgFrameRateVal)
	if len(plan.Notes) > 0 {
		log.Infof("transcode %d %s on %s: %s", f.ID, codec, plan.Label(), strings.Join(plan.Notes, "; "))
	}
	if plan.Unavailable {
		log.Warnf("transcode %d %s: %s; refusing", f.ID, codec, strings.Join(plan.Notes, "; "))
		resp.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	total := opts.VirtualSize(f.VideoDuration)
	if total <= 0 {
		total = f.Size
	}

	start, end, hasRange := parseByteRange(req.Request.Header.Get("Range"), total)
	seek := 0.0
	if hasRange && total > 0 && f.VideoDuration > 0 {
		seek = f.VideoDuration * float64(start) / float64(total)
		if seek >= f.VideoDuration {
			seek = f.VideoDuration - 0.1
		}
	}
	// A client may ask for a new encode from a time instead of a byte range. HereSphere
	// appends start=<seconds> when it seeks a server-side transcode.
	if s, ok := queryStart(req); ok {
		// HereSphere uses seconds; a value far beyond the length is treated as milliseconds.
		if f.VideoDuration > 0 && s > f.VideoDuration*1.5 {
			s /= 1000
		}
		seek = s
		hasRange = false
		if seek >= f.VideoDuration {
			seek = f.VideoDuration - 0.1
		}
	}
	if seek < 0 {
		seek = 0
	}
	playDuration := f.VideoDuration - seek
	if playDuration <= 0 {
		playDuration = f.VideoDuration
	}
	duration := f.VideoDuration
	if duration <= 0 {
		duration = playDuration
	}
	log.Infof("transcode request %d %s (%s): range=%q start=%q seek=%.3f total=%d source=%.3f query=%q ua=%q",
		f.ID, codec, plan.Label(), req.Request.Header.Get("Range"), req.QueryParameter("start"), seek, total, f.VideoDuration,
		req.Request.URL.RawQuery, req.Request.UserAgent())

	var limit int64
	h := resp.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Duration", strconv.FormatFloat(duration, 'f', 3, 64))
	if hasRange {
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

	args := transcode.Args(ffmpegBin(), f.GetPath(), seek, opts, plan, -1)
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

	// ffmpeg's fragmented MP4 header has a zero duration, so players treat it as a live
	// stream with no length. Write the known length into the header before streaming it.
	header, headerErr := transcode.ReadMP4Header(stdout, duration)
	if headerErr != nil {
		log.Warnf("transcode %d: mp4 header: %v", f.ID, headerErr)
	}
	writer := &flushWriter{w: resp.ResponseWriter}
	written := int64(0)
	headerComplete := true
	if limit > 0 && int64(len(header)) > limit {
		header = header[:limit]
		headerComplete = false
	}
	if len(header) > 0 {
		n, _ := writer.Write(header)
		written = int64(n)
	}

	dest := io.Writer(writer)
	if limit > 0 {
		remaining := limit - written
		if remaining < 0 {
			remaining = 0
		}
		dest = &limitedWriter{w: writer, remaining: remaining}
	}
	var streamErr error
	if seek > 0 && headerErr == nil && headerComplete {
		// A range seek starts a new encode at seek. Shift its fragment timestamps to the
		// requested time so the player keeps the seek position instead of restarting at zero.
		streamErr = transcode.PatchFragmentTimes(stdout, dest, header, seek)
	} else {
		_, streamErr = io.Copy(dest, stdout)
	}
	if limit > 0 {
		// The rest of the encode is no longer wanted; stop ffmpeg rather than block on it.
		cmd.Process.Kill()
	} else if streamErr != nil && !errors.Is(streamErr, io.ErrShortWrite) && req.Request.Context().Err() == nil {
		log.Warnf("transcode %d: stream: %v", f.ID, streamErr)
	}
	if err := cmd.Wait(); err != nil && req.Request.Context().Err() == nil && limit == 0 {
		log.Warnf("transcode %d: ffmpeg: %v: %s", f.ID, err, strings.TrimSpace(stderr.String()))
	}
}

// limitedWriter stops a stream at a byte count without buffering the rest of the encode.
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, io.ErrShortWrite
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	if err == nil && l.remaining == 0 {
		err = io.ErrShortWrite
	}
	return n, err
}

// ffmpegBin prefers an ffmpeg on PATH, which in the container is the build made with NVENC,
// VAAPI, Quick Sync and x264; it falls back to the bundled download when there is none.
func ffmpegBin() string {
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path
	}
	return tasks.GetBinPath("ffmpeg")
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

// queryStart returns the last usable start value. Repeated start parameters are taken from
// the end, since a client may append its own start to a URL that already carries one.
func queryStart(req *restful.Request) (float64, bool) {
	values := req.Request.URL.Query()["start"]
	for i := len(values) - 1; i >= 0; i-- {
		if s, err := strconv.ParseFloat(values[i], 64); err == nil && s >= 0 {
			return s, true
		}
	}
	return 0, false
}
