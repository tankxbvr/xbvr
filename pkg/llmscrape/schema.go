package llmscrape

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Page kinds the LLM classifies a page as.
const (
	KindOfficial   = "official_studio"
	KindStore      = "store_or_aggregator"
	KindDownload   = "download_or_piracy"
	KindForum      = "forum_or_review"
	KindOther      = "other"
	maxGallery     = 20
	maxTags        = 40
	maxDurationMin = 600
)

var pageKinds = []string{KindOfficial, KindStore, KindDownload, KindForum, KindOther}

// Extraction is what the LLM returns about a page. Images and the trailer are indexes into the
// page's candidate lists, so the model can only pick URLs that exist on the page.
type Extraction struct {
	IsScenePage     bool     `json:"is_scene_page"`
	PageKind        string   `json:"page_kind"`
	MatchConfidence float64  `json:"match_confidence"`
	Reason          string   `json:"reason"`
	Title           string   `json:"title"`
	Studio          string   `json:"studio"`
	Site            string   `json:"site"`
	SiteSceneID     string   `json:"site_scene_id"`
	Cast            []string `json:"cast"`
	Tags            []string `json:"tags"`
	Synopsis        string   `json:"synopsis"`
	Released        string   `json:"released"`
	DurationMinutes int      `json:"duration_minutes"`
	CoverImage      int      `json:"cover_image"`
	GalleryImages   []int    `json:"gallery_images"`
	Trailer         int      `json:"trailer"`
}

// ExtractionSchema builds the JSON schema for one page. The image and trailer fields are enums of
// that page's candidate indexes, with -1 meaning none.
func ExtractionSchema(numImages, numVideos int) map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	strList := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	indexEnum := func(n int, allowNone bool) []any {
		var e []any
		if allowNone {
			e = append(e, -1)
		}
		for i := 0; i < n; i++ {
			e = append(e, i)
		}
		return e
	}

	gallery := map[string]any{
		"type":        "array",
		"description": fmt.Sprintf("Indexes of up to %d image candidates that are stills or promotional photos from this scene, best first. Exclude the cover, logos, banners and images of other scenes.", maxGallery),
		"items":       map[string]any{"type": "integer"},
	}
	if numImages > 0 {
		gallery["items"] = map[string]any{"type": "integer", "enum": indexEnum(numImages, false)}
	} else {
		gallery["maxItems"] = 0
	}

	props := map[string]any{
		"is_scene_page": map[string]any{"type": "boolean",
			"description": "True only if the page is about one specific video scene. False for search results, performer profiles, category listings, home pages and pages about a different scene."},
		"page_kind": map[string]any{"type": "string", "enum": toAny(pageKinds),
			"description": "official_studio: the website of the studio that produced the scene, whose domain belongs to that studio. store_or_aggregator: a site carrying scenes from many studios, such as a store, streaming or tube site. download_or_piracy: file hosts, siterips, torrents, direct downloads. forum_or_review: discussion or reviews."},
		"match_confidence": map[string]any{"type": "number",
			"description": "0 to 1: how confident you are that this page describes the same scene as the file being matched, judging title, performers, studio and duration together."},
		"reason":        str("One short sentence explaining match_confidence."),
		"title":         str("The scene's own title only. Strip performer names, studio or site names and resolution that prefix or suffix it: 'Jane Doe: Summer Heat' and 'Summer Heat - ExampleVR' are both 'Summer Heat'. Empty if not shown."),
		"studio":        str("The production studio. Empty if not shown."),
		"site":          str("The brand or channel the scene belongs to, usually the studio's site name. Not the website hosting this page when that is a store or aggregator carrying many studios."),
		"site_scene_id": str("The site's own identifier for this scene if it is visible in the URL or page, otherwise empty."),
		"cast":          strList("Performer names only, as full names. No roles or character names."),
		"tags":          strList(fmt.Sprintf("Genre and category tags exactly as shown on the page, at most %d.", maxTags)),
		"synopsis":      str("The scene's own description, verbatim or lightly cleaned. Ignore site boilerplate and SEO text about streaming, downloading or the website itself. Empty if there is no real description."),
		"released": map[string]any{"type": "string", "pattern": `^(\d{4}-\d{2}-\d{2})?$`,
			"description": "Release date as YYYY-MM-DD, or empty if the page does not show one."},
		"duration_minutes": map[string]any{"type": "integer",
			"description": "Running time in whole minutes, 0 if the page does not show it."},
		"cover_image": map[string]any{"type": "integer", "enum": indexEnum(numImages, true),
			"description": "Index of the image candidate that is this scene's cover or poster, -1 if none fits."},
		"gallery_images": gallery,
		"trailer": map[string]any{"type": "integer", "enum": indexEnum(numVideos, true),
			"description": "Index of the video candidate that is this scene's trailer or preview, -1 if none."},
	}

	// Strict structured output requires every property to be listed as required; absence is
	// expressed with the empty values the descriptions give instead.
	required := []string{"is_scene_page", "page_kind", "match_confidence", "reason", "title", "studio", "site",
		"site_scene_id", "cast", "tags", "synopsis", "released", "duration_minutes", "cover_image", "gallery_images", "trailer"}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// FileInfo is what the LLM is told about the file being matched.
