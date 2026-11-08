package api

import "testing"

func TestParseByteRange(t *testing.T) {
	cases := []struct {
		header     string
		total      int64
		start, end int64
		ok         bool
	}{
		{"", 1000, 0, 0, false},
		{"bytes=0-", 1000, 0, 999, true},
		{"bytes=100-199", 1000, 100, 199, true},
		{"bytes=900-10000", 1000, 900, 999, true},
		{"bytes=-100", 1000, 900, 999, true},
		{"bytes=-5000", 1000, 0, 999, true},
		{"bytes=1000-", 1000, 0, 0, false},
		{"bytes=100-99", 1000, 0, 0, false},
		{"bytes=5", 1000, 0, 0, false},
		{"items=0-1", 1000, 0, 0, false},
		{"bytes=0-", 0, 0, 0, false},
	}
	for _, c := range cases {
		start, end, ok := parseByteRange(c.header, c.total)
		if ok != c.ok || start != c.start || end != c.end {
			t.Errorf("parseByteRange(%q, %d) = (%d, %d, %v), want (%d, %d, %v)",
				c.header, c.total, start, end, ok, c.start, c.end, c.ok)
		}
	}
}
