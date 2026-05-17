// captcha_test — standalone Mac binary that runs the FULL VK captcha-solve
// flow (bootstrap login.vk.ru → calls.getAnonymousToken → solveCaptchaPoW)
// using the EXACT same Go code paths as the iOS extension, but from a Mac
// process instead of inside iOS NetworkExtension.
//
// Purpose: discriminate "iOS environment introduces a detection signal VK
// catches" from "our Go code is wrong". If this Mac binary gets a
// success_token, our Go code is fine and the BOT response on iOS is
// iOS-specific (TCP fingerprint, Network Extension peculiarities, or
// similar). If this Mac binary also gets BOT, the problem is in our Go
// code's request structure regardless of environment.
//
// Usage:
//
//	go build -o /tmp/captcha-test ./tools/captcha_test
//	./tmp/captcha-test \
//	    --vk-link "https://vk.ru/call/join/<linkID>" \
//	    --vk-profile /path/to/vk_profile.json
//
// --vk-profile is optional. Accepts either:
//   - the bare vk_profile.json content (top-level browser_fp/device/user_agent fields), OR
//   - a vkturnproxy-backup-*.json full backup (auto-detects and extracts vk_profile inner object).
//
// Without --vk-profile, captcha_pow.go falls back to generated browser_fp
// + canned device descriptor, which empirically gets BOT instantly even
// before VK's 2026-05-15 update. Almost always you want to pass the
// captured profile for a meaningful test.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cacggghp/vk-turn-proxy/pkg/proxy"
)

func main() {
	vkLink := flag.String("vk-link", "", "VK call link, e.g. https://vk.ru/call/join/<linkID>")
	profilePath := flag.String("vk-profile", "", "path to vk_profile.json OR a vkturnproxy-backup-*.json (auto-detect)")
	timeout := flag.Duration("timeout", 90*time.Second, "overall timeout")
	flag.Parse()

	if *vkLink == "" {
		log.Fatal("--vk-link is required (e.g. https://vk.ru/call/join/<linkID>)")
	}

	linkID, err := extractLinkID(*vkLink)
	if err != nil {
		log.Fatalf("parse --vk-link: %v", err)
	}
	log.Printf("test: linkID=%s", linkID)

	if *profilePath != "" {
		if err := loadProfileFromPath(*profilePath); err != nil {
			log.Fatalf("load --vk-profile: %v", err)
		}
	} else {
		log.Printf("test: no --vk-profile flag — solver will use generated fp+device (almost certainly BOT)")
	}

	log.Printf("test: starting GetVKCreds (full bootstrap + Go captcha solver, no WebView fallback)")
	log.Printf("test: this calls login.vk.ru/?act=get_anonym_token → api.vk.ru/method/calls.getAnonymousToken")
	log.Printf("test: on captcha demand, solveCaptchaPoW runs (Phase 11.1 with all preflight calls)")

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Channel to receive result + run GetVKCreds in goroutine so we can
	// observe ctx timeout cleanly.
	type result struct {
		creds *proxy.TURNCreds
		err   error
	}
	resCh := make(chan result, 1)
	go func() {
		// nil captchaSolver → on Go-solver failure VK error surfaces as
		// CaptchaRequiredError; we report it rather than blocking on a
		// WebView that doesn't exist outside iOS app.
		creds, err := proxy.GetVKCreds(linkID, nil, "", "", 0, 0, "", "")
		resCh <- result{creds, err}
	}()

	select {
	case <-ctx.Done():
		log.Fatalf("test: TIMEOUT after %s", *timeout)
	case r := <-resCh:
		if r.err != nil {
			var captchaErr *proxy.CaptchaRequiredError
			if errors.As(r.err, &captchaErr) {
				log.Printf("test: RESULT=CAPTCHA_REQUIRED — Go solver failed (likely BOT)")
				log.Printf("test: captcha_sid=%s, image=%s, isRateLimit=%v",
					captchaErr.SID, captchaErr.ImageURL, captchaErr.IsRateLimit)
				log.Printf("test: this is what iOS extension sees → falls back to WebView in production")
				os.Exit(2)
			}
			log.Fatalf("test: RESULT=ERROR — bootstrap/network error: %v", r.err)
		}
		log.Printf("test: RESULT=SUCCESS — got TURN creds!")
		log.Printf("test:   username=%s", r.creds.Username)
		log.Printf("test:   password=%s", maskPass(r.creds.Password))
		log.Printf("test:   address=%s", r.creds.Address)
		log.Printf("test:   addresses=%v", r.creds.Addresses)
		log.Printf("test: this means the Go captcha solver WORKS when run outside iOS NetworkExtension")
		log.Printf("test: implies the BOT response we see on iPhone is iOS-environment-specific")
	}
}

func extractLinkID(vkLink string) (string, error) {
	// Accept "https://vk.ru/call/join/<linkID>", "vk.com/call/join/<linkID>",
	// or just bare <linkID>.
	for _, prefix := range []string{
		"https://vk.ru/call/join/",
		"http://vk.ru/call/join/",
		"https://vk.com/call/join/",
		"http://vk.com/call/join/",
		"vk.ru/call/join/",
		"vk.com/call/join/",
	} {
		if strings.HasPrefix(vkLink, prefix) {
			id := strings.TrimPrefix(vkLink, prefix)
			id = strings.TrimRight(id, "/")
			if i := strings.IndexAny(id, "?#"); i > 0 {
				id = id[:i]
			}
			return id, nil
		}
	}
	if !strings.Contains(vkLink, "/") && len(vkLink) > 8 {
		return vkLink, nil
	}
	return "", fmt.Errorf("unrecognized vk-link format: %s", vkLink)
}

// loadProfileFromPath reads the given file. If it's a full backup file
// (has "vk_profile" key at top level), extracts the inner profile object
// and writes it to a temp file in the bare vk_profile.json format the
// proxy package's loadSavedVKProfile expects. Otherwise treats the file
// as the bare profile and points captcha_pow at it directly.
func loadProfileFromPath(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	// Try parsing as backup format (top-level "vk_profile" key)
	var backupShape struct {
		VKProfile json.RawMessage `json:"vk_profile"`
	}
	if err := json.Unmarshal(data, &backupShape); err == nil && len(backupShape.VKProfile) > 0 {
		// Backup format — extract inner profile, write to temp file
		tmpPath := "/tmp/vk_profile_standalone.json"
		if err := os.WriteFile(tmpPath, backupShape.VKProfile, 0644); err != nil {
			return fmt.Errorf("write tmp profile: %w", err)
		}
		proxy.SetVKProfilePath(tmpPath)
		// Sanity-log the extracted profile keys
		var profile map[string]interface{}
		_ = json.Unmarshal(backupShape.VKProfile, &profile)
		log.Printf("test: extracted vk_profile from backup — browser_fp=%s, ua=%s",
			truncate(toString(profile["browser_fp"]), 40),
			truncate(toString(profile["user_agent"]), 60))
		log.Printf("test: vk_profile written to %s", tmpPath)
		return nil
	}

	// Bare profile file — use directly
	proxy.SetVKProfilePath(path)
	log.Printf("test: using vk_profile.json directly: %s", path)
	return nil
}

func maskPass(p string) string {
	if len(p) < 8 {
		return "[" + fmt.Sprintf("%d", len(p)) + " chars]"
	}
	return p[:3] + "..." + p[len(p)-3:] + " (" + fmt.Sprintf("%d chars", len(p)) + ")"
}

func toString(v interface{}) string {
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
