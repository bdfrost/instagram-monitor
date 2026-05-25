package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Version info injected at build time via ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Config holds the full monitor configuration.
type Config struct {
	Monitors        []MonitorConfig `json:"monitors"`
	StateFile       string          `json:"stateFile"`
	NotificationURL string          `json:"notificationURL"`
	HTTPTimeout     int             `json:"httpTimeout"`
}

// MonitorConfig defines what to watch for a single Instagram user.
type MonitorConfig struct {
	Username    string   `json:"username"`    // Instagram username (without @)
	DisplayName string   `json:"displayName"` // Friendly name for alerts
	Keywords    []string `json:"keywords"`    // Keywords to match in captions (case-insensitive)
	NotifyOnAny bool     `json:"notifyOnAny"` // If true, alert on ANY new post (not just keyword matches)
}

// PostState tracks what we've already seen for a user.
type PostState struct {
	LastPostID    string `json:"lastPostId"`
	LastTimestamp string `json:"lastTimestamp"`
}

// InstagramPost holds the metadata we extract from a post.
type InstagramPost struct {
	Shortcode string
	Caption   string
	Timestamp time.Time
	MediaURL  string
	IsVideo   bool
}

func main() {
	configFile := flag.String("config", "/app/config/config.json", "path to config file (JSON)")
	dryRun := flag.Bool("dry-run", false, "print what would be monitored without sending notifications")
	stateDir := flag.String("state-dir", "/app/state", "directory to persist state")
	flag.Parse()

	cfg, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Apply env overrides
	if url := os.Getenv("NOTIFICATION_URL"); url != "" {
		cfg.NotificationURL = url
	}
	if timeout := os.Getenv("HTTP_TIMEOUT"); timeout != "" {
		fmt.Sscanf(timeout, "%d", &cfg.HTTPTimeout)
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 30
	}
	if cfg.StateFile == "" {
		cfg.StateFile = *stateDir + "/state.json"
	}

	log.Printf("Starting Instagram monitor with %d configured monitor(s) (dry-run=%v)", len(cfg.Monitors), *dryRun)

	// Load persisted state
	state := loadState(cfg.StateFile)

	client := &http.Client{
		Timeout: time.Duration(cfg.HTTPTimeout) * time.Second,
	}

	var alerts []string

	for _, m := range cfg.Monitors {
		displayName := m.DisplayName
		if displayName == "" {
			displayName = "@" + m.Username
		}

		log.Printf("Checking %s...", displayName)

		posts, err := fetchRecentPosts(client, m.Username, 12)
		if err != nil {
			log.Printf("ERROR fetching posts for %s: %v", m.Username, err)
			continue
		}

		if len(posts) == 0 {
			log.Printf("  No posts found for %s (account may be private or scraping blocked)", m.Username)
			continue
		}

		newPosts := filterNewPosts(posts, state[m.Username])

		if len(newPosts) == 0 {
			log.Printf("  No new posts for %s since last check", displayName)
			continue
		}

		log.Printf("  Found %d new post(s) for %s", len(newPosts), displayName)

		for _, post := range newPosts {
			matches, match := checkKeywords(post.Caption, m.Keywords)

			if m.NotifyOnAny || matches {
				alert := formatAlert(displayName, post, match)
				alerts = append(alerts, alert)
				log.Printf("  ALERT: %s", alert)
			}
		}

		// Update state to the newest post
		if len(newPosts) > 0 {
			state[m.Username] = PostState{
				LastPostID:    newPosts[0].Shortcode,
				LastTimestamp: newPosts[0].Timestamp.Format(time.RFC3339),
			}
		}
	}

	// Persist state
	if err := saveState(cfg.StateFile, state); err != nil {
		log.Printf("WARNING: failed to save state: %v", err)
	}

	// Send notifications
	if len(alerts) > 0 && !*dryRun && cfg.NotificationURL != "" {
		payload := map[string]interface{}{
			"service":   "instagram-monitor",
			"alerts":    len(alerts),
			"messages":  alerts,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		if err := sendWebhook(client, cfg.NotificationURL, payload); err != nil {
			log.Printf("ERROR sending notification: %v", err)
		} else {
			log.Printf("Sent %d alert(s) to webhook", len(alerts))
		}
	} else if len(alerts) > 0 && *dryRun {
		fmt.Println("\n--- DRY RUN: would have sent alerts ---")
		for _, a := range alerts {
			fmt.Println(a)
		}
	} else if len(alerts) > 0 && cfg.NotificationURL == "" {
		log.Printf("ALERTS found but NOTIFICATION_URL not configured — alerts logged only")
	}

	if len(alerts) == 0 {
		log.Println("No new matching posts found this run")
	}
}

// Regex to extract the __additionalDataLoaded or _sharedData JSON from page HTML
var dataRegexes = []*regexp.Regexp{
	regexp.MustCompile(`window\._sharedData\s*=\s*({.*?});\s*</script>`),
	regexp.MustCompile(`window\.__additionalDataLoaded\(\s*'graphql',\s*({.*?})\s*\);`),
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found: %s", path)
		}
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON (%s): %v", path, err)
	}

	if len(cfg.Monitors) == 0 {
		return nil, fmt.Errorf("no monitors configured in %s", path)
	}

	return &cfg, nil
}

