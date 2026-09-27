package llmscrape

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractionSchemaShape(t *testing.T) {
	s := ExtractionSchema(3, 1)
	props := s["properties"].(map[string]any)
	required := s["required"].([]string)
	if len(required) != len(props) {
		t.Fatalf("strict mode needs every property required: %d required, %d properties", len(required), len(props))
	}
	for _, r := range required {
		if _, ok := props[r]; !ok {
			t.Errorf("required %q is not a property", r)
		}
	}
	if s["additionalProperties"] != false {
		t.Error("additionalProperties must be false")
	}

	cover := props["cover_image"].(map[string]any)["enum"].([]any)
	if len(cover) != 4 || cover[0] != -1 || cover[3] != 2 {
		t.Errorf("cover enum = %v, want [-1 0 1 2]", cover)
	}
	trailer := props["trailer"].(map[string]any)["enum"].([]any)
	if len(trailer) != 2 {
		t.Errorf("trailer enum = %v, want [-1 0]", trailer)
	}

	// The schema has to survive JSON encoding to be sent to the server.
	if _, err := json.Marshal(s); err != nil {
		t.Fatal(err)
	}
}

func TestExtractionSchemaNoCandidates(t *testing.T) {
	s := ExtractionSchema(0, 0)
	props := s["properties"].(map[string]any)
	if e := props["cover_image"].(map[string]any)["enum"].([]any); len(e) != 1 || e[0] != -1 {
		t.Errorf("with no images the only cover choice is -1, got %v", e)
	}
	if props["gallery_images"].(map[string]any)["maxItems"] != 0 {
		t.Error("with no images the gallery must be empty")
	}
}

func TestParseExtractionRepairs(t *testing.T) {
	page := &Page{
		Images: []Candidate{{URL: "a"}, {URL: "b"}, {URL: "c"}},
		Videos: []Candidate{{URL: "v"}},
	}
	raw := json.RawMessage(`{
		"is_scene_page": true, "page_kind": "made_up_kind", "match_confidence": 1.7,
		"reason": "  looks   right  ", "title": " Title ", "studio": "S", "site": "", "site_scene_id": "",
		"cast": ["Jane Doe", " jane doe ", "", "Ann Lee"], "tags": ["VR", "vr", "POV"],
		"synopsis": "  text ", "released": "2025-02-30", "duration_minutes": 9999,
		"cover_image": 7, "gallery_images": [1, 1, -3, 2, 99, 0], "trailer": 4
	}`)
	e, err := ParseExtraction(raw, page)
	if err != nil {
		t.Fatal(err)
	}
	if e.PageKind != KindOther {
		t.Errorf("unknown page kind should become %q, got %q", KindOther, e.PageKind)
	}
	if e.MatchConfidence != 1 {
		t.Errorf("confidence should clamp to 1, got %v", e.MatchConfidence)
	}
	if e.Released != "" {
		t.Errorf("impossible date should be cleared, got %q", e.Released)
	}
	if e.DurationMinutes != 0 {
		t.Errorf("absurd duration should be cleared, got %d", e.DurationMinutes)
	}
	if e.CoverImage != -1 || e.Trailer != -1 {
		t.Errorf("out of range indexes should become -1: cover %d trailer %d", e.CoverImage, e.Trailer)
	}
	if got := e.GalleryImages; len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 0 {
		t.Errorf("gallery = %v, want [1 2 0]", got)
	}
	if len(e.Cast) != 2 || e.Cast[0] != "Jane Doe" || e.Cast[1] != "Ann Lee" {
		t.Errorf("cast = %v", e.Cast)
	}
	if len(e.Tags) != 2 {
		t.Errorf("tags should dedupe case-insensitively, got %v", e.Tags)
	}
	if e.Title != "Title" || e.Reason != "looks right" || e.Synopsis != "text" {
		t.Errorf("whitespace not cleaned: %q %q %q", e.Title, e.Reason, e.Synopsis)
	}
}

func TestParseExtractionCoverNotRepeatedInGallery(t *testing.T) {
	page := &Page{Images: []Candidate{{URL: "a"}, {URL: "b"}}}
	raw := json.RawMessage(`{"is_scene_page":true,"page_kind":"official_studio","match_confidence":0.9,"reason":"",
		"title":"","studio":"","site":"","site_scene_id":"","cast":[],"tags":[],"synopsis":"","released":"",
		"duration_minutes":0,"cover_image":0,"gallery_images":[0,1],"trailer":-1}`)
	e, err := ParseExtraction(raw, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.GalleryImages) != 1 || e.GalleryImages[0] != 1 {
		t.Errorf("cover should not also be in the gallery, got %v", e.GalleryImages)
	}
}

func TestParseExtractionRejectsWrongShape(t *testing.T) {
	if _, err := ParseExtraction(json.RawMessage(`{"cast": "not a list"}`), &Page{}); err == nil {
		t.Error("expected an error for a mistyped field")
	}
}

func TestBuildMessagesListsCandidatesByIndex(t *testing.T) {
	page := &Page{URL: "https://x/s", Title: "T", Meta: map[string]string{"og:title": "T"},
		Images: []Candidate{{URL: "https://x/a.jpg", Hint: "og:image"}}, Text: "body"}
	msgs := BuildMessages(page, &FileInfo{Filename: "x.mp4", DurationMinutes: 42, Context: "studio is X"})
	if len(msgs) != 2 || msgs[0].Role != "system" {
		t.Fatalf("messages = %+v", msgs)
	}
	u := msgs[1].Content
	for _, want := range []string{"[0] https://x/a.jpg (og:image)", "file duration: 42 minutes", "notes from the library owner: studio is X", "VIDEO CANDIDATES\n(none)"} {
		if !strings.Contains(u, want) {
			t.Errorf("prompt missing %q:\n%s", want, u)
		}
	}
}

func TestFieldGuideReachesThePrompt(t *testing.T) {
	msgs := BuildMessages(&Page{URL: "https://x", Meta: map[string]string{}}, nil)
	sys := msgs[0].Content
	// Every described field must be in the system prompt, in schema order.
	for _, want := range []string{"- is_scene_page:", "- page_kind:", "download_or_piracy: file hosts", "- title:", "'Jane Doe: Summer Heat'", "- trailer:"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	if strings.Index(sys, "- is_scene_page:") > strings.Index(sys, "- trailer:") {
		t.Error("field guide should follow schema order")
	}
}
