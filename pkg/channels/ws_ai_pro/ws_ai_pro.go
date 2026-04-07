package ws_ai_pro

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"

	"golang.org/x/net/websocket"
)

const channelName = "ws_ai_pro"

// --- Wire protocol (JSON over WebSocket) ---

// ClientMessage is sent by the Electron app to picoclaw.
type ClientMessage struct {
	// Type: "chat" (send a message) or "ping" (keep-alive)
	Type string `json:"type"`
	// Unique request ID — response will echo this back for correlation
	ID string `json:"id,omitempty"`
	// Chat session identifier (maps to picoclaw session/chatID)
	Session string `json:"session,omitempty"`
	// The user message content
	Content string `json:"content,omitempty"`
	// If true, disable all tool calls for this request (pure LLM response)
	NoTools bool `json:"no_tools,omitempty"`
	// Bearer token for API proxy authentication (JWT from Electron).
	// When auth_method=jwt is configured, this token is forwarded as-is
	// to the upstream API proxy in the Authorization header.
	Token string `json:"token,omitempty"`
}

// ServerMessage is sent by picoclaw back to the Electron app.
type ServerMessage struct {
	// Type: "reply" | "pong" | "error"
	Type string `json:"type"`
	// Echoed request ID for correlation
	ID string `json:"id,omitempty"`
	// The agent's reply content
	Content string `json:"content,omitempty"`
	// Error message (when Type == "error")
	Error string `json:"error,omitempty"`
}

// --- Channel implementation ---

// pendingRequest tracks a single in-flight chat request.
type pendingRequest struct {
	done chan string      // receives the final complete response
	ws   *websocket.Conn // WebSocket connection for streaming chunks
	id   string          // request ID for message correlation
	mu   sync.Mutex      // protects WebSocket writes
}

type HTTPAPIChannel struct {
	*channels.BaseChannel
	config   config.WSAIProConfig
	server   *http.Server
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc

	// Active WebSocket clients, keyed by connection pointer
	clients    map[*websocket.Conn]bool
	clientsMux sync.RWMutex

	// Pending responses: chatID → pending request.
	// When Send() is called, we find the matching pending request by chatID
	// and deliver the final response.
	pending    map[string]*pendingRequest
	pendingMux sync.Mutex
}

func NewHTTPAPIChannel(cfg config.WSAIProConfig, msgBus *bus.MessageBus) (*HTTPAPIChannel, error) {
	base := channels.NewBaseChannel(
		channelName,
		cfg,
		msgBus,
		cfg.AllowFrom,
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &HTTPAPIChannel{
		BaseChannel: base,
		config:      cfg,
		clients:     make(map[*websocket.Conn]bool),
		pending:     make(map[string]*pendingRequest),
	}, nil
}

func (c *HTTPAPIChannel) Start(ctx context.Context) error {
	logger.InfoC(channelName, "Starting HTTP API channel")

	c.ctx, c.cancel = context.WithCancel(ctx)

	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", c.handleHealth)
	mux.Handle("/ws", &websocket.Server{
		Handler:   c.handleWebSocket,
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
	})

	var err error
	c.listener, err = net.Listen("tcp", addr)
	if err != nil {
		c.cancel()
		return fmt.Errorf("httpapi: failed to listen on %s: %w", addr, err)
	}

	c.server = &http.Server{Handler: mux}
	c.SetRunning(true)

	logger.InfoCF(channelName, "HTTP API channel listening", map[string]any{
		"host": c.config.Host,
		"port": c.config.Port,
	})

	go func() {
		if err := c.server.Serve(c.listener); err != nil && err != http.ErrServerClosed {
			logger.ErrorCF(channelName, "Server error", map[string]any{"error": err.Error()})
		}
	}()

	return nil
}

func (c *HTTPAPIChannel) Stop(ctx context.Context) error {
	logger.InfoC(channelName, "Stopping HTTP API channel")
	c.SetRunning(false)

	if c.cancel != nil {
		c.cancel()
	}

	if c.server != nil {
		c.server.Shutdown(ctx)
	}

	c.clientsMux.Lock()
	for conn := range c.clients {
		conn.Close()
	}
	c.clients = make(map[*websocket.Conn]bool)
	c.clientsMux.Unlock()

	logger.InfoC(channelName, "HTTP API channel stopped")
	return nil
}

// Send is called by the channel manager when the agent produces a response.
// We look up the pending request by chatID and deliver the reply.
func (c *HTTPAPIChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}

	c.pendingMux.Lock()
	pr, ok := c.pending[msg.ChatID]
	if ok {
		delete(c.pending, msg.ChatID)
	}
	c.pendingMux.Unlock()

	if ok {
		// Signal the waiting handler that the response is complete
		select {
		case pr.done <- msg.Content:
		default:
		}
		return nil, nil
	}

	// No pending request — broadcast to all connected WS clients as unsolicited message
	reply := ServerMessage{Type: "reply", Content: msg.Content}
	c.broadcast(reply)
	return nil, nil
}

