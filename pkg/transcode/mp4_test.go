package transcode

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func testBox(typ string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	size := uint32(8 + len(body))
	b := make([]byte, 8, size)
	binary.BigEndian.PutUint32(b[:4], size)
	copy(b[4:8], typ)
	return append(b, body...)
}

func testFullBox(typ string, version byte, payload []byte) []byte {
	return testBox(typ, append([]byte{version, 0, 0, 0}, payload...))
}

func testDurationBox(typ string, version byte, timescale uint32, durationOffset int) []byte {
	payload := make([]byte, durationOffset+4)
	payload[0] = version
	if version == 0 {
		binary.BigEndian.PutUint32(payload[12:16], timescale)
	}
	return testBox(typ, payload)
}

func durationField(t *testing.T, b []byte, typ string, offset int) uint32 {
	t.Helper()
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b[:4]))
		name := string(b[4:8])
		if size < 8 || size > len(b) {
			t.Fatalf("bad box %q size %d", name, size)
		}
		if name == typ {
			if len(b) < offset+4 {
				t.Fatalf("%s too short", typ)
			}
			return binary.BigEndian.Uint32(b[offset : offset+4])
		}
		switch name {
		case "moov", "trak", "mdia", "mvex":
			if v := findDurationField(t, b[8:size], typ, offset); v != 0 {
				return v
			}
		}
		b = b[size:]
	}
	return 0
}

func findDurationField(t *testing.T, b []byte, typ string, offset int) uint32 {
	t.Helper()
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b[:4]))
		name := string(b[4:8])
		if size < 8 || size > len(b) {
			t.Fatalf("bad box %q size %d", name, size)
		}
		if name == typ {
			return binary.BigEndian.Uint32(b[offset : offset+4])
		}
		switch name {
		case "trak", "mdia", "mvex":
			if v := findDurationField(t, b[8:size], typ, offset); v != 0 {
				return v
			}
		}
		b = b[size:]
	}
	return 0
}

func TestReadMP4HeaderSetsDurations(t *testing.T) {
	// mvhd: version/flags, creation, modification, timescale, duration
	mvhd := testFullBox("mvhd", 0, func() []byte {
		p := make([]byte, 16)
		binary.BigEndian.PutUint32(p[8:12], 1000)
		return p
	}())
	// tkhd: version/flags, creation, modification, track_ID, reserved, duration
	tkhd := testFullBox("tkhd", 0, func() []byte {
		p := make([]byte, 20)
		binary.BigEndian.PutUint32(p[8:12], 1)
		return p
	}())
	// mdhd: version/flags, creation, modification, timescale, duration
	mdhd := testFullBox("mdhd", 0, func() []byte {
		p := make([]byte, 16)
		binary.BigEndian.PutUint32(p[8:12], 15360)
		return p
	}())
	mehd := testFullBox("mehd", 0, make([]byte, 4))
	moov := testBox("moov", mvhd, testBox("trak", tkhd, testBox("mdia", mdhd)), testBox("mvex", mehd))
	stream := testBox("ftyp", []byte("iso5iso6mp41")) // 16 bytes

	header, err := ReadMP4Header(bytes.NewReader(append(stream, moov...)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(header) != len(stream)+len(moov) {
		t.Fatalf("header length = %d, want %d", len(header), len(stream)+len(moov))
	}
	if got := durationField(t, header, "mvhd", 8+4+12); got != 10_000 {
		t.Errorf("mvhd duration = %d, want 10000", got)
	}
	if got := durationField(t, header, "tkhd", 8+4+16); got != 10_000 {
		t.Errorf("tkhd duration = %d, want 10000", got)
	}
	if got := durationField(t, header, "mdhd", 8+4+12); got != 153_600 {
		t.Errorf("mdhd duration = %d, want 153600", got)
	}
	if got := durationField(t, header, "mehd", 8+4); got != 10_000 {
		t.Errorf("mehd duration = %d, want 10000", got)
	}
}

func TestReadMP4HeaderRejectsMissingMoov(t *testing.T) {
	stream := testBox("ftyp", []byte("iso5iso6mp41"))
	stream = append(stream, testBox("mdat", make([]byte, 12))...)
	if _, err := ReadMP4Header(bytes.NewReader(stream), 10); err == nil {
		t.Fatal("a stream without moov should be rejected")
	}
}