func loadState(path string) map[string]PostState {
	state := make(map[string]PostState)

	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("No existing state file at %s, starting fresh", path)
		return state
	}

	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("WARNING: corrupt state file, starting fresh: %v", err)
		return state
	}

	log.Printf("Loaded state for %d monitored account(s)", len(state))
	return state
}

func saveState(path string, state map[string]PostState) error {
	dir := path[:strings.LastIndex(path, "/")]
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func fetchRecentPosts(client *http.Client, username string, count int) ([]InstagramPost, error) {
	// Strategy: fetch the Instagram profile page and parse embedded GraphQL data
	url := fmt.Sprintf("https://www.instagram.com/%s/", username)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Try to extract JSON from page
	var data map[string]interface{}

	for _, re := range dataRegexes {
		match := re.FindSubmatch(body)
		if match != nil {
			if err := json.Unmarshal(match[1], &data); err == nil {
				break
			}
		}
	}

	if data == nil {
		return nil, fmt.Errorf("could not extract embedded data from page (structure may have changed)")
	}

	return extractPosts(data, count)
}

func extractPosts(data map[string]interface{}, count int) ([]InstagramPost, error) {
	// Navigate the GraphQL structure:
	// data.data.user.edge_owner_to_timeline_media.edges[].node
	// or data.graphql.user.edge_owner_to_timeline_media.edges[].node

	var edgeMedia map[string]interface{}

	// Try path 1: data.data.user(...)
	if d, ok := data["data"].(map[string]interface{}); ok {
		if user, ok := d["user"].(map[string]interface{}); ok {
			if em, ok := user["edge_owner_to_timeline_media"].(map[string]interface{}); ok {
				edgeMedia = em
			}
		}
	}

	// Try path 2: data.graphql.user(...)
	if edgeMedia == nil {
		if gql, ok := data["graphql"].(map[string]interface{}); ok {
			if user, ok := gql["user"].(map[string]interface{}); ok {
				if em, ok := user["edge_owner_to_timeline_media"].(map[string]interface{}); ok {
					edgeMedia = em
				}
			}
		}
	}

	if edgeMedia == nil {
		return nil, fmt.Errorf("could not find timeline media in response")
	}

	edges, ok := edgeMedia["edges"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("edges is not an array")
	}

	var posts []InstagramPost
	for i, e := range edges {
		if i >= count {
			break
		}
		edge, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		node, ok := edge["node"].(map[string]interface{})
		if !ok {
			continue
		}
		posts = append(posts, parsePostNode(node))
	}

	return posts, nil
}

func parsePostNode(node map[string]interface{}) InstagramPost {
	var post InstagramPost

	if s, ok := node["shortcode"].(string); ok {
		post.Shortcode = s
	}

	// Caption: node.edge_media_to_caption.edges[0].node.text
	if edgeCap, ok := node["edge_media_to_caption"].(map[string]interface{}); ok {
		if edges, ok := edgeCap["edges"].([]interface{}); ok && len(edges) > 0 {
			if first, ok := edges[0].(map[string]interface{}); ok {
				if n, ok := first["node"].(map[string]interface{}); ok {
					post.Caption, _ = n["text"].(string)
				}
			}
		}
	}

	// Timestamp
	if ts, ok := node["taken_at_timestamp"].(float64); ok {
		post.Timestamp = time.Unix(int64(ts), 0)
	}

	// Media type
	post.IsVideo, _ = node["is_video"].(bool)

	// Display URL
	if url, ok := node["display_url"].(string); ok {
		post.MediaURL = url
	}

	return post
}

func filterNewPosts(posts []InstagramPost, prev PostState) []InstagramPost {
	if prev.LastPostID == "" {
		// First run — baseline all existing posts, alert on none
		return nil
	}

	var newPosts []InstagramPost
	for _, p := range posts {
		if p.Shortcode == prev.LastPostID {
			break
		}
		newPosts = append(newPosts, p)
	}

	return newPosts
}

func checkKeywords(caption string, keywords []string) (bool, string) {
	captionLower := strings.ToLower(caption)
	for _, kw := range keywords {
		if strings.Contains(captionLower, strings.ToLower(kw)) {
			return true, kw
		}
	}
	return false, ""
}

func formatAlert(displayName string, post InstagramPost, matchedKeyword string) string {
	msg := fmt.Sprintf("📸 New post from %s", displayName)
	if matchedKeyword != "" {
		msg += fmt.Sprintf(" (matched keyword: %q)", matchedKeyword)
	}
	msg += fmt.Sprintf("\nhttps://www.instagram.com/p/%s/", post.Shortcode)
	if !post.Timestamp.IsZero() {
		msg += fmt.Sprintf("\nPosted: %s", post.Timestamp.Format("2006-01-02 15:04 MST"))
	}
	if post.Caption != "" {
		caption := post.Caption
		if len(caption) > 200 {
			caption = caption[:197] + "..."
		}
		msg += fmt.Sprintf("\nCaption: %s", caption)
	}
	return msg
}

func sendWebhook(client *http.Client, url string, payload map[string]interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "instagram-monitor/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
