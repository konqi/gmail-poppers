// Package gatus pushes run results to Gatus external endpoints.
package gatus

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gmail-poppers/internal/config"
	"gmail-poppers/internal/status"
)

const (
	ModePull = "pull"
	ModePush = "push"
	ModeBoth = "both"

	maxErrorLen = 1000
)

func ValidMode(m string) bool { return m == ModePull || m == ModePush || m == ModeBoth }

func PushEnabled(m string) bool { return m == ModePush || m == ModeBoth }

func PullEnabled(m string) bool { return m == ModePull || m == ModeBoth }

var sanitizer = strings.NewReplacer("/", "-", "_", "-", ".", "-", ",", "-", " ", "-", "#", "-", "+", "-", "&", "-")

// Key mirrors Gatus' endpoint key format: <group>_<name>.
func Key(group, name string) string {
	s := func(v string) string { return sanitizer.Replace(strings.ToLower(strings.TrimSpace(v))) }
	return s(group) + "_" + s(name)
}

type Pusher struct {
	BaseURL string
	Token   string
	Client  *http.Client
	Log     *slog.Logger
}

func (p *Pusher) Report(ctx context.Context, e status.Entry) {
	if err := p.push(ctx, e); err != nil {
		p.Log.Warn("gatus push failed", "account", e.Account, "source", e.Source, "error", err)
	}
}

func (p *Pusher) push(ctx context.Context, e status.Entry) error {
	q := url.Values{}
	q.Set("success", fmt.Sprint(e.Error == ""))
	if e.Error != "" {
		msg := e.Error
		if len(msg) > maxErrorLen {
			msg = msg[:maxErrorLen] + "..."
		}
		q.Set("error", msg)
	}
	q.Set("duration", e.Duration.Round(time.Millisecond).String())
	u := strings.TrimRight(p.BaseURL, "/") + "/api/v1/endpoints/" + url.PathEscape(Key(e.Account, e.Source)) + "/external?" + q.Encode()

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// RenderExternalEndpoints returns a Gatus config snippet for all sources.
func RenderExternalEndpoints(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("external-endpoints:\n")
	for _, s := range cfg.Sources() {
		fmt.Fprintf(&b, "  - name: %q\n", s.Name)
		fmt.Fprintf(&b, "    group: %q\n", s.Account)
		b.WriteString("    token: \"${GMAIL_POPPERS_GATUS_TOKEN}\"\n")
		b.WriteString("    heartbeat:\n")
		fmt.Fprintf(&b, "      interval: %s\n", 3*s.Interval+s.Timeout)
	}
	return b.String()
}

// RenderPullEndpoints returns a Gatus config snippet that polls the status endpoints.
func RenderPullEndpoints(cfg *config.Config, baseURL string) string {
	var b strings.Builder
	b.WriteString("endpoints:\n")
	for _, s := range cfg.Sources() {
		fmt.Fprintf(&b, "  - name: %q\n", s.Name)
		fmt.Fprintf(&b, "    group: %q\n", s.Account)
		fmt.Fprintf(&b, "    url: %q\n", strings.TrimRight(baseURL, "/")+"/status/"+url.PathEscape(s.Account)+"/"+url.PathEscape(s.Name))
		fmt.Fprintf(&b, "    interval: %s\n", s.Interval)
		b.WriteString("    conditions:\n")
		b.WriteString("      - \"[STATUS] == 200\"\n")
		b.WriteString("      - \"[BODY].message == ok\"\n")
	}
	return b.String()
}
