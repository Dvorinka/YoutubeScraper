package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type VideoResponse struct {
	VideoID     string `json:"video_id"`
	ChannelName string `json:"channel_name"`
}

var ctx = context.Background()

// ChannelVideosResponse represents the response for channel videos scraping
type ChannelVideosResponse struct {
	Channel            string      `json:"channel"`
	ChannelURL         string      `json:"channel_url"`
	ChannelID          string      `json:"channel_id,omitempty"`
	ChannelAvatar      string      `json:"channel_avatar,omitempty"`
	ChannelDescription string      `json:"channel_description,omitempty"`
	RSSURL             string      `json:"rss_url,omitempty"`
	SubscribersText    string      `json:"subscribers_text"`
	Subscribers        int64       `json:"subscribers"`
	Videos             []VideoItem `json:"videos"`
}

// VideoItem holds per-video metadata extracted from the /videos page
type VideoItem struct {
	VideoID       string `json:"video_id"`
	Title         string `json:"title,omitempty"`
	Length        string `json:"length,omitempty"`
	ThumbnailURL  string `json:"thumbnail_url,omitempty"`
	ViewsText     string `json:"views_text,omitempty"`
	Views         int64  `json:"views"`
	PublishedText string `json:"published_text,omitempty"`
	PublishedDate string `json:"published_date,omitempty"` // ISO 8601 date
}

// normalizeChannelInput accepts a handle like "@FCBizoniUH" or "FCBizoniUH" or a full URL
// and returns the canonical handle (with leading @) and the corresponding /videos URL.
func normalizeChannelInput(input string) (handle string, url string) {
	in := strings.TrimSpace(input)
	lower := strings.ToLower(in)
	isURL := strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "www.") || strings.HasPrefix(lower, "youtube.com/")
	if isURL {
		// Ensure scheme
		if strings.HasPrefix(lower, "www.") || strings.HasPrefix(lower, "youtube.com/") {
			in = "https://" + strings.TrimPrefix(in, "www.")
			if !strings.HasPrefix(strings.ToLower(in), "https://youtube.com/") && !strings.HasPrefix(strings.ToLower(in), "https://www.youtube.com/") {
				in = "https://www." + strings.TrimPrefix(in, "https://")
			}
		}
		// Normalize m.youtube.com -> www.youtube.com
		in = strings.ReplaceAll(in, "m.youtube.com", "www.youtube.com")

		// Extract handle if present
		reHandle := regexp.MustCompile(`https?://(www\.)?youtube\.com/(@[^/]+)`) // group with @
		if m := reHandle.FindStringSubmatch(in); len(m) >= 3 {
			handle = m[2]
		} else {
			// Try path segment after domain
			rePath := regexp.MustCompile(`https?://(www\.)?youtube\.com/([^/?#]+)`) // capture after domain
			if m2 := rePath.FindStringSubmatch(in); len(m2) >= 3 {
				seg := m2[2]
				if strings.HasPrefix(seg, "@") {
					handle = seg
				} else {
					handle = "@" + seg
				}
			}
		}

		// Respect provided tab if present: /videos, /shorts, /streams; default to /videos
		if strings.Contains(strings.ToLower(in), "/videos") || strings.Contains(strings.ToLower(in), "/shorts") || strings.Contains(strings.ToLower(in), "/streams") {
			url = in
		} else {
			// Build a /videos URL from detected handle
			if handle == "" {
				// If we couldn't find a handle, just use the original URL
				url = in
			} else {
				url = fmt.Sprintf("https://www.youtube.com/%s/videos", handle)
			}
		}
	} else {
		// Not a URL; treat as handle or bare identifier
		if strings.HasPrefix(in, "@") {
			handle = in
		} else {
			handle = "@" + in
		}
		url = fmt.Sprintf("https://www.youtube.com/%s/videos", handle)
	}
	if handle == "" {
		// As a final fallback from given input
		handle = in
		if !strings.HasPrefix(handle, "@") {
			handle = "@" + handle
		}
	}
	return
}