type FileInfo struct {
	Filename        string
	DurationMinutes int
	Context         string // the user's saved notes for this file
}

const systemPrompt = `You identify adult VR video scenes from web pages for a personal media library.

Rules:
- Use only information present in the page content you are given. Never guess or invent values.
- When something is absent, use an empty string, an empty list, 0, or -1 as the field describes.
- Images and videos are chosen by index from the numbered candidate lists. Never write a URL.
- A page can describe the right scene without being a scene page (for example a performer page
  listing it); in that case is_scene_page is false.
- Judge match_confidence against the file: studio, performers and title words should agree, and
  a known file duration should be close to the page's running time.`

// FieldGuide renders the schema's field descriptions as prompt text. Constrained decoding
// enforces the schema's structure but does not show it to the model, so without this the
// descriptions would shape nothing.
func FieldGuide(schema map[string]any) string {
	props, _ := schema["properties"].(map[string]any)
	order, _ := schema["required"].([]string)
	var b strings.Builder
	for _, name := range order {
		prop, _ := props[name].(map[string]any)
		desc, _ := prop["description"].(string)
		if desc == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, desc)
	}
	return b.String()
}

// BuildMessages renders the page and file into the chat messages sent to the LLM.
func BuildMessages(page *Page, file *FileInfo) []Message {
	var b strings.Builder

	if file != nil {
		b.WriteString("FILE BEING MATCHED\n")
		fmt.Fprintf(&b, "filename: %s\n", file.Filename)
		if file.DurationMinutes > 0 {
			fmt.Fprintf(&b, "file duration: %d minutes\n", file.DurationMinutes)
		}
		if strings.TrimSpace(file.Context) != "" {
			fmt.Fprintf(&b, "notes from the library owner: %s\n", strings.TrimSpace(file.Context))
		}
		b.WriteString("\n")
	}

	b.WriteString("PAGE\n")
	fmt.Fprintf(&b, "url: %s\n", page.URL)
	if page.CanonicalURL != "" && page.CanonicalURL != page.URL {
		fmt.Fprintf(&b, "canonical url: %s\n", page.CanonicalURL)
	}
	if page.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", page.Title)
	}
	for _, k := range page.SortedMetaKeys() {
		fmt.Fprintf(&b, "meta %s: %s\n", k, page.Meta[k])
	}
	if page.StructuredData != "" {
		fmt.Fprintf(&b, "\nSTRUCTURED DATA (JSON-LD)\n%s\n", page.StructuredData)
	}
	fmt.Fprintf(&b, "\nVISIBLE TEXT\n%s\n", page.Text)

	b.WriteString("\nIMAGE CANDIDATES\n")
	if len(page.Images) == 0 {
		b.WriteString("(none)\n")
	}
	for i, c := range page.Images {
		fmt.Fprintf(&b, "[%d] %s (%s)\n", i, c.URL, c.Hint)
	}
	b.WriteString("\nVIDEO CANDIDATES\n")
	if len(page.Videos) == 0 {
		b.WriteString("(none)\n")
	}
	for i, c := range page.Videos {
		fmt.Fprintf(&b, "[%d] %s (%s)\n", i, c.URL, c.Hint)
	}

	guide := FieldGuide(ExtractionSchema(len(page.Images), len(page.Videos)))
	return []Message{
		{Role: "system", Content: systemPrompt + "\n\nFields to fill:\n" + guide},
		{Role: "user", Content: b.String()},
	}
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

