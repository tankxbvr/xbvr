// Package llmdraft proposes XBVR scenes for unmatched files: it finds candidate pages with a web
// search, extracts them with llmscrape, and holds the results as draft scenes for review.
package llmdraft

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mozillazg/go-slugify"

	"github.com/xbapps/xbvr/pkg/config"
	"github.com/xbapps/xbvr/pkg/llmscrape"
	"github.com/xbapps/xbvr/pkg/models"
)

// Settings controls how search results are turned into drafts.
type Settings struct {
	MaxPageChars      int
	ResultsPerFile    int
	MinConfidence     float64
	Concurrency       int
	SkipDownloadSites bool
	BlockedDomains    []string
}

// Service is the LLM scraper wired to XBVR's database.
type Service struct {
	llm      *llmscrape.Client
	fetcher  *llmscrape.Fetcher
	searcher llmscrape.Searcher // nil when no search provider is configured
	settings Settings
}

// NewService builds a service from XBVR's configuration. Pages are fetched with userAgent.
func NewService(userAgent string) (*Service, error) {
	c := config.Config.LLMScraper
	llm, err := llmscrape.NewClient(llmscrape.LLMConfig{
		BaseURL:          c.BaseURL,
		Model:            c.Model,
		APIKey:           c.APIKey,
		DisableThinking:  c.DisableThinking,
		StructuredOutput: c.StructuredOutput,
		Timeout:          time.Duration(c.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	s := &Service{
		llm:     llm,
		fetcher: llmscrape.NewFetcher(userAgent, c.AllowPrivateNetworks, 30*time.Second),
		settings: Settings{
			MaxPageChars:      c.MaxPageChars,
			ResultsPerFile:    clampInt(c.ResultsPerFile, 1, 20, 5),
			MinConfidence:     c.MinConfidence,
			Concurrency:       clampInt(c.Concurrency, 1, 8, 2),
			SkipDownloadSites: c.SkipDownloadSites,
			BlockedDomains:    ParseDomains(c.BlockedDomains),
		},
	}
	if c.BraveAPIKey != "" {
		s.searcher = llmscrape.NewBraveSearch(c.BraveAPIKey)
	}
	return s, nil
}

func clampInt(v, lo, hi, def int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ParseDomains splits a comma or newline separated domain list.
func ParseDomains(s string) []string {
	var out []string
	for _, d := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == ';' }) {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "www."))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// DomainBlocked reports whether rawURL's host is one of domains or a subdomain of one.
func DomainBlocked(rawURL string, domains []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// ScrapeResult is the outcome for one page.
type ScrapeResult struct {
	URL     string             `json:"url"`
	Draft   *models.DraftScene `json:"draft,omitempty"`
	Skipped string             `json:"skipped,omitempty"` // why no draft was kept
	Error   string             `json:"error,omitempty"`
}

// ScrapeURL turns one page into a draft scene. keepAny is set for pages the user chose, which
// become drafts even when the LLM doubts they are scene pages; search results are filtered.
func (s *Service) ScrapeURL(ctx context.Context, rawURL string, fileID uint, file *llmscrape.FileInfo, query string, keepAny bool) ScrapeResult {
	res := ScrapeResult{URL: rawURL}
	if DomainBlocked(rawURL, s.settings.BlockedDomains) {
		res.Skipped = "domain is blocked in the LLM scraper settings"
		return res
	}

	page, err := s.fetcher.Fetch(ctx, rawURL, s.settings.MaxPageChars)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	raw, err := s.llm.Complete(ctx, llmscrape.BuildMessages(page, file), "xbvr_scene",
		llmscrape.ExtractionSchema(len(page.Images), len(page.Videos)), 4000)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	ext, err := llmscrape.ParseExtraction(raw, page)
	if err != nil {
		res.Error = err.Error()
		return res
	}

	if !keepAny {
		switch {
		case !ext.IsScenePage:
			res.Skipped = "not a page about a single scene: " + ext.Reason
			return res
		case s.settings.SkipDownloadSites && ext.PageKind == llmscrape.KindDownload:
			res.Skipped = "download or piracy site"
			return res
		}
	}

	draft := &models.DraftScene{
		FileID:          fileID,
		SourceURL:       rawURL,
		Query:           query,
		Status:          models.DraftStatusDraft,
		MatchConfidence: ext.MatchConfidence,
		PageKind:        ext.PageKind,
		Reason:          ext.Reason,
		Model:           s.llm.Model(),
	}
	if err := draft.SetScene(BuildScrapedScene(page, ext, file)); err != nil {
		res.Error = err.Error()
		return res
	}
	if err := draft.Save(); err != nil {
		res.Error = "saving draft: " + err.Error()
		return res
	}
	res.Draft = draft
	return res
}

// BuildScrapedScene converts an extraction into the structure every XBVR site scraper produces.
func BuildScrapedScene(page *llmscrape.Page, ext *llmscrape.Extraction, file *llmscrape.FileInfo) models.ScrapedScene {
	home := page.CanonicalURL
	if home == "" {
		home = page.URL
	}
	// A store or aggregator page names itself as the site; the scene belongs to the studio's
	// brand, which is how XBVR's own scrapers for such hosts file their scenes.
	site := ext.Site
	if ext.PageKind != llmscrape.KindOfficial {
		site = firstNonEmpty(ext.Studio, ext.Site)
	}
	site = brandName(firstNonEmpty(site, ext.Studio, hostOf(home)))
	studio := brandName(firstNonEmpty(ext.Studio, site))

	// Stable across runs, so scraping the same page again updates the scene rather than
	// duplicating it. The prefix keeps these apart from IDs owned by XBVR's site scrapers.
	idPart := ext.SiteSceneID
	if idPart == "" {
		sum := sha1.Sum([]byte(home))
		idPart = hex.EncodeToString(sum[:])[:10]
	}

	scene := models.ScrapedScene{
		SceneID:     slugify.Slugify("llm-" + site + "-" + idPart),
		SiteID:      idPart,
		SceneType:   "VR",
		Title:       ext.Title,
		Studio:      studio,
		Site:        site,
		Cast:        ext.Cast,
		Tags:        ext.Tags,
		Synopsis:    ext.Synopsis,
		Released:    ext.Released,
		Duration:    ext.DurationMinutes,
		HomepageURL: home,
	}
	if scene.Title == "" {
		scene.Title = page.Title
	}
	// Covers must stay nil rather than empty: the scene import reads Covers[0] whenever the
	// slice is non-nil.
	if ext.CoverImage >= 0 {
		scene.Covers = []string{page.Images[ext.CoverImage].URL}
	}
	for _, i := range ext.GalleryImages {
		scene.Gallery = append(scene.Gallery, page.Images[i].URL)
	}
	if ext.Trailer >= 0 {
		scene.TrailerType = "url"
		scene.TrailerSrc = page.Videos[ext.Trailer].URL
	}
	if file != nil && file.Filename != "" {
		scene.Filenames = []string{file.Filename}
	}
	return scene
}

var domainLikeRe = regexp.MustCompile(`(?i)^([a-z0-9][a-z0-9-]*)\.(com|net|org|xxx|tv|io|co|eu|vr|porn|app)$`)

// brandName turns a bare domain such as "Dezyred.com" into the brand "Dezyred".
func brandName(s string) string {
	s = strings.TrimSpace(s)
	if m := domainLikeRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// SuggestResult is the outcome of searching for drafts for one file.
type SuggestResult struct {
	FileID  uint           `json:"file_id"`
	Query   string         `json:"query"`
	Results []ScrapeResult `json:"results"`
}

// SuggestForFile searches the web for the file's scene and turns the promising pages into drafts.
// Pages already drafted for this file, including rejected ones, are not scraped again.
func (s *Service) SuggestForFile(ctx context.Context, fileID uint) (*SuggestResult, error) {
	if s.searcher == nil {
		return nil, errors.New("no web search provider is configured (add a Brave Search API key)")
	}
	file, err := LoadFileInfo(fileID)
	if err != nil {
		return nil, err
	}
	out := &SuggestResult{FileID: fileID, Query: llmscrape.BuildQuery(file.Filename, file.Context)}

	hits, err := s.searcher.Search(ctx, out.Query, s.settings.ResultsPerFile*2)
	if err != nil {
		return nil, err
	}
	models.MarkFileSearched(fileID)
	known := draftedURLs(fileID)
	var urls []string
	for _, h := range hits {
		if known[h.URL] || DomainBlocked(h.URL, s.settings.BlockedDomains) {
			continue
		}
		known[h.URL] = true
		urls = append(urls, h.URL)
		if len(urls) >= s.settings.ResultsPerFile {
			break
		}
	}

	out.Results = make([]ScrapeResult, len(urls))
	sem := make(chan struct{}, s.settings.Concurrency)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out.Results[i] = s.ScrapeURL(ctx, u, fileID, file, out.Query, false)
		}(i, u)
	}
	wg.Wait()

	sort.SliceStable(out.Results, func(a, b int) bool {
		return confidence(out.Results[a]) > confidence(out.Results[b])
	})
	return out, nil
}

func confidence(r ScrapeResult) float64 {
	if r.Draft == nil {
		return -1
	}
	return r.Draft.MatchConfidence
}

// LoadFileInfo reads what the LLM needs to know about a file.
func LoadFileInfo(fileID uint) (*llmscrape.FileInfo, error) {
	db, _ := models.GetDB()
	defer db.Close()
	var f models.File
	if err := db.Where(&models.File{ID: fileID}).First(&f).Error; err != nil {
		return nil, fmt.Errorf("file %d not found", fileID)
	}
	return &llmscrape.FileInfo{
		Filename:        f.Filename,
		DurationMinutes: int(f.VideoDuration / 60),
		Context:         models.GetFileMatchContext(fileID),
	}, nil
}

func draftedURLs(fileID uint) map[string]bool {
	db, _ := models.GetDB()
	defer db.Close()
	var drafts []models.DraftScene
	db.Select("source_url").Where("file_id = ?", fileID).Find(&drafts)
	m := make(map[string]bool, len(drafts))
	for _, d := range drafts {
		m[d.SourceURL] = true
	}
	return m
}