// extractYTInitialData parses the ytInitialData JSON object embedded in YouTube HTML.
// This is the most robust approach since it walks the actual data structure regardless
// of renderer name changes (e.g., videoRenderer -> lockupViewModel -> whatever comes next).
func extractYTInitialData(html string) (map[string]interface{}, error) {
	// Find the ytInitialData variable assignment in script tags
	// Try multiple patterns in case YouTube changes the exact format
	patterns := []string{
		`var ytInitialData\s*=\s*({.+?});\s*</script>`,
		`window\["ytInitialData"\]\s*=\s*({.+?});`,
		`ytInitialData\s*=\s*({.+?});`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		if m := re.FindStringSubmatch(html); len(m) >= 2 {
			var data map[string]interface{}
			if err := json.Unmarshal([]byte(m[1]), &data); err == nil {
				return data, nil
			}
		}
	}
	return nil, fmt.Errorf("ytInitialData not found or unparseable")
}

// findVideosInJSON recursively walks a parsed JSON tree and extracts video metadata
// from any objects that look like video items. It is renderer-name-agnostic.
func findVideosInJSON(data interface{}) []VideoItem {
	var videos []VideoItem
	seen := make(map[string]struct{})

	var walk func(v interface{})
	walk = func(v interface{}) {
		switch val := v.(type) {
		case map[string]interface{}:
			// Check if this object represents a video by looking for videoId patterns
			// or lockupViewModel-like structure with contentId.
			vid := extractVideoFromObject(val)
			if vid.VideoID != "" {
				if _, ok := seen[vid.VideoID]; !ok {
					seen[vid.VideoID] = struct{}{}
					videos = append(videos, vid)
				}
			}
			// Continue walking all fields
			for _, fv := range val {
				walk(fv)
			}
		case []interface{}:
			for _, item := range val {
				walk(item)
			}
		}
	}

	walk(data)
	return videos
}

