// Package instagram fetches public profile media for the one-time host
// media scrape (profile photo + recent posts). It uses Instagram's public
// web_profile_info endpoint — the same call instagram.com makes to render a
// profile page — so it works only for public profiles and is strictly
// best-effort: Instagram may rate-limit or block datacenter IPs, in which
// case callers must degrade gracefully.
package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	profileInfoEndpoint = "https://i.instagram.com/api/v1/users/web_profile_info/?username=%s"
	// x-ig-app-id of the instagram.com web client; required or the endpoint
	// returns a login redirect.
	webAppID  = "936619743392459"
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// Profile holds the public media extracted from an Instagram profile.
type Profile struct {
	Username      string
	ProfilePicURL string   // highest-resolution profile photo available
	RecentPosts   []string // display URLs of the most recent post images, newest first
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`)

// UsernameFromURL extracts the username from an Instagram profile URL or a
// bare "@handle"/"handle" string. Returns "" if none can be extracted.
func UsernameFromURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	candidate := strings.TrimPrefix(raw, "@")
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		if host != "instagram.com" && host != "instagr.am" {
			return ""
		}
		segments := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(segments) == 0 || segments[0] == "" {
			return ""
		}
		// Skip non-profile paths like /p/<post>, /reel/<id>, /explore/...
		switch segments[0] {
		case "p", "reel", "reels", "tv", "explore", "stories", "accounts":
			return ""
		}
		candidate = segments[0]
	}
	if !usernameRe.MatchString(candidate) {
		return ""
	}
	return candidate
}

// FetchProfile fetches a public profile's photo and up to maxPosts recent
// post images.
func FetchProfile(ctx context.Context, username string, maxPosts int) (*Profile, error) {
	if !usernameRe.MatchString(username) {
		return nil, fmt.Errorf("invalid instagram username %q", username)
	}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf(profileInfoEndpoint, url.QueryEscape(username)), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-IG-App-ID", webAppID)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch instagram profile %q: %w", username, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch instagram profile %q: status %d", username, resp.StatusCode)
	}

	var payload struct {
		Data struct {
			User struct {
				ProfilePicURL   string `json:"profile_pic_url"`
				ProfilePicURLHD string `json:"profile_pic_url_hd"`
				IsPrivate       bool   `json:"is_private"`
				Timeline        struct {
					Edges []struct {
						Node struct {
							DisplayURL string `json:"display_url"`
							IsVideo    bool   `json:"is_video"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"edge_owner_to_timeline_media"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 5<<20)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse instagram profile %q: %w", username, err)
	}

	user := payload.Data.User
	picURL := user.ProfilePicURLHD
	if picURL == "" {
		picURL = user.ProfilePicURL
	}
	if picURL == "" {
		return nil, fmt.Errorf("instagram profile %q not found or unavailable", username)
	}

	profile := &Profile{Username: username, ProfilePicURL: picURL}
	if user.IsPrivate {
		// Profile photo is still public; posts are not.
		return profile, nil
	}
	for _, edge := range user.Timeline.Edges {
		if len(profile.RecentPosts) >= maxPosts {
			break
		}
		if edge.Node.DisplayURL == "" {
			continue
		}
		// display_url is a static image even for video posts (the cover frame).
		profile.RecentPosts = append(profile.RecentPosts, edge.Node.DisplayURL)
	}
	return profile, nil
}
