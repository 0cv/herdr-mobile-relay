package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

const (
	wsMaxReadBytes         = 21 * 1024 * 1024
	wsSendTimeout          = 5 * time.Second
	wsCloseTimeout         = 1 * time.Second
	handlerCapacity        = 32
	orderedIngressCapacity = 128
)

type ClientConn struct {
	id       string
	conn     *websocket.Conn
	buf      *sendBuffer
	logger   *slog.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	closeOne sync.Once
}

func (c *ClientConn) ID() string { return c.id }

type MessageHandler func(client *ClientConn, msg map[string]any, admitted func())
type ConnectHandler func(client *ClientConn)

type inboundMessage struct {
	client  *ClientConn
	message map[string]any
}

type Metrics struct {
	IngressHighWater      uint64 `json:"ingress_high_water"`
	IngressRejected       uint64 `json:"ingress_rejected"`
	OutboundHighWaterItem uint64 `json:"outbound_high_water_items"`
	OutboundHighWaterByte uint64 `json:"outbound_high_water_bytes"`
	Coalesced             uint64 `json:"coalesced"`
	SlowClientEvictions   uint64 `json:"slow_client_evictions"`
	ConnectedClients      int    `json:"connected_clients"`
}

type Hub struct {
	cfg       *config.Config
	logger    *slog.Logger
	register  sync.Mutex
	mu        sync.RWMutex
	clients   map[string]*ClientConn
	nextID    int
	handler   MessageHandler
	onConnect ConnectHandler
	closing   bool

	receiptSequence atomic.Uint64
	receiptMu       sync.Mutex
	orderedIngress  chan inboundMessage
	handlerSlots    chan struct{}
	connectionWG    sync.WaitGroup
	handlerWG       sync.WaitGroup
	ingressDone     chan struct{}
	closeIngress    sync.Once

	ingressHighWater      atomic.Uint64
	ingressRejected       atomic.Uint64
	outboundHighWaterItem atomic.Uint64
	outboundHighWaterByte atomic.Uint64
	coalesced             atomic.Uint64
	slowClientEvictions   atomic.Uint64
}

func NewHub(cfg *config.Config, logger *slog.Logger) *Hub {
	hub := &Hub{
		cfg:            cfg,
		logger:         logger,
		clients:        make(map[string]*ClientConn),
		orderedIngress: make(chan inboundMessage, orderedIngressCapacity),
		handlerSlots:   make(chan struct{}, handlerCapacity),
		ingressDone:    make(chan struct{}),
	}
	go func() {
		hub.runOrderedIngress()
		close(hub.ingressDone)
	}()
	return hub
}

func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !Authenticate(h.cfg, r) {
		if h.cfg.Token != "" {
			http.Error(w, "Invalid token", http.StatusUnauthorized)
		} else {
			http.Error(w, "Origin not allowed", http.StatusForbidden)
		}
		return
	}

	h.mu.RLock()
	closing := h.closing
	h.mu.RUnlock()
	if closing {
		http.Error(w, "Relay is shutting down", http.StatusServiceUnavailable)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.logger.Warn("websocket accept failed", "error", err)
		return
	}
	conn.SetReadLimit(wsMaxReadBytes)

	ctx, cancel := context.WithCancel(r.Context())
	h.register.Lock()
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		h.register.Unlock()
		cancel()
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	h.nextID++
	clientID := fmt.Sprintf("client-%d", h.nextID)
	client := &ClientConn{
		id:     clientID,
		conn:   conn,
		buf:    newSendBuffer(clientOutboundMaxItems, clientOutboundMaxBytes),
		logger: h.logger.With("client_id", clientID),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	h.connectionWG.Add(1)
	defer h.connectionWG.Done()
	go h.writePump(client)
	h.mu.Unlock()
	if h.onConnect != nil {
		h.onConnect(client)
	}
	if client.ctx.Err() != nil {
		h.register.Unlock()
		conn.CloseNow()
		<-client.done
		return
	}
	h.mu.Lock()
	h.clients[clientID] = client
	h.mu.Unlock()
	h.register.Unlock()

	h.logger.Info("client connected", "client_id", clientID)
	h.readPump(client)
	h.removeClient(client)

	closeCtx, closeCancel := context.WithTimeout(context.Background(), wsCloseTimeout)
	defer closeCancel()
	closed := make(chan struct{})
	go func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		close(closed)
	}()
	select {
	case <-closed:
	case <-closeCtx.Done():
		conn.CloseNow()
	}
	<-client.done
}

func (h *Hub) readPump(client *ClientConn) {
	for {
		_, data, err := client.conn.Read(client.ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && client.ctx.Err() == nil {
				client.logger.Debug("read error", "error", err)
			}
			return
		}
		var message map[string]any
		if err := json.Unmarshal(data, &message); err != nil {
			continue
		}
		h.receiptMu.Lock()
		message["_server_sequence"] = h.receiptSequence.Add(1)
		message["_server_received_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		h.submitMessage(client, message)
		h.receiptMu.Unlock()
	}
}

func (h *Hub) submitMessage(client *ClientConn, message map[string]any) {
	select {
	case h.orderedIngress <- inboundMessage{client: client, message: message}:
		observeAtomicMax(&h.ingressHighWater, uint64(len(h.orderedIngress)))
	default:
		h.ingressRejected.Add(1)
		requestID, _ := message["request_id"].(string)
		action, _ := message["type"].(string)
		h.Send(client, map[string]any{
			"type":       "command_result",
			"request_id": requestID,
			"action":     action,
			"ok":         false,
			"phase":      "not_started",
			"error":      "Relay is busy; command was not sent",
		})
	}
}

func (h *Hub) runOrderedIngress() {
	for inbound := range h.orderedIngress {
		if inbound.client.ctx.Err() != nil {
			continue
		}
		h.handlerSlots <- struct{}{}
		admitted := make(chan struct{})
		var admittedOnce sync.Once
		signal := func() { admittedOnce.Do(func() { close(admitted) }) }
		h.handlerWG.Add(1)
		go func() {
			defer h.handlerWG.Done()
			defer func() { <-h.handlerSlots }()
			defer signal()
			if h.handler != nil && inbound.client.ctx.Err() == nil {
				h.handler(inbound.client, inbound.message, signal)
			}
		}()
		select {
		case <-admitted:
		case <-inbound.client.ctx.Done():
		}
	}
}

func (h *Hub) writePump(client *ClientConn) {
	defer close(client.done)
	for {
		data, ok := client.buf.Pop()
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(client.ctx, wsSendTimeout)
		err := client.conn.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			client.logger.Debug("write failed, evicting", "error", err)
			h.removeClient(client)
			return
		}
	}
}

