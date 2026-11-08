package transcode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// maxMP4Header bounds the prefix buffered while looking for the moov box. A fragmented MP4
// writer emits ftyp and moov before the first fragment, so this is far larger than needed.
const maxMP4Header = 8 << 20

// ReadMP4Header reads the ftyp and moov boxes from a fragmented MP4 stream and writes the
// known duration into every movie, track and media duration field. A fragmented MP4 normally
// carries zero in those fields, which makes players report a live stream with no length and
// disable seeking. The returned bytes must be written before the rest of r is copied.
func ReadMP4Header(r io.Reader, durationSeconds float64) ([]byte, error) {
	if durationSeconds <= 0 {
		return nil, errors.New("mp4: duration must be positive")
	}

	var prefix []byte
	for len(prefix) < maxMP4Header {
		size, typ, header, err := readBoxHeader(r)
		if err != nil {
			return prefix, err
		}
		if size < int64(len(header)) || size > maxMP4Header {
			return prefix, fmt.Errorf("mp4: invalid %q box size %d", typ, size)
		}
		body := make([]byte, size-int64(len(header)))
		if _, err := io.ReadFull(r, body); err != nil {
			return prefix, err
		}
		start := len(prefix)
		prefix = append(prefix, header...)
		prefix = append(prefix, body...)

		switch typ {
		case "moov":
			patchMoov(prefix[start:], durationSeconds)
			return prefix, nil
		case "ftyp", "free", "skip", "wide":
			// Boxes that can precede moov in a fragmented MP4.
		default:
			return prefix, fmt.Errorf("mp4: unexpected %q box before moov", typ)
		}
	}
	return prefix, errors.New("mp4: moov box not found")
}

// readBoxHeader reads an MP4 box header and returns the total box size, its type and the exact
// header bytes (including the 64-bit size when size == 1).
func readBoxHeader(r io.Reader) (int64, string, []byte, error) {
	head := make([]byte, 8)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, "", nil, err
	}
	size := int64(binary.BigEndian.Uint32(head[:4]))
	typ := string(head[4:])
	if size == 1 {
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return 0, "", nil, err
		}
		size = int64(binary.BigEndian.Uint64(ext))
		head = append(head, ext...)
	}
	return size, typ, head, nil
}

// patchMoov walks a moov box and rewrites the duration fields it finds. It returns the movie
// timescale so tkhd, which counts in movie time, can be written too.
func patchMoov(moov []byte, seconds float64) uint32 {
	var movieTimescale uint32
	walkBoxes(moov[8:], func(typ string, payload []byte) {
		if typ == "mvhd" {
			if ts := patchTimeHeader(payload, seconds, 0); ts > 0 {
				movieTimescale = ts
			}
		}
	})
	// The first pass above fixes mvhd; tkhd needs the timescale from it, so walk again once
	// the value is known.
	walkBoxes(moov[8:], func(typ string, payload []byte) {
		switch typ {
		case "tkhd":
			patchTkhd(payload, seconds, movieTimescale)
		case "mdhd":
			patchTimeHeader(payload, seconds, 0)
		case "mehd":
			patchMehd(payload, seconds, movieTimescale)
		}
	})
	return movieTimescale
}

// walkBoxes calls fn for every box at this level and recurses into the containers that hold
// the headers we care about.
func walkBoxes(b []byte, fn func(typ string, payload []byte)) {
	for len(b) >= 8 {
		size := int64(binary.BigEndian.Uint32(b[:4]))
		typ := string(b[4:8])
		hdr := int64(8)
		if size == 1 {
			if len(b) < 16 {
				return
			}
			size = int64(binary.BigEndian.Uint64(b[8:16]))
			hdr = 16
		}
		if size < hdr || size > int64(len(b)) {
			return
		}
		payload := b[hdr:size]
		fn(typ, payload)
		switch typ {
		case "trak", "mdia", "minf", "stbl", "mvex", "edts", "dinf", "udta":
			walkBoxes(payload, fn)
		}
		b = b[size:]
	}
}

// patchTimeHeader writes seconds into an mvhd, tkhd or mdhd payload. The duration unit is the
// box's own timescale; fallback is used when the box has no timescale of its own (tkhd).
func patchTimeHeader(payload []byte, seconds float64, fallback uint32) uint32 {
	if len(payload) < 4 {
		return 0
	}
	version := payload[0]
	switch version {
	case 0:
		// creation, modification, timescale, duration
		if len(payload) < 20 {
			return 0
		}
		timescale := binary.BigEndian.Uint32(payload[12:16])
		if timescale == 0 {
			timescale = fallback
		}
		if timescale == 0 {
			return 0
		}
		binary.BigEndian.PutUint32(payload[16:20], durationUnits(seconds, timescale))
		return timescale
	case 1:
		// creation, modification, timescale, duration (64-bit)
		if len(payload) < 32 {
			return 0
		}
		timescale := binary.BigEndian.Uint32(payload[20:24])
		if timescale == 0 {
			timescale = fallback
		}
		if timescale == 0 {
			return 0
		}
		binary.BigEndian.PutUint64(payload[24:32], durationUnits64(seconds, timescale))
		return timescale
	}
	return 0
}

// patchTkhd writes the track duration. Unlike mvhd and mdhd, tkhd has track_ID and reserved
// fields before the duration and always counts in the movie timescale.
func patchTkhd(payload []byte, seconds float64, movieTimescale uint32) {
	if movieTimescale == 0 || len(payload) < 4 {
		return
	}
	switch payload[0] {
	case 0:
		if len(payload) >= 24 {
			binary.BigEndian.PutUint32(payload[20:24], durationUnits(seconds, movieTimescale))
		}
	case 1:
		if len(payload) >= 36 {
			binary.BigEndian.PutUint64(payload[28:36], durationUnits64(seconds, movieTimescale))
		}
	}
}

// patchMehd writes the overall fragment duration in the movie timescale.
func patchMehd(payload []byte, seconds float64, timescale uint32) {
	if timescale == 0 || len(payload) < 4 {
		return
	}
	version := payload[0]
	if version == 0 && len(payload) >= 8 {
		binary.BigEndian.PutUint32(payload[4:8], durationUnits(seconds, timescale))
	} else if version == 1 && len(payload) >= 12 {
		binary.BigEndian.PutUint64(payload[4:12], durationUnits64(seconds, timescale))
	}
}

func durationUnits(seconds float64, timescale uint32) uint32 {
	n := math.Round(seconds * float64(timescale))
	if n < 0 {
		return 0
	}
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}

func durationUnits64(seconds float64, timescale uint32) uint64 {
	n := math.Round(seconds * float64(timescale))
	if n < 0 {
		return 0
	}
	if n > math.MaxUint64 {
		return math.MaxUint64
	}
	return uint64(n)
}