// --- WebSocket handler ---

func (c *HTTPAPIChannel) handleWebSocket(ws *websocket.Conn) {
	c.clientsMux.Lock()
	c.clients[ws] = true
	c.clientsMux.Unlock()

	logger.InfoCF(channelName, "WebSocket client connected", map[string]any{
		"remote": ws.Request().RemoteAddr,
	})

	defer func() {
		c.clientsMux.Lock()
		delete(c.clients, ws)
		c.clientsMux.Unlock()
		ws.Close()
		logger.InfoC(channelName, "WebSocket client disconnected")
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		var msg ClientMessage
		if err := websocket.JSON.Receive(ws, &msg); err != nil {
			if err.Error() != "EOF" {
				logger.ErrorCF(channelName, "WS read error", map[string]any{"error": err.Error()})
			}
			return
		}

		switch msg.Type {
		case "ping":
			websocket.JSON.Send(ws, ServerMessage{Type: "pong", ID: msg.ID})

		case "chat":
			go c.handleChatMessage(ws, msg)

		default:
			websocket.JSON.Send(ws, ServerMessage{
				Type:  "error",
				ID:    msg.ID,
				Error: fmt.Sprintf("unknown message type: %s", msg.Type),
			})
		}
	}
}

func (c *HTTPAPIChannel) handleChatMessage(ws *websocket.Conn, msg ClientMessage) {
	chatID := msg.Session
	if chatID == "" {
		chatID = randomID()
	}

	// Create a pending request with streaming support
	pr := &pendingRequest{
		done: make(chan string, 1),
		ws:   ws,
		id:   msg.ID,
	}

	c.pendingMux.Lock()
	c.pending[chatID] = pr
	c.pendingMux.Unlock()

	// Stream callback: sends each LLM token chunk to the WebSocket client
	streamCallback := func(chunk string) {
		pr.mu.Lock()
		defer pr.mu.Unlock()
		websocket.JSON.Send(ws, ServerMessage{
			Type:    "chunk",
			ID:      msg.ID,
			Content: chunk,
		})
	}

	// Build and publish inbound message directly (with StreamCallback attached)
	sender := bus.SenderInfo{
		Platform:    channelName,
		PlatformID:  "ws-client",
		CanonicalID: identity.BuildCanonicalID(channelName, "ws-client"),
		Username:    "ws-client",
		DisplayName: "WS Client",
	}

	metadata := map[string]string{"request_id": msg.ID}
	if msg.NoTools {
		metadata["no_tools"] = "true"
	}
	if msg.Token != "" {
		metadata["bearer_token"] = msg.Token
	}

	// If client provides a session key, prefix it with "agent:" so PicoClaw's
	// resolveScopeKey preserves it as-is (stable session across messages).
	sessionKey := ""
	if msg.Session != "" {
		sessionKey = "agent:" + msg.Session
	}

	inbound := bus.InboundMessage{
		Channel:        channelName,
		SenderID:       sender.CanonicalID,
		Sender:         sender,
		ChatID:         chatID,
		Content:        msg.Content,
		Media:          []string{},
		Peer:           bus.Peer{Kind: "direct", ID: chatID},
		MessageID:      msg.ID,
		SessionKey:     sessionKey,
		Metadata:       metadata,
		StreamCallback: streamCallback,
	}

	if err := c.GetBus().PublishInbound(c.ctx, inbound); err != nil {
		websocket.JSON.Send(ws, ServerMessage{
			Type:  "error",
			ID:    msg.ID,
			Error: "failed to publish message",
		})
		return
	}

	// Wait for the agent to finish (Send() signals completion)
	select {
	case finalContent := <-pr.done:
		pr.mu.Lock()
		// If the LLM didn't stream (no chunks were sent via callback),
		// send the full content as a final chunk so the client receives it.
		if finalContent != "" {
			websocket.JSON.Send(ws, ServerMessage{
				Type:    "chunk",
				ID:      msg.ID,
				Content: finalContent,
			})
		}
		// Send a "done" signal so the client knows the response is complete.
		websocket.JSON.Send(ws, ServerMessage{Type: "done", ID: msg.ID})
		pr.mu.Unlock()
	case <-c.ctx.Done():
		pr.mu.Lock()
		websocket.JSON.Send(ws, ServerMessage{
			Type:  "error",
			ID:    msg.ID,
			Error: "channel shutting down",
		})
		pr.mu.Unlock()
	}
}

// --- Health endpoint ---

func (c *HTTPAPIChannel) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"channel": channelName,
	})
}

// --- Helpers ---

func (c *HTTPAPIChannel) broadcast(msg ServerMessage) {
	c.clientsMux.RLock()
	defer c.clientsMux.RUnlock()

	for conn := range c.clients {
		if err := websocket.JSON.Send(conn, msg); err != nil {
			logger.ErrorCF(channelName, "Broadcast send error", map[string]any{"error": err.Error()})
		}
	}
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
