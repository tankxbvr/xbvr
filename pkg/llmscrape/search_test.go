package llmscrape

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBraveSearchRetriesRateLimitAndParses(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "key" {
			t.Errorf("missing API key header")
		}
		if r.URL.Query().Get("safesearch") != "off" {
			t.Errorf("safesearch = %q, want off", r.URL.Query().Get("safesearch"))
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []any{
			map[string]any{"title": "Sweet <strong>Pink</strong> Bunny", "url": "https://a/1", "description": "d"},
		}}})
	}))
	defer srv.Close()

	b := NewBraveSearch("key")
	b.endpoint = srv.URL
	b.minGap = 10 * time.Millisecond
	res, err := b.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected one retry after 429, got %d calls", calls)
	}
	if len(res) != 1 || res[0].Title != "Sweet Pink Bunny" || res[0].URL != "https://a/1" {
		t.Errorf("results = %+v", res)
	}
}

func TestBraveSearchSpacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	b := NewBraveSearch("key")
	b.endpoint = srv.URL
	b.minGap = 150 * time.Millisecond
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := b.Search(context.Background(), "q", 1); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 280*time.Millisecond {
		t.Errorf("3 requests took %v; they should be spaced at least 150ms apart", el)
	}
}

func TestBraveSearchNeedsKey(t *testing.T) {
	if _, err := NewBraveSearch("").Search(context.Background(), "q", 1); err == nil {
		t.Error("expected an error without an API key")
	}
}

func TestFilenameTerms(t *testing.T) {
	cases := map[string]string{
		"Dezyred_Adriana Chechik_Slutty House_Fantasy-Fucked_4096p_8K_LR_180.mp4": "Dezyred Adriana Chechik Slutty House Fantasy Fucked",
		"drvr-katrina lunk-welcome-home-sir-7k_180_LR.mp4":                        "drvr katrina lunk welcome home sir",
		"VirtualRealPorn_Chloe Lapiedra_My_brothers_home_8K_180x180_3dh.mp4":      "VirtualRealPorn Chloe Lapiedra My brothers home",
		"RealJamVR-Five-Stars-For-A-Rookie-Full_4096_60_LR_180.mp4":               "RealJamVR Five Stars For A Rookie Full 4096 60",
		"FPVR-AliciaWilliams-DannySteele-180-POV_8K_UHD.mp4":                      "FPVR AliciaWilliams DannySteele POV",
	}
	for in, want := range cases {
		if got := FilenameTerms(in); got != want {
			t.Errorf("FilenameTerms(%q)\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestBuildQueryPutsContextFirstAndStaysWithinLimits(t *testing.T) {
	q := BuildQuery("cleo-vega-sweet-pink-bunny.mp4", "EthernalVR")
	if q != "EthernalVR cleo vega sweet pink bunny VR" {
		t.Errorf("query = %q", q)
	}
	long := BuildQuery(strings.Repeat("word_", 80)+".mp4", "")
	if n := len(strings.Fields(long)); n > 45 {
		t.Errorf("query has %d words; Brave allows 50", n)
	}
	if !strings.HasSuffix(long, "VR") {
		t.Errorf("truncated query should still end with VR: %q", long)
	}
}