var (
	boilerplateRe = regexp.MustCompile(`(?i)(available (for|to) (online )?(stream|download|watch)|` +
		`here on [a-z0-9-]+\.(com|net|org|xxx|tv)|vr porn (video|scene) featuring|` +
		`in 4k-8k virtual reality|(watch|stream|download) (it |this )?(now |today |online )?(on|at) [a-z0-9-]+\.(com|net|org|xxx|tv)|` +
		`(join|sign up) (now|today)|click here)`)
)

// splitSentences splits at . ! or ? followed by whitespace or the end, so the dot in a domain name
// such as example.com does not end a sentence.
func splitSentences(text string) []string {
	var out []string
	runes := []rune(text)
	start := 0
	for i, r := range runes {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i+1 < len(runes) && !unicode.IsSpace(runes[i+1]) {
			continue
		}
		if s := strings.TrimSpace(string(runes[start : i+1])); s != "" {
			out = append(out, s)
		}
		start = i + 1
	}
	if s := strings.TrimSpace(string(runes[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

// StripBoilerplate removes sentences that describe the website rather than the scene, which store
// and tube pages put where a description would be. Models keep these despite being told not to.
func StripBoilerplate(text string) string {
	var kept []string
	for _, sentence := range splitSentences(strings.TrimSpace(text)) {
		if sentence == "" || boilerplateRe.MatchString(sentence) {
			continue
		}
		kept = append(kept, sentence)
	}
	return strings.Join(kept, " ")
}

// ParseExtraction decodes the LLM's answer and repairs what the schema cannot guarantee: indexes
// in range, sane durations and dates, no duplicate or blank names. Servers without constrained
// decoding may return anything, so nothing is trusted.
func ParseExtraction(raw json.RawMessage, page *Page) (*Extraction, error) {
	var e Extraction
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("LLM answer does not match the extraction schema: %w", err)
	}

	e.Title = collapse(e.Title)
	e.Studio = collapse(e.Studio)
	e.Site = collapse(e.Site)
	e.SiteSceneID = collapse(e.SiteSceneID)
	e.Synopsis = StripBoilerplate(e.Synopsis)
	e.Reason = truncate(collapse(e.Reason), 300)

	if !contains(pageKinds, e.PageKind) {
		e.PageKind = KindOther
	}
	if e.MatchConfidence < 0 {
		e.MatchConfidence = 0
	}
	if e.MatchConfidence > 1 {
		e.MatchConfidence = 1
	}
	if e.DurationMinutes < 0 || e.DurationMinutes > maxDurationMin {
		e.DurationMinutes = 0
	}
	if e.Released != "" {
		if !dateRe.MatchString(e.Released) {
			e.Released = ""
		} else if _, err := time.Parse("2006-01-02", e.Released); err != nil {
			e.Released = ""
		}
	}

	e.Cast = cleanNames(e.Cast, 0)
	e.Tags = cleanNames(e.Tags, maxTags)

	if e.CoverImage < -1 || e.CoverImage >= len(page.Images) {
		e.CoverImage = -1
	}
	if e.Trailer < -1 || e.Trailer >= len(page.Videos) {
		e.Trailer = -1
	}
	seen := map[int]bool{e.CoverImage: true}
	var gallery []int
	for _, i := range e.GalleryImages {
		if i < 0 || i >= len(page.Images) || seen[i] {
			continue
		}
		seen[i] = true
		gallery = append(gallery, i)
		if len(gallery) >= maxGallery {
			break
		}
	}
	e.GalleryImages = gallery
	return &e, nil
}

func cleanNames(in []string, max int) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = collapse(s)
		key := strings.ToLower(s)
		if s == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