func (h *Hub) Send(client *ClientConn, message any) bool {
	data, kind, replaceable, err := encodeMessage(message)
	if err != nil {
		return false
	}
	if !h.push(client, data, kind, replaceable) {
		client.logger.Warn("send buffer full, evicting client")
		h.slowClientEvictions.Add(1)
		h.removeClient(client)
		return false
	}
	return true
}

func (h *Hub) SendByID(clientID string, message any) bool {
	h.mu.RLock()
	client := h.clients[clientID]
	h.mu.RUnlock()
	if client == nil {
		return false
	}
	return h.Send(client, message)
}

func (h *Hub) Broadcast(message any) {
	data, kind, replaceable, err := encodeMessage(message)
	if err != nil {
		return
	}
	h.register.Lock()
	defer h.register.Unlock()
	h.mu.RLock()
	clients := make([]*ClientConn, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.RUnlock()
	for _, client := range clients {
		if !h.push(client, data, kind, replaceable) {
			h.slowClientEvictions.Add(1)
			h.removeClient(client)
		}
	}
}

// BroadcastPrepared applies a logical state update under the same registration
// barrier used by onConnect, then snapshots recipients. A client therefore
// observes the update either in its handshake snapshot or as a live delta,
// never both and never neither.
func (h *Hub) BroadcastPrepared(message any, prepare func()) {
	data, kind, replaceable, err := encodeMessage(message)
	if err != nil {
		return
	}
	h.register.Lock()
	defer h.register.Unlock()
	if prepare != nil {
		prepare()
	}
	h.mu.RLock()
	clients := make([]*ClientConn, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.RUnlock()
	for _, client := range clients {
		if !h.push(client, data, kind, replaceable) {
			h.slowClientEvictions.Add(1)
			h.removeClient(client)
		}
	}
}

func (h *Hub) push(client *ClientConn, data []byte, kind string, replaceable bool) bool {
	result := client.buf.pushTyped(data, kind, replaceable)
	if result == pushRejected {
		return false
	}
	if result == pushCoalesced {
		h.coalesced.Add(1)
	}
	observeAtomicMax(&h.outboundHighWaterItem, uint64(client.buf.Len()))
	observeAtomicMax(&h.outboundHighWaterByte, uint64(client.buf.Bytes()))
	return true
}

func encodeMessage(message any) ([]byte, string, bool, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, "", false, err
	}
	kind := messageType(data)
	replaceable := kind == "agents" || kind == "inventory_status" || kind == "update_status" || kind == "app_deploy_status"
	return data, kind, replaceable, nil
}

func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) Metrics() Metrics {
	return Metrics{
		IngressHighWater:      h.ingressHighWater.Load(),
		IngressRejected:       h.ingressRejected.Load(),
		OutboundHighWaterItem: h.outboundHighWaterItem.Load(),
		OutboundHighWaterByte: h.outboundHighWaterByte.Load(),
		Coalesced:             h.coalesced.Load(),
		SlowClientEvictions:   h.slowClientEvictions.Load(),
		ConnectedClients:      h.ClientCount(),
	}
}

func observeAtomicMax(target *atomic.Uint64, value uint64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

func (h *Hub) removeClient(client *ClientConn) {
	client.closeOne.Do(func() {
		h.mu.Lock()
		delete(h.clients, client.id)
		h.mu.Unlock()
		client.cancel()
		client.buf.Close()
		h.logger.Info("client disconnected", "client_id", client.id)
	})
}

func (h *Hub) SetHandler(fn MessageHandler)   { h.handler = fn }
func (h *Hub) SetOnConnect(fn ConnectHandler) { h.onConnect = fn }

func (h *Hub) CloseAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Shutdown(ctx)
}

func (h *Hub) Shutdown(ctx context.Context) error {
	h.register.Lock()
	h.mu.Lock()
	h.closing = true
	clients := make([]*ClientConn, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
		delete(h.clients, client.id)
	}
	h.mu.Unlock()
	h.register.Unlock()

	for _, client := range clients {
		go func(c *ClientConn) {
			closeCtx, cancel := context.WithTimeout(context.Background(), wsCloseTimeout)
			defer cancel()
			done := make(chan struct{})
			go func() {
				_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
				close(done)
			}()
			select {
			case <-done:
			case <-closeCtx.Done():
				c.conn.CloseNow()
			}
			h.removeClient(c)
		}(client)
	}

	if err := waitGroup(ctx, &h.connectionWG); err != nil {
		for _, client := range clients {
			client.conn.CloseNow()
		}
		return err
	}
	h.closeIngress.Do(func() { close(h.orderedIngress) })
	select {
	case <-h.ingressDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	return waitGroup(ctx, &h.handlerWG)
}

func waitGroup(ctx context.Context, group *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
