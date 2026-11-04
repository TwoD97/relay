package controller

import (
	"context"
	"errors"
	"github.com/TwoD97/relay/internal/conversations"
)

func localConversations(context.Context, string, conversations.Request) ([]byte, error) {
	return nil, errors.New("Saved conversation browsing is currently available on Linux hosts")
}
