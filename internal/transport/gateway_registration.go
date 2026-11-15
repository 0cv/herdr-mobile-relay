package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
	"github.com/coder/websocket"
)

// registerLink performs the JSON hello exchange that claims the relay id.
func (c *GatewayClient) registerLink(parent context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithTimeout(parent, gatewayHelloTimeout)
	defer cancel()

	messageType, rawHello, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read gateway hello: %w", err)
	}
	if messageType != websocket.MessageText {
		return errors.New("gateway hello was not a text frame")
	}
	var hello gatewaywire.ServerHello
	if err := json.Unmarshal(rawHello, &hello); err != nil {
		return fmt.Errorf("decode gateway hello: %w", err)
	}
	if hello.Type != gatewaywire.TypeServerHello {
		return fmt.Errorf("unexpected gateway hello type %q", hello.Type)
	}
	if hello.Proto != gatewaywire.Proto {
		return fmt.Errorf("unsupported gateway protocol %d, want %d", hello.Proto, gatewaywire.Proto)
	}
	c.setGatewayBuild(hello.Version, hello.Revision)
	c.setSTUNPort(hello.StunPort)

	registration := gatewaywire.RegisterHello{
		Type:    gatewaywire.TypeRegister,
		Proto:   gatewaywire.Proto,
		RelayID: c.relayID,
	}
	if c.registrationToken != "" {
		nonce, err := base64.RawURLEncoding.DecodeString(hello.Nonce)
		if err != nil || len(nonce) != gatewaywire.NonceBytes {
			return errors.New("gateway registration challenge is malformed")
		}
		registration.RegistrationProof = base64.RawURLEncoding.EncodeToString(
			gatewaywire.RegistrationProof(c.registrationToken, c.relayID, nonce))
	}
	register, err := json.Marshal(registration)
	if err != nil {
		return fmt.Errorf("encode register hello: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, register); err != nil {
		return fmt.Errorf("write register hello: %w", err)
	}

	messageType, rawReply, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read gateway registration reply: %w", err)
	}
	if messageType != websocket.MessageText {
		return errors.New("gateway registration reply was not a text frame")
	}
	var reply struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rawReply, &reply); err != nil {
		return fmt.Errorf("decode gateway registration reply: %w", err)
	}
	switch reply.Type {
	case gatewaywire.TypeReady:
		var ready gatewaywire.ReadyMessage
		if err := json.Unmarshal(rawReply, &ready); err != nil {
			return fmt.Errorf("decode gateway ready: %w", err)
		}
		if ready.Proto != gatewaywire.Proto {
			return fmt.Errorf("unsupported gateway protocol %d, want %d", ready.Proto, gatewaywire.Proto)
		}
		return nil
	case gatewaywire.TypeError:
		var rejection gatewaywire.ErrorMessage
		if err := json.Unmarshal(rawReply, &rejection); err != nil {
			return fmt.Errorf("decode gateway error: %w", err)
		}
		if rejection.Code == gatewaywire.CodeUnauthorized {
			rejection.Message = "registration token missing or wrong: set HERDR_GATEWAY_REGISTRATION_TOKEN to match the gateway; also check the gateway's HERDR_GATEWAY_ALLOWED_RELAY_IDS"
		}
		return &gatewayRejectedError{code: rejection.Code, message: rejection.Message}
	default:
		return fmt.Errorf("unexpected gateway registration reply %q", reply.Type)
	}
}

func (c *GatewayClient) setGatewayBuild(version, revision string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gatewayVersion = version
	c.gatewayRevision = revision
}

// setSTUNPort records an advertised address-discovery port. Anything outside
// the valid port range is a broken or hostile gateway, and disables discovery
// rather than being passed on.
func (c *GatewayClient) setSTUNPort(port int) {
	if port < 1 || port > 65535 {
		port = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stunPort = port
}

func (c *GatewayClient) markRegistered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registered = true
	c.lastError = ""
	c.connectedSince = time.Now().UTC()
}

func (c *GatewayClient) markUnregistered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.registered = false
	c.connectedSince = time.Time{}
}

func (c *GatewayClient) recordError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastError = err.Error()
}

func (c *GatewayClient) recordNotice(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastNotice = message
}
