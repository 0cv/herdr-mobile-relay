package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// pythonSubscription matches the Python relay's on-disk format:
// {"subscriptions":[{"subscription":{endpoint,keys},"client_id":"...","user_agent":"..."}]}
type pythonSubscription struct {
	Subscription struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	} `json:"subscription"`
	DeviceID  string   `json:"device_id,omitempty"`
	Locale    string   `json:"locale,omitempty"`
	Platform  Platform `json:"platform,omitempty"`
	ClientID  string   `json:"client_id"`
	UserAgent string   `json:"user_agent"`
}

type pythonFile struct {
	Subscriptions []pythonSubscription `json:"subscriptions"`
}

func validPushEndpoint(raw string) bool {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil ||
		endpoint.Hostname() == "" || endpoint.Fragment != "" {
		return false
	}
	if port := endpoint.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), "."))
	switch host {
	case "fcm.googleapis.com", "android.googleapis.com", "updates.push.services.mozilla.com",
		"push.services.mozilla.com", "web.push.apple.com":
		return true
	default:
		return host == "notify.windows.com" || strings.HasSuffix(host, ".notify.windows.com")
	}
}

func keepValidPushSubscriptions(subscriptions []Subscription) []Subscription {
	valid := subscriptions[:0]
	for _, subscription := range subscriptions {
		if validPushEndpoint(subscription.Endpoint) {
			valid = append(valid, subscription)
		}
	}
	return valid
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read subscriptions: %w", err)
	}

	var pf pythonFile
	if err := json.Unmarshal(data, &pf); err != nil {
		m.logger.Warn("parse subscriptions; starting with no subscriptions", "error", err)
		return nil
	}
	m.subscriptions = make([]Subscription, 0, len(pf.Subscriptions))
	for _, ps := range pf.Subscriptions {
		var sub Subscription
		sub.Endpoint = ps.Subscription.Endpoint
		sub.Keys.P256dh = ps.Subscription.Keys.P256dh
		sub.Keys.Auth = ps.Subscription.Keys.Auth
		sub.DeviceID = ps.DeviceID
		sub.Locale = ps.Locale
		sub.Platform = ps.Platform
		sub.UserAgent = ps.UserAgent
		sub.ClientID = ps.ClientID
		m.subscriptions = append(m.subscriptions, sub)
	}
	m.subscriptions = keepValidPushSubscriptions(m.subscriptions)
	return nil
}

func encodeSubscriptions(subscriptions []Subscription) ([]byte, error) {
	pf := pythonFile{Subscriptions: make([]pythonSubscription, 0, len(subscriptions))}
	for _, sub := range subscriptions {
		var ps pythonSubscription
		ps.Subscription.Endpoint = sub.Endpoint
		ps.Subscription.Keys.P256dh = sub.Keys.P256dh
		ps.Subscription.Keys.Auth = sub.Keys.Auth
		ps.DeviceID = sub.DeviceID
		ps.Locale = sub.Locale
		ps.Platform = sub.Platform
		ps.UserAgent = sub.UserAgent
		ps.ClientID = sub.ClientID
		pf.Subscriptions = append(pf.Subscriptions, ps)
	}

	data, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (m *Manager) persist(subscriptions []Subscription) error {
	data, err := encodeSubscriptions(subscriptions)
	if err != nil {
		return err
	}
	return atomicWrite(m.path, data, 0o600)
}

func (m *Manager) Subscribe(sub Subscription, replacementSets ...[]string) error {
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return fmt.Errorf("subscription endpoint and keys are required")
	}
	if !validPushEndpoint(sub.Endpoint) {
		return errors.New("push_subscription_endpoint_not_allowed")
	}
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()

	var replaceEndpoints []string
	if len(replacementSets) > 0 {
		replaceEndpoints = replacementSets[0]
	}
	replace := make(map[string]bool, len(replaceEndpoints))
	for _, endpoint := range replaceEndpoints {
		if endpoint != "" {
			replace[endpoint] = true
		}
	}
	filtered := make([]Subscription, 0, len(m.subscriptions)+1)
	for _, existing := range m.subscriptions {
		if replace[existing.Endpoint] && existing.Endpoint != sub.Endpoint && existing.DeviceID == sub.DeviceID {
			continue
		}
		filtered = append(filtered, existing)
	}
	for i, existing := range filtered {
		if existing.Endpoint != sub.Endpoint {
			continue
		}
		if existing.DeviceID != sub.DeviceID {
			return errors.New("push_subscription_device_mismatch")
		}
		filtered[i] = sub
		if err := m.persist(filtered); err != nil {
			return err
		}
		replacements := append(append([]string(nil), replaceEndpoints...), sub.Endpoint)
		if err := m.queue.replaceSubscriptionsWhileProcessing(sub.DeviceID, replacements, sub); err != nil {
			_ = m.persist(m.subscriptions)
			return err
		}
		m.subscriptions = filtered
		return nil
	}
	filtered = append(filtered, sub)
	if err := m.persist(filtered); err != nil {
		return err
	}
	if err := m.queue.replaceSubscriptionsWhileProcessing(sub.DeviceID, replaceEndpoints, sub); err != nil {
		_ = m.persist(m.subscriptions)
		return err
	}
	m.subscriptions = filtered
	return nil
}

func (m *Manager) UnsubscribeDevice(deviceID string, endpoints []string, clientID string) error {
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("push_device_required")
	}
	remove := make(map[string]bool, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint != "" {
			remove[endpoint] = true
		}
	}
	m.mu.Lock()
	filtered := make([]Subscription, 0, len(m.subscriptions))
	removed := make([]Subscription, 0)
	for _, subscription := range m.subscriptions {
		match := subscription.DeviceID == deviceID &&
			(remove[subscription.Endpoint] || (clientID != "" && subscription.ClientID == clientID))
		if match {
			removed = append(removed, subscription)
			continue
		}
		filtered = append(filtered, subscription)
	}
	if err := m.persist(filtered); err != nil {
		m.mu.Unlock()
		return err
	}
	m.subscriptions = filtered
	m.mu.Unlock()
	if len(removed) == 0 {
		return nil
	}
	removedEndpoints := make([]string, 0, len(removed))
	for _, subscription := range removed {
		removedEndpoints = append(removedEndpoints, subscription.Endpoint)
	}
	return m.queue.removeSubscriptions(deviceID, removedEndpoints)
}
func (m *Manager) RemoveDevice(deviceID string) error {
	if strings.TrimSpace(deviceID) == "" {
		return errors.New("push device is required")
	}
	m.mu.Lock()
	filtered := make([]Subscription, 0, len(m.subscriptions))
	for _, subscription := range m.subscriptions {
		if subscription.DeviceID != deviceID {
			filtered = append(filtered, subscription)
		}
	}
	if err := m.persist(filtered); err != nil {
		m.mu.Unlock()
		return err
	}
	m.subscriptions = filtered
	for key := range m.active {
		if key.DeviceID == deviceID {
			delete(m.active, key)
		}
	}
	for key := range m.retracting {
		if key.DeviceID == deviceID {
			delete(m.retracting, key)
		}
	}
	m.mu.Unlock()
	if err := m.queue.removeDevice(deviceID); err != nil {
		return err
	}
	return m.policy.RemoveDevice(deviceID)
}

func (m *Manager) Subscriptions() []Subscription {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Subscription, len(m.subscriptions))
	copy(result, m.subscriptions)
	return result
}

func (m *Manager) VAPIDPublicKey() string {
	return m.vapidPublic
}
