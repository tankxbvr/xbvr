package llmdraft

// These tests import XBVR's models package, whose init opens a database in the app directory.
// Point that somewhere disposable:
//
//	XBVR_APPDIR=$(mktemp -d) go test ./pkg/llmdraft/

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xbapps/xbvr/pkg/llmscrape"
	"github.com/xbapps/xbvr/pkg/models"
)

func TestBuildScrapedSceneWithoutCoverLeavesCoversNil(t *testing.T) {
	page := &llmscrape.Page{URL: "https://examplevr.com/scene/9", Title: "llmscrape.Page title"}
	ext := &llmscrape.Extraction{Site: "ExampleVR", CoverImage: -1, Trailer: -1}
	scene := BuildScrapedScene(page, ext, nil)

	// The scene import reads Covers[0] whenever Covers is non-nil, so an empty slice would panic.
	// The draft is stored as JSON, so the round trip must preserve nil too.
	var d models.DraftScene
	if err := d.SetScene(scene); err != nil {
		t.Fatal(err)
	}
	back, err := d.Scene()
	if err != nil {
		t.Fatal(err)
	}
	if back.Covers != nil {
		t.Fatalf("Covers = %#v after a JSON round trip; must be nil when there is no cover", back.Covers)
	}
	if scene.Title != "llmscrape.Page title" {
		t.Errorf("empty extracted title should fall back to the page title, got %q", scene.Title)
	}
}

func TestBuildScrapedScene(t *testing.T) {
	page := &llmscrape.Page{
		URL:          "https://examplevr.com/scene/1234?ref=x",
		CanonicalURL: "https://examplevr.com/scene/1234",
		Images:       []llmscrape.Candidate{{URL: "https://c/cover.jpg"}, {URL: "https://c/s1.jpg"}, {URL: "https://c/s2.jpg"}},
		Videos:       []llmscrape.Candidate{{URL: "https://c/trailer.mp4"}},
	}
	ext := &llmscrape.Extraction{
		Title: "Sweet Pink Bunny", Studio: "Ethernal VR", Site: "", SiteSceneID: "1234",
		Cast: []string{"Cleo Vega"}, Tags: []string{"POV"}, Released: "2025-03-03", DurationMinutes: 40,
		CoverImage: 0, GalleryImages: []int{2, 1}, Trailer: 0,
	}
	s := BuildScrapedScene(page, ext, &llmscrape.FileInfo{Filename: "cleo-vega-sweet-pink-bunny.mp4"})

	if s.SceneID != "llm-ethernal-vr-1234" {
		t.Errorf("SceneID = %q", s.SceneID)
	}
	if s.Site != "Ethernal VR" || s.Studio != "Ethernal VR" {
		t.Errorf("site/studio = %q/%q; site should fall back to the studio", s.Site, s.Studio)
	}
	if s.HomepageURL != "https://examplevr.com/scene/1234" {
		t.Errorf("homepage should prefer the canonical URL, got %q", s.HomepageURL)
	}
	if len(s.Covers) != 1 || s.Covers[0] != "https://c/cover.jpg" {
		t.Errorf("covers = %v", s.Covers)
	}
	if strings.Join(s.Gallery, ",") != "https://c/s2.jpg,https://c/s1.jpg" {
		t.Errorf("gallery should keep the model's order, got %v", s.Gallery)
	}
	if s.TrailerType != "url" || s.TrailerSrc != "https://c/trailer.mp4" {
		t.Errorf("trailer = %q %q", s.TrailerType, s.TrailerSrc)
	}
	if len(s.Filenames) != 1 || s.Filenames[0] != "cleo-vega-sweet-pink-bunny.mp4" {
		t.Errorf("filenames = %v", s.Filenames)
	}
	if s.Duration != 40 || s.Released != "2025-03-03" || s.SceneType != "VR" {
		t.Errorf("duration/released/type = %d %q %q", s.Duration, s.Released, s.SceneType)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
}

func TestSceneIDIsStableWithoutSiteID(t *testing.T) {
	page := &llmscrape.Page{URL: "https://examplevr.com/a", CanonicalURL: "https://examplevr.com/scene/abc"}
	a := BuildScrapedScene(page, &llmscrape.Extraction{Site: "ExampleVR", CoverImage: -1, Trailer: -1}, nil)
	b := BuildScrapedScene(page, &llmscrape.Extraction{Site: "ExampleVR", CoverImage: -1, Trailer: -1}, nil)
	if a.SceneID != b.SceneID {
		t.Errorf("scraping the same page twice gave %q and %q", a.SceneID, b.SceneID)
	}
	if !strings.HasPrefix(a.SceneID, "llm-examplevr-") {
		t.Errorf("SceneID = %q", a.SceneID)
	}
	other := &llmscrape.Page{URL: "https://examplevr.com/scene/xyz"}
	c := BuildScrapedScene(other, &llmscrape.Extraction{Site: "ExampleVR", CoverImage: -1, Trailer: -1}, nil)
	if c.SceneID == a.SceneID {
		t.Error("different pages must not share a SceneID")
	}
}

func TestDomainBlocked(t *testing.T) {
	domains := ParseDomains("hqcollect.is, www.vrpornx.net\nexample.org")
	if len(domains) != 3 || domains[1] != "vrpornx.net" {
		t.Fatalf("ParseDomains = %v", domains)
	}
	cases := map[string]bool{
		"https://hqcollect.is/siterip/1":     true,
		"https://www.vrpornx.net/x":          true,
		"https://cdn.example.org/y":          true,
		"https://notexample.org/z":           false,
		"https://dezyred.com/games/slutty":   false,
		"https://hqcollect.is.evil.com/path": false,
	}
	for u, want := range cases {
		if got := DomainBlocked(u, domains); got != want {
			t.Errorf("DomainBlocked(%s) = %v, want %v", u, got, want)
		}
	}
}

func TestSiteComesFromStudioOnAggregatorPages(t *testing.T) {
	page := &llmscrape.Page{URL: "https://vrporn.com/cleo-vega-sweet-pink-bunny/"}
	agg := BuildScrapedScene(page, &llmscrape.Extraction{PageKind: llmscrape.KindStore, Site: "VRPorn.com",
		Studio: "Ethernal VR", CoverImage: -1, Trailer: -1}, nil)
	if agg.Site != "Ethernal VR" {
		t.Errorf("aggregator page: site = %q, want the studio", agg.Site)
	}

	own := BuildScrapedScene(page, &llmscrape.Extraction{PageKind: llmscrape.KindOfficial, Site: "SLR Originals",
		Studio: "SexLikeReal", CoverImage: -1, Trailer: -1}, nil)
	if own.Site != "SLR Originals" || own.Studio != "SexLikeReal" {
		t.Errorf("official page: site/studio = %q/%q; the page's own site name should be kept", own.Site, own.Studio)
	}
}

func TestBrandName(t *testing.T) {
	cases := map[string]string{"Dezyred.com": "Dezyred", "VRPorn.com": "VRPorn", "Ethernal VR": "Ethernal VR",
		"cdn.example.com": "cdn.example.com", " RealJamVR ": "RealJamVR", "VR-Bangers.net": "VR-Bangers"}
	for in, want := range cases {
		if got := brandName(in); got != want {
			t.Errorf("brandName(%q) = %q, want %q", in, got, want)
		}
	}
}
