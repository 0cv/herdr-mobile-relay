package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

// CurrentURL reports the gateway entry the client is registered with, or the one
// its next attempt will dial. Status output and address discovery follow it, so
// a failover is visible instead of pointing at a gateway we left behind.
func (c *GatewayClient) CurrentURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.urls[c.active]
}

// advance rotates to the next configured entry, wrapping at the end, so a dead
// gateway does not absorb every reconnect attempt.
func (c *GatewayClient) advance() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = (c.active + 1) % len(c.urls)
}

// activeFirstLocked returns the selected gateway followed by every cold
// fallback in configured order. The active entry leads the descriptor the
// phone stores, so it does not waste a failed connection on the next resume.
func (c *GatewayClient) activeFirstLocked() []string {
	ordered := make([]string, 0, len(c.urls))
	ordered = append(ordered, c.urls[c.active])
	for index, entry := range c.urls {
		if index != c.active {
			ordered = append(ordered, entry)
		}
	}
	return ordered
}

func (c *GatewayClient) activeIndex() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *GatewayClient) setActive(index int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = index
}

type gatewayProbeResult struct {
	index int
	rtt   time.Duration
	err   error
}

// latencyRanked reports whether measured round trip decides the winner. Only
// an interchangeable list, such as the community gateways, opts into it.
func (c *GatewayClient) latencyRanked() bool {
	return c.selection == config.GatewaySelectionLatency
}

// Selection reports the rule in force, so status output can answer "why that
// gateway" without the log.
func (c *GatewayClient) Selection() string {
	if c.latencyRanked() {
		return config.GatewaySelectionLatency
	}
	return config.GatewaySelectionOrdered
}

// selectGateway probes every candidate concurrently and commits to one healthy
// entry. A failed active entry may be excluded for one selection pass; this
// prevents a working /healthz route from immediately winning after its
// WebSocket registration path failed. Probing is identical in both selection
// modes: ordered still has to learn which entries answer at all.
func (c *GatewayClient) selectGateway(parent context.Context, excluded int) bool {
	candidates := make([]int, 0, len(c.urls))
	for index := range c.urls {
		if index != excluded {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	if len(candidates) == 1 {
		c.setActive(candidates[0])
		return true
	}

	ctx, cancel := context.WithTimeout(parent, c.probeTimeout)
	defer cancel()
	results := make(chan gatewayProbeResult, len(candidates))
	for _, index := range candidates {
		go func() {
			rtt, err := c.probe(ctx, c.urls[index])
			results <- gatewayProbeResult{index: index, rtt: rtt, err: err}
		}()
	}

	successful := make([]gatewayProbeResult, 0, len(candidates))
	for range candidates {
		select {
		case result := <-results:
			if result.err != nil {
				c.logger.Debug("gateway probe failed",
					"url", c.urls[result.index], "error", result.err)
				continue
			}
			successful = append(successful, result)
		case <-ctx.Done():
			return c.applyProbeResults(successful)
		}
	}
	return c.applyProbeResults(successful)
}

// applyProbeResults commits the winner among the healthy candidates. This is
// the only step the selection rule changes.
func (c *GatewayClient) applyProbeResults(results []gatewayProbeResult) bool {
	if len(results) == 0 {
		return false
	}
	if !c.latencyRanked() {
		return c.applyConfiguredOrder(results)
	}
	minRTT := results[0].rtt
	for _, result := range results[1:] {
		if result.rtt < minRTT {
			minRTT = result.rtt
		}
	}
	bestIndex := len(c.urls)
	var bestRTT time.Duration
	for _, result := range results {
		if result.rtt-minRTT <= gatewayProbeTieWindow && result.index < bestIndex {
			bestIndex, bestRTT = result.index, result.rtt
		}
	}
	c.setActive(bestIndex)
	c.logger.Info("gateway selected by latency", "url", c.urls[bestIndex], "rtt", bestRTT)
	return true
}

// applyConfiguredOrder takes the earliest healthy entry, however slow it is: an
// explicitly listed gateway is a priority, not a suggestion. The log names the
// position it landed on and how many entries answered, so "why is it not on my
// first gateway" is answerable from the log alone.
func (c *GatewayClient) applyConfiguredOrder(results []gatewayProbeResult) bool {
	best := results[0]
	for _, result := range results[1:] {
		if result.index < best.index {
			best = result
		}
	}
	c.setActive(best.index)
	c.logger.Info("gateway selected by configured order",
		"url", c.urls[best.index], "position", best.index+1, "candidates", len(c.urls),
		"healthy", len(results), "rtt", best.rtt)
	return true
}

// measureGatewayRTT performs one fresh request to the gateway's real health
// endpoint. This needs no ICMP privilege and includes DNS, TCP, TLS and HTTP in
// the number used for cold selection.
func measureGatewayRTT(ctx context.Context, base string) (time.Duration, error) {
	httpBase, err := gatewaywire.HTTPBase(base)
	if err != nil {
		return 0, err
	}
	target, err := url.Parse(httpBase)
	if err != nil {
		return 0, err
	}
	target.Path = gatewaywire.HealthPath
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""

	transport := &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	var health gatewaywire.HealthResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, gatewaywire.MaxHTTPResponseBytes)).Decode(&health); err != nil {
		return 0, fmt.Errorf("decode health response: %w", err)
	}
	if !health.OK {
		return 0, errors.New("health response is not ok")
	}
	return time.Since(started), nil
}
