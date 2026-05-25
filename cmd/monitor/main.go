package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Config holds the full monitor configuration.
type Config struct {
	Monitors        []MonitorConfig `json:"monitors"`
	StateFile       string          `json:"stateFile"`
	HTTPTimeout     int             `json:"httpTimeout"`
	NotificationURL string          `json:"notificationURL"` // Reads from NOTIFICATION_URL env (overrides JSON)
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
	IsVideo   bool
	Timestamp int64
}

func main() {
	log.Printf("Instagram Monitor starting (version=%s commit=%s date=%s)", version, commit, date)

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "./config.json"
	}
	log.Printf("Loading config from %s", configPath)

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// NOTIFICATION_URL env var overrides JSON config
	if webhookURL := os.Getenv("NOTIFICATION_URL"); webhookURL != "" {
		cfg.NotificationURL = webhookURL
	}
	log.Printf("HTTP timeout: %ds, Monitors: %d, Webhook configured: %v", cfg.HTTPTimeout, len(cfg.Monitors), cfg.NotificationURL != "")

	client := &http.Client{Timeout: time.Duration(cfg.HTTPTimeout) * time.Second}

	state := loadState(cfg.StateFile)
	defer saveState(cfg.StateFile, state)

	for _, m := range cfg.Monitors {
		log.Printf("Fetching recent posts for %s...", m.Username)
		posts, err := fetchRecentPosts(client, m.Username, 12)
		if err != nil {
			log.Printf("ERROR fetching posts for %s: %v", m.Username, err)
			continue
		}
		log.Printf("Got %d posts for %s", len(posts), m.Username)

		prev, ok := state[m.Username]
		if !ok {
			// First run: record the newest post and don't notify
			if len(posts) > 0 {
				state[m.Username] = PostState{
					LastPostID:    posts[0].Shortcode,
					LastTimestamp: time.Unix(posts[0].Timestamp, 0).Format(time.RFC3339),
				}
				log.Printf("First run for %s, recording baseline shortcode %s", m.Username, posts[0].Shortcode)
			}
			continue
		}

	newLoop:
		for _, p := range posts {
			if p.Shortcode == prev.LastPostID {
				break newLoop // Seen up to this point
			}
			shouldNotify := m.NotifyOnAny
			if !shouldNotify {
				for _, kw := range m.Keywords {
					if strings.Contains(strings.ToLower(p.Caption), strings.ToLower(kw)) {
						shouldNotify = true
						break
					}
				}
			}
			if shouldNotify && cfg.NotificationURL != "" {
				log.Printf("MATCH: %s posted '%s' (%s)", m.DisplayName, p.Caption[:mini(50, len(p.Caption))], p.Shortcode)
				if err := sendDiscordWebhook(cfg.NotificationURL, m.DisplayName, p); err != nil {
					log.Printf("ERROR sending webhook: %v", err)
				}
			}
			if err := checkStoryAvailability(client, m.Username, p.Shortcode); err != nil {
				log.Printf("ERROR checking story for %s/%s: %v", m.Username, p.Shortcode, err)
			}
		}

		if len(posts) > 0 {
			state[m.Username] = PostState{
				LastPostID:    posts[0].Shortcode,
				LastTimestamp: time.Unix(posts[0].Timestamp, 0).Format(time.RFC3339),
			}
		}
	}
	log.Println("Monitor run complete")
}

func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fetchRecentPosts(client *http.Client, username string, count int) ([]InstagramPost, error) {
	// Strategy: use Instagram's internal web profile API endpoint.
	// This returns the same GraphQL structure (edge_owner_to_timeline_media)
	// that was previously embedded in the HTML page, but is far more stable.
	u := fmt.Sprintf("https://www.instagram.com/api/v1/users/web_profile_info/?username=%s", url.QueryEscape(username))

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("X-IG-App-ID", "936619743392459")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(body), 500))
	}

	var wrapper struct {
		Data struct {
			User map[string]interface{} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	if wrapper.Data.User == nil {
		return nil, fmt.Errorf("no user data in API response (account may be private or not found)")
	}

	return extractPosts(wrapper.Data.User, count)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractPosts(userData map[string]interface{}, count int) ([]InstagramPost, error) {
	// Navigate the GraphQL structure from the API response:
	// userData["edge_owner_to_timeline_media"]["edges"][].node
	var edgeMedia map[string]interface{}
	if em, ok := userData["edge_owner_to_timeline_media"].(map[string]interface{}); ok {
		edgeMedia = em
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

		// Extract shortcode
		sc, _ := node["shortcode"].(string)
		if sc == "" {
			continue
		}

		// Extract caption text
		var caption string
		if capData, ok := node["edge_media_to_caption"].(map[string]interface{}); ok {
			if edgesArr, ok := capData["edges"].([]interface{}); ok && len(edgesArr) > 0 {
				if first, ok := edgesArr[0].(map[string]interface{}); ok {
					if n2, ok := first["node"].(map[string]interface{}); ok {
						caption, _ = n2["text"].(string)
					}
				}
			}
		}

		// Extract is_video
		isVideo, _ := node["is_video"].(bool)

		// Extract timestamp (taken_at_timestamp)
		ts, _ := node["taken_at_timestamp"].(float64)

		posts = append(posts, InstagramPost{
			Shortcode: sc,
			Caption:   caption,
			IsVideo:   isVideo,
			Timestamp: int64(ts),
		})
	}

	// Sort by timestamp descending (newest first)
	sort.Slice(posts, func(a, b int) bool {
		return posts[a].Timestamp > posts[b].Timestamp
	})

	return posts, nil
}

func sendDiscordWebhook(webhookURL string, displayName string, post InstagramPost) error {
	postType := "post"
	if post.IsVideo {
		postType = "Reel"
	}

	payload := map[string]interface{}{
		"content": fmt.Sprintf("⚠️ **%s** posted a new %s!\n📝 **Caption:** %s\n📎 **Link:** https://instagram.com/p/%s/\n⏰ **Time:** %s",
			displayName,
			postType,
			func() string {
				if len(post.Caption) > 200 {
					return post.Caption[:200] + "..."
				}
				return post.Caption
			}(),
			post.Shortcode,
			time.Unix(post.Timestamp, 0).Format("2006-01-02 15:04"),
		),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	resp, err := http.Post(webhookURL, "application/json", strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("POST webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func checkStoryAvailability(client *http.Client, username, shortcode string) error {
	// Quick HEAD request to the post URL to verify it's accessible
	// Stories from tattoo artists sometimes disappear after 1-2 weeks
	url := fmt.Sprintf("https://www.instagram.com/p/%s/", shortcode)

	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HEAD request: %w", err)
	}
	defer resp.Body.Close()

	// 404 means the post may have been deleted or is a vanished story
	if resp.StatusCode == http.StatusNotFound {
		log.Printf("WARNING: post %s/%s returned 404 (may have been deleted/story expired)", username, shortcode)
	} else if resp.StatusCode >= 400 {
		log.Printf("WARNING: post %s/%s returned %d", username, shortcode, resp.StatusCode)
	}

	return nil
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

	return state
}

func saveState(path string, state map[string]PostState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func init() {
	// Suppress unused import warnings for removed packages
	_ = strconv.Itoa(0) // kept for potential future use
}
