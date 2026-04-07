package ws_ai_pro

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterFactory("ws_ai_pro", func(cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
		return NewHTTPAPIChannel(cfg.Channels.WSAIPro, b)
	})
}