// extractVideoFromObject tries to extract a VideoItem from a JSON object.
// It uses multiple heuristics to work across different YouTube data structures.
func extractVideoFromObject(obj map[string]interface{}) VideoItem {
	var vi VideoItem

	// Heuristic 1: Look for contentId (new lockupViewModel structure)
	if id, ok := obj["contentId"].(string); ok && len(id) == 11 {
		vi.VideoID = id
		vi.ThumbnailURL = fmt.Sprintf("https://img.youtube.com/vi/%s/maxresdefault.jpg", id)
	}

	// Heuristic 2: Look for videoId (older videoRenderer structure)
	if vi.VideoID == "" {
		if id, ok := obj["videoId"].(string); ok && len(id) == 11 {
			vi.VideoID = id
			vi.ThumbnailURL = fmt.Sprintf("https://img.youtube.com/vi/%s/maxresdefault.jpg", id)
		}
	}

	if vi.VideoID == "" {
		return vi
	}

	// --- Extract Title ---
	// Try multiple title paths
	vi.Title = extractStringFromPath(obj,
		[]string{"metadata", "lockupMetadataViewModel", "title", "content"},
		[]string{"title", "content"},
		[]string{"title", "simpleText"},
		[]string{"title", "runs", "0", "text"},
		[]string{"accessibility", "accessibilityData", "label"},
		[]string{"rendererContext", "accessibilityContext", "label"},
	)
	// Remove duration suffix from accessibility labels
	if vi.Title != "" {
		durationRe := regexp.MustCompile(`\s+\d+\s+(?:minute|second|hour)s?(?:,\s*\d+\s+(?:minute|second|hour)s?)*\s*$`)
		vi.Title = strings.TrimSpace(durationRe.ReplaceAllString(vi.Title, ""))
	}

	// --- Extract Length ---
	// Try lockupViewModel badge path
	if badges := extractFromPath(obj, []string{"contentImage", "thumbnailViewModel", "overlays"}); badges != nil {
		if arr, ok := badges.([]interface{}); ok {
			for _, badge := range arr {
				if b, ok := badge.(map[string]interface{}); ok {
					if text := extractStringFromPath(b, []string{"thumbnailBottomOverlayViewModel", "badges", "0", "thumbnailBadgeViewModel", "text"}); text != "" {
						vi.Length = text
						break
					}
				}
			}
		}
	}
	// Fallback to older lengthText paths
	if vi.Length == "" {
		vi.Length = extractStringFromPath(obj,
			[]string{"lengthText", "simpleText"},
			[]string{"lengthText", "runs", "0", "text"},
		)
	}

	// --- Extract Views and Published Time ---
	// Try metadata rows (new structure)
	if meta := extractFromPath(obj, []string{"metadata", "lockupMetadataViewModel", "metadata", "contentMetadataViewModel", "metadataRows"}); meta != nil {
		if rows, ok := meta.([]interface{}); ok && len(rows) > 0 {
			if firstRow, ok := rows[0].(map[string]interface{}); ok {
				if parts := extractFromPath(firstRow, []string{"metadataParts"}); parts != nil {
					if arr, ok := parts.([]interface{}); ok {
						for i, part := range arr {
							if p, ok := part.(map[string]interface{}); ok {
								text := extractStringFromPath(p, []string{"text", "content"})
								if text != "" {
									// First part is usually views, second is published time
									if i == 0 {
										vi.ViewsText = text
										vi.Views = parseCountText(text)
									} else if i == 1 {
										vi.PublishedText = text
										vi.PublishedDate = parseRelativeToISO(text)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	// Fallback to older viewCountText/publishedTimeText paths
	if vi.ViewsText == "" {
		if text := extractStringFromPath(obj, []string{"viewCountText", "simpleText"}); text != "" {
			vi.ViewsText = text
			vi.Views = parseCountText(text)
		}
	}
	if vi.PublishedText == "" {
		if text := extractStringFromPath(obj, []string{"publishedTimeText", "simpleText"}); text != "" {
			vi.PublishedText = text
			vi.PublishedDate = parseRelativeToISO(text)
		}
	}

	return vi
}

// extractFromPath navigates a nested map[string]interface{} by key path and returns the value.
func extractFromPath(obj map[string]interface{}, path []string) interface{} {
	current := obj
	for i, key := range path {
		if i == len(path)-1 {
			return current[key]
		}
		if next, ok := current[key].(map[string]interface{}); ok {
			current = next
		} else if nextArr, ok := current[key].([]interface{}); ok {
			// If the next step is an array, try to index into it
			if i+1 < len(path) {
				idxStr := path[i+1]
				if idx, err := strconv.Atoi(idxStr); err == nil && idx >= 0 && idx < len(nextArr) {
					if idx == len(path)-2 {
						return nextArr[idx]
					}
					if nextMap, ok := nextArr[idx].(map[string]interface{}); ok {
						current = nextMap
						// Skip the index in path since we already consumed it
						path = append(path[:i+1], path[i+2:]...)
						continue
					}
				}
			}
			return nil
		} else {
			return nil
		}
	}
	return nil
}

// extractStringFromPath tries multiple paths and returns the first non-empty string found.
func extractStringFromPath(obj map[string]interface{}, paths ...[]string) string {
	for _, path := range paths {
		if v := extractFromPath(obj, path); v != nil {
			if s, ok := v.(string); ok && s != "" {
				return unescapeYT(s)
			}
		}
	}
	return ""
}

// fetchChannelVideos scrapes the channel's /videos page and extracts video IDs present
func fetchChannelVideos(channelInput string) (ChannelVideosResponse, error) {
	handle, channelURL := normalizeChannelInput(channelInput)
	log.Printf("Fetching channel videos: handle=%s url=%s", handle, channelURL)

	// Craft request with a desktop UA to improve likelihood of getting full HTML payload
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, channelURL, nil)
	if err != nil {
		return ChannelVideosResponse{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ChannelVideosResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ChannelVideosResponse{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ChannelVideosResponse{}, err
	}
	html := string(body)

	// --- PRIMARY: Try to parse ytInitialData JSON ---
	// This is the most future-proof approach since it walks the actual data
	// structure regardless of renderer name changes.
	var videos []VideoItem
	var channelID, channelAvatar, channelDesc, rssURL string

	if ytData, err := extractYTInitialData(html); err == nil {
		log.Println("Successfully extracted ytInitialData, using JSON parser")
		videos = findVideosInJSON(ytData)

		// Extract additional channel metadata from JSON
		if meta := extractFromPath(ytData, []string{"metadata", "channelMetadataRenderer"}); meta != nil {
			if m, ok := meta.(map[string]interface{}); ok {
				channelID = extractStringFromPath(m, []string{"externalId"})
				channelDesc = extractStringFromPath(m, []string{"description"})
				if av := extractFromPath(m, []string{"avatar", "thumbnails"}); av != nil {
					if arr, ok := av.([]interface{}); ok && len(arr) > 0 {
						if first, ok := arr[0].(map[string]interface{}); ok {
							channelAvatar = extractStringFromPath(first, []string{"url"})
						}
					}
				}
				if rss := extractFromPath(m, []string{"rssUrl"}); rss != nil {
					if s, ok := rss.(string); ok {
						rssURL = s
					}
				}
			}
		}
	}

	// --- FALLBACK: If JSON parsing yields no videos, use regex patterns ---
	if len(videos) == 0 {
		log.Println("JSON extraction yielded no videos, falling back to regex patterns")
		videos = extractVideosWithRegex(html)
	}

	// --- Channel metadata extraction ---
	// Attempt to derive a displayable channel handle/name
	channelDisplay := handle
	// Try to extract canonicalBaseUrl if present
	if canRe := regexp.MustCompile(`"canonicalBaseUrl":"\\/(@[^\"]+)"`).FindStringSubmatch(html); len(canRe) >= 2 {
		channelDisplay = canRe[1]
	}

	// Extract subscribers (header section)
	subText := ""
	// Try simpleText first
	if m := regexp.MustCompile(`"subscriberCountText":\{"simpleText":"([^"]+)"`).FindStringSubmatch(html); len(m) >= 2 {
		subText = m[1]
	} else {
		// Try runs: join all text segments inside subscriberCountText.runs
		if loc := regexp.MustCompile(`"subscriberCountText":\{"runs":\[`).FindStringIndex(html); loc != nil {
			slice := html[loc[1]:]
			if endIdx := strings.Index(slice, "]}"); endIdx != -1 {
				runsChunk := slice[:endIdx]
				texts := regexp.MustCompile(`"text":"([^"]+)"`).FindAllStringSubmatch(runsChunk, -1)
				var parts []string
				for _, t := range texts {
					if len(t) >= 2 {
						parts = append(parts, unescapeYT(t[1]))
					}
				}
				subText = strings.Join(parts, "")
			}
		}
	}
	// Fallbacks: approximateSubscriberCount or localized patterns
	if subText == "" {
		if m := regexp.MustCompile(`"approximateSubscriberCount":"([^"]+)"`).FindStringSubmatch(html); len(m) >= 2 {
			subText = m[1]
		}
	}
	if subText == "" {
		if m := regexp.MustCompile(`(?i)([0-9][0-9\s\.,]*)\s*(odběratel(?:é|ů)?|subscribers?)`).FindStringSubmatch(html); len(m) >= 2 {
			subText = strings.TrimSpace(m[0])
		}
	}
	subs := parseCountText(subText)

	res := ChannelVideosResponse{
		Channel:            channelDisplay,
		ChannelURL:         channelURL,
		ChannelID:          channelID,
		ChannelAvatar:      channelAvatar,
		ChannelDescription: channelDesc,
		RSSURL:             rssURL,
		SubscribersText:    subText,
		Subscribers:        subs,
		Videos:             videos,
	}
	return res, nil
}

// extractVideosWithRegex is the fallback method that uses regex patterns
// to extract video data when JSON parsing fails or yields no results.
func extractVideosWithRegex(html string) []VideoItem {
	seen := make(map[string]struct{})
	var videos []VideoItem

	// Method 1: Find video IDs from thumbnail URLs (current YouTube structure)
	vidRe := regexp.MustCompile(`i\.ytimg\.com/vi/([a-zA-Z0-9_-]{11})`)
	matches := vidRe.FindAllStringSubmatchIndex(html, -1)
	for _, idx := range matches {
		if len(idx) < 4 {
			continue
		}
		id := html[idx[2]:idx[3]]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		start := idx[0]
		if start-3000 > 0 {
			start = start - 3000
		}
		end := idx[1] + 10000
		if end > len(html) {
			end = len(html)
		}
		snippet := html[start:end]

		vi := VideoItem{VideoID: id}
		vi.ThumbnailURL = fmt.Sprintf("https://img.youtube.com/vi/%s/maxresdefault.jpg", id)

		// Title from accessibilityContext.label
		if m := regexp.MustCompile(`"accessibilityContext":\{[^}]*"label":"([^"]{15,})"`).FindStringSubmatch(snippet); len(m) >= 2 {
			title := unescapeYT(m[1])
			durationRe := regexp.MustCompile(`\s+\d+\s+(?:minute|second|hour)s?(?:,\s*\d+\s+(?:minute|second|hour)s?)*\s*$`)
			title = durationRe.ReplaceAllString(title, "")
			vi.Title = strings.TrimSpace(title)
		} else if m := regexp.MustCompile(`"metadata":\{[^}]*"content":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			content := unescapeYT(m[1])
			if content != "Share" {
				vi.Title = content
			}
		}

		// Length
		if m := regexp.MustCompile(`"thumbnailBottomOverlayViewModel":\{[^}]*"text":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Length = m[1]
		} else if m := regexp.MustCompile(`"lengthText":\{[^}]*"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Length = m[1]
		}

		// Published time
		if m := regexp.MustCompile(`"publishedTimeText":\{"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.PublishedText = m[1]
			vi.PublishedDate = parseRelativeToISO(m[1])
		}

		// Views
		if m := regexp.MustCompile(`"viewCountText":\{"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.ViewsText = m[1]
			vi.Views = parseCountText(m[1])
		}

		videos = append(videos, vi)
	}

	// Method 2: Old videoRenderer structure (for backwards compatibility)
	oldRe := regexp.MustCompile(`"videoRenderer":\{[^}]*?"videoId":"([a-zA-Z0-9_-]{11})"`)
	oldMatches := oldRe.FindAllStringSubmatchIndex(html, -1)
	for _, idx := range oldMatches {
		if len(idx) < 4 {
			continue
		}
		id := html[idx[2]:idx[3]]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		start := idx[0]
		if start-2000 > 0 {
			start = start - 2000
		}
		end := idx[1] + 8000
		if end > len(html) {
			end = len(html)
		}
		snippet := html[start:end]

		vi := VideoItem{VideoID: id}
		vi.ThumbnailURL = fmt.Sprintf("https://img.youtube.com/vi/%s/maxresdefault.jpg", id)

		if m := regexp.MustCompile(`"title":\{"runs":\[\{"text":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Title = unescapeYT(m[1])
		} else if m := regexp.MustCompile(`"title":\{"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Title = unescapeYT(m[1])
		}
		if m := regexp.MustCompile(`"lengthText":\{[^}]*"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Length = m[1]
		}
		if m := regexp.MustCompile(`"publishedTimeText":\{"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.PublishedText = m[1]
			vi.PublishedDate = parseRelativeToISO(m[1])
		}
		if m := regexp.MustCompile(`"viewCountText":\{"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.ViewsText = m[1]
			vi.Views = parseCountText(m[1])
		}

		videos = append(videos, vi)
	}

	// Method 3: Shorts videos (reelItemRenderer)
	shortsRe := regexp.MustCompile(`"reelItemRenderer":\{[^}]*?"videoId":"([a-zA-Z0-9_-]{11})"`)
	shorts := shortsRe.FindAllStringSubmatchIndex(html, -1)
	for _, idx := range shorts {
		if len(idx) < 4 {
			continue
		}
		id := html[idx[2]:idx[3]]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		start := idx[0]
		if start-2000 > 0 {
			start = start - 2000
		}
		end := idx[1] + 8000
		if end > len(html) {
			end = len(html)
		}
		snippet := html[start:end]

		vi := VideoItem{VideoID: id}
		vi.ThumbnailURL = fmt.Sprintf("https://img.youtube.com/vi/%s/maxresdefault.jpg", id)
		if m := regexp.MustCompile(`"headline":\{"simpleText":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Title = unescapeYT(m[1])
		} else if m := regexp.MustCompile(`"title":\{"runs":\[\{"text":"([^"]+)"`).FindStringSubmatch(snippet); len(m) >= 2 {
			vi.Title = unescapeYT(m[1])
		}
		videos = append(videos, vi)
	}

	return videos
}

// unescapeYT fixes escaped sequences in YouTube HTML JSON strings
func unescapeYT(s string) string {
	s = strings.ReplaceAll(s, `\/`, `/`)
	s = strings.ReplaceAll(s, `\u0026`, `&`)
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

// normalizeThumbURL ensures thumbnails use https and removes query artifacts if needed
func normalizeThumbURL(u string) string {
	u = unescapeYT(u)
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	return u
}

// parseRelativeToISO converts strings like "3 days ago", "2 weeks ago", "1 year ago" to ISO date (yyyy-mm-dd)
func parseRelativeToISO(rel string) string {
	now := time.Now()
	lower := strings.ToLower(rel)
	re := regexp.MustCompile(`(\d+)[\s-]*(second|minute|hour|day|week|month|year)s?\s+ago`)
	if m := re.FindStringSubmatch(lower); len(m) >= 3 {
		n, _ := strconv.Atoi(m[1])
		unit := m[2]
		dur := time.Duration(0)
		switch unit {
		case "second":
			dur = time.Duration(n) * time.Second
			return now.Add(-dur).Format("2006-01-02")
		case "minute":
			dur = time.Duration(n) * time.Minute
			return now.Add(-dur).Format("2006-01-02")
		case "hour":
			dur = time.Duration(n) * time.Hour
			return now.Add(-dur).Format("2006-01-02")
		case "day":
			return now.AddDate(0, 0, -n).Format("2006-01-02")
		case "week":
			return now.AddDate(0, 0, -7*n).Format("2006-01-02")
		case "month":
			return now.AddDate(0, -n, 0).Format("2006-01-02")
		case "year":
			return now.AddDate(-n, 0, 0).Format("2006-01-02")
		}
	}
	// Sometimes YouTube uses "Streamed X days ago" or "Premiered ..."
	re2 := regexp.MustCompile(`(streamed|premiered|started|live)\s+(\d+)\s+(second|minute|hour|day|week|month|year)s?\s+ago`)
	if m := re2.FindStringSubmatch(lower); len(m) >= 4 {
		n, _ := strconv.Atoi(m[2])
		unit := m[3]
		switch unit {
		case "second":
			return now.Add(-time.Duration(n) * time.Second).Format("2006-01-02")
		case "minute":
			return now.Add(-time.Duration(n) * time.Minute).Format("2006-01-02")
		case "hour":
			return now.Add(-time.Duration(n) * time.Hour).Format("2006-01-02")
		case "day":
			return now.AddDate(0, 0, -n).Format("2006-01-02")
		case "week":
			return now.AddDate(0, 0, -7*n).Format("2006-01-02")
		case "month":
			return now.AddDate(0, -n, 0).Format("2006-01-02")
		case "year":
			return now.AddDate(-n, 0, 0).Format("2006-01-02")
		}
	}
	return ""
}

// parseLocalizedDuration converts localized duration phrases (e.g., "5 minut a 59 sekund")
// into a mm:ss or hh:mm:ss string. Supports English and basic Czech variants.
func parseLocalizedDuration(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	// Replace HTML entities and non-breaking spaces
	t = strings.ReplaceAll(t, "&nbsp;", " ")
	t = strings.ReplaceAll(t, "\u00a0", " ")
	t = strings.TrimSpace(t)

	// If already in 00:00 or 0:00:00 form, return as-is trimmed
	if m := regexp.MustCompile(`^\d{1,2}:\d{2}(?::\d{2})?$`).FindString(t); m != "" {
		return m
	}

	// Patterns like: 1 hour 2 minutes 3 seconds (EN)
	// or Czech: 1 hodina/hodiny/hodin, 2 minuty/minut, 3 sekundy/sekund
	// We'll extract numbers for h/m/s separately.
	var h, m, sec int

	// English capture
	if mm := regexp.MustCompile(`(\d+)\s*hour`).FindStringSubmatch(t); len(mm) >= 2 {
		h, _ = strconv.Atoi(mm[1])
	}
	if mm := regexp.MustCompile(`(\d+)\s*minute`).FindStringSubmatch(t); len(mm) >= 2 {
		m, _ = strconv.Atoi(mm[1])
	}
	if mm := regexp.MustCompile(`(\d+)\s*second`).FindStringSubmatch(t); len(mm) >= 2 {
		sec, _ = strconv.Atoi(mm[1])
	}

	// Czech capture
	if mm := regexp.MustCompile(`(\d+)\s*hodin(?:a|y)?`).FindStringSubmatch(t); len(mm) >= 2 {
		if h == 0 {
			h, _ = strconv.Atoi(mm[1])
		}
	}
	if mm := regexp.MustCompile(`(\d+)\s*minut(?:a|y)?`).FindStringSubmatch(t); len(mm) >= 2 {
		if m == 0 {
			m, _ = strconv.Atoi(mm[1])
		}
	}
	if mm := regexp.MustCompile(`(\d+)\s*sekund(?:a|y)?`).FindStringSubmatch(t); len(mm) >= 2 {
		if sec == 0 {
			sec, _ = strconv.Atoi(mm[1])
		}
	}

	// If we still didn't parse anything but string contains a plain number like "5 minutes",
	// ensure we at least capture minutes.
	if h == 0 && m == 0 && sec == 0 {
		if mm := regexp.MustCompile(`^(\d+)$`).FindStringSubmatch(t); len(mm) >= 2 {
			m, _ = strconv.Atoi(mm[1])
		}
	}

	// Build the time string
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	if m > 0 || sec > 0 {
		return fmt.Sprintf("%d:%02d", m, sec)
	}
	return ""
}

// parseCountText handles strings like "1,234 views", "12K subscribers", "3.4M"
func parseCountText(s string) int64 {
	t := strings.ToLower(strings.TrimSpace(s))
	// keep only the first number token
	re := regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)([kmb])?`)
	if m := re.FindStringSubmatch(t); len(m) >= 2 {
		numStr := m[1]
		suf := ""
		if len(m) >= 3 {
			suf = m[2]
		}
		f, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0
		}
		switch suf {
		case "k":
			f *= 1_000
		case "m":
			f *= 1_000_000
		case "b":
			f *= 1_000_000_000
		}
		return int64(f)
	}
	// Fallback: strip non-digits and parse
	digits := regexp.MustCompile(`[^0-9]`).ReplaceAllString(t, "")
	if digits == "" {
		return 0
	}
	v, _ := strconv.ParseInt(digits, 10, 64)
	return v
}

func channelVideosHandler(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		log.Println("Missing channel parameter")
		http.Error(w, "Missing channel parameter. Provide a handle like @FCBizoniUH, FCBizoniUH, or a full channel URL.", http.StatusBadRequest)
		return
	}

	res, err := fetchChannelVideos(channel)
	if err != nil {
		log.Printf("Failed to fetch channel videos for %s: %v", channel, err)
		http.Error(w, "Failed to fetch channel videos", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// CORS Middleware
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		// Handle preflight requests
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	html := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>YouTube Channel Scraper API</title>
    <style>
        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            line-height: 1.6;
            max-width: 900px;
            margin: 0 auto;
            padding: 20px;
            color: #333;
        }
        header {
            background-color: #4a6fa5;
            color: white;
            padding: 20px;
            border-radius: 5px;
            margin-bottom: 30px;
        }
        h1, h2, h3 {
            color: #2c3e50;
        }
        .endpoint {
            background-color: #f8f9fa;
            border-left: 4px solid #4a6fa5;
            padding: 15px;
            margin: 20px 0;
            border-radius: 0 4px 4px 0;
        }
        code {
            background-color: #f0f0f0;
            padding: 2px 5px;
            border-radius: 3px;
            font-family: 'Courier New', monospace;
        }
        pre {
            background-color: #f8f9fa;
            padding: 15px;
            border-radius: 4px;
            overflow-x: auto;
        }
        .method {
            display: inline-block;
            padding: 3px 8px;
            border-radius: 3px;
            color: white;
            font-weight: bold;
            font-size: 0.8em;
            margin-right: 10px;
        }
        .method.get { background-color: #61affe; }
        .example {
            margin: 15px 0;
        }
    </style>
</head>
<body>
    <header>
        <h1>YouTube Channel Scraper API</h1>
        <p>Fetch per-video metadata from a channel's tabs using a handle or full URL. Supported links include /videos, /shorts, and /streams.</p>
    </header>

    <section>
        <h2>Introduction</h2>
        <p>This service scrapes a YouTube channel's videos page and returns the list of video IDs present in the initial HTML payload.</p>
    </section>

    <section>
        <h2>Base URL</h2>
        <p>All API requests should be made to:</p>
        <pre>http://localhost:7857</pre>
    </section>

    <section>
        <h2>Endpoints</h2>

        <div class="endpoint">
            <h3>Get Channel Videos</h3>
            <div class="method get">GET</div> <code>/channel_videos?channel={handle_or_url}</code>
            
            <h4>Description</h4>
            <p>Scrapes the specified channel's /videos page and returns per-video metadata.</p>
            
            <h4>Query Parameters</h4>
            <ul>
                <li><code>channel</code> (required): A channel handle like <code>@FCBizoniUH</code>, <code>FCBizoniUH</code>, or a full channel URL.</li>
            </ul>
            
            <h4>Response</h4>
            <p>Returns a JSON object containing channel info and an array of videos with id, title, length, thumbnail, views, and published date.</p>
            
            <div class="example">
                <h5>Example Request:</h5>
                <pre>GET /channel_videos?channel=@FotbalKunovice</pre>
                
                <h5>Example Response:</h5>
                <pre>{
  "channel": "@FotbalKunovice",
  "channel_url": "https://www.youtube.com/@FotbalKunovice/videos",
  "subscribers_text": "131 odběratelů",
  "subscribers": 131,
  "videos": [
    {
      "video_id": "Eze9AtRrvN4",
      "title": "Jiří Chramcov po Frýdku",
      "length": "4:21",
      "thumbnail_url": "https://i.ytimg.com/vi/Eze9AtRrvN4/hqdefault.jpg",
      "views_text": "34 views",
      "views": 34,
      "published_text": "1 day ago",
      "published_date": "2025-09-08"
    }
  ]
}</pre>
            </div>
        </div>
    </section>

    <section>
        <h2>Features</h2>
        <ul>
            <li>Accepts handle or full URL</li>
            <li>No external dependencies like Redis</li>
            <li>Simple and intuitive API with CORS support</li>
            <li>CORS support for web applications</li>
        </ul>
    </section>

    <footer>
        <p> 2025 YouTube Channel Scraper | API Version 1.0.0</p>
    </footer>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func main() {
	mux := http.NewServeMux()

	// Create a new mux with CORS middleware
	handlerWithCORS := corsMiddleware(mux)

	// Register routes on the original mux
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/channel_videos", channelVideosHandler)

	log.Println("Server starting on :7857...")
	log.Fatal(http.ListenAndServe("0.0.0.0:7857", handlerWithCORS))
}
