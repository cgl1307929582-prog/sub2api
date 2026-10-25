package service

import (
	"context"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
)

// Observe frames before the relay emits its terminal usage callback. Embedding
// preserves writes/close and the frame bytes returned to the forwarding path.
type openAIWSObservedFrameConn struct {
	openaiwsv2.FrameConn
	observe func([]byte)
}

func (c *openAIWSObservedFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	kind, payload, err := c.FrameConn.ReadFrame(ctx)
	if err == nil && (kind == coderws.MessageText || kind == coderws.MessageBinary) {
		c.observe(payload)
	}
	return kind, payload, err
}
