package controller

import (
	"context"
	"encoding/json"
	"github.com/TwoD97/relay/internal/conversations"
	runtimeServer "github.com/TwoD97/relay/internal/runtime"
	"path/filepath"
)

func localConversations(ctx context.Context, socket string, request conversations.Request) ([]byte, error) {
	result, err := runtimeServer.ConversationOperation(ctx, filepath.Dir(filepath.Dir(socket)), request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
