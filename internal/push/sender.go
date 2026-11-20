package push

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"
)

type sendOptions struct {
	TTL   int
	Topic string
}

func (m *Manager) sendOne(ctx context.Context, sub Subscription, payload []byte, options sendOptions) error {
	wpSub := &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.Keys.P256dh,
			Auth:   sub.Keys.Auth,
		},
	}

	resp, err := webpush.SendNotificationWithContext(ctx, payload, wpSub, &webpush.Options{
		HTTPClient:      m.httpClient,
		Subscriber:      "https://github.com/0cv/herdr-mobile-relay",
		VAPIDPublicKey:  m.vapidPublic,
		VAPIDPrivateKey: m.vapidPrivate,
		TTL:             options.TTL,
		Topic:           options.Topic,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return &pushError{statusCode: resp.StatusCode}
}

type pushError struct {
	statusCode int
}

func (e *pushError) Error() string {
	return fmt.Sprintf("push service returned %d", e.statusCode)
}

// isTerminalError returns true only when the push endpoint is permanently gone.
// Authentication failures can be caused by relay-side VAPID configuration and
// must not destroy an otherwise valid subscription.
func isTerminalError(err error) bool {
	var pe *pushError
	if errors.As(err, &pe) {
		switch pe.statusCode {
		case http.StatusNotFound, http.StatusGone:
			return true
		}
	}
	return false
}
