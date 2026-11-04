//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/TwoD97/relay/internal/conversations"
)

// Codex owns its metadata format and rename operation. This client invokes only
// initialize, thread/list, thread/read and thread/name/set, never thread/start,
// thread/resume or turn/start. No account or transcript data leaves this helper.
type conversationRPC struct {
	mu        sync.Mutex
	partial   []byte
	bytes     int
	responses chan rpcResponse
	cancel    context.CancelFunc
}
type rpcResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (r *conversationRPC) Write(data []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(data)
	r.bytes += n
	if r.bytes > 2<<20 {
		r.cancel()
		return n, errors.New("conversation metadata exceeded its limit")
	}
	r.partial = append(r.partial, data...)
	for {
		i := bytes.IndexByte(r.partial, '\n')
		if i < 0 {
			break
		}
		line := r.partial[:i]
		r.partial = r.partial[i+1:]
		var response rpcResponse
		if json.Unmarshal(line, &response) == nil && response.ID > 0 {
			select {
			case r.responses <- response:
			default:
				r.cancel()
				return n, errors.New("too many conversation responses")
			}
		}
	}
	return n, nil
}
func (s *Server) codexRPC(ctx context.Context, path, cwd, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	stream := &conversationRPC{responses: make(chan rpcResponse, 8), cancel: cancel}
	cmd := exec.CommandContext(ctx, path, "app-server")
	cmd.Dir, cmd.Env = cwd, s.environment()
	cmd.Stdin = reader
	cmd.Stdout = stream
	cmd.Stderr = &boundedOutput{limit: 8 << 10}
	done := make(chan error, 1)
	go func() { done <- runIsolatedCommand(ctx, cmd) }()
	defer func() { cancel(); reader.Close(); writer.Close(); <-done }()
	send := func(value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		_, err = writer.Write(data)
		return err
	}
	// Closing the stdin pipe also releases a writer if the child never reads it.
	stop := context.AfterFunc(ctx, func() { reader.Close(); writer.Close() })
	defer stop()
	await := func(id int, dst any) error {
		for {
			select {
			case response := <-stream.responses:
				if response.ID != id {
					continue
				}
				if len(response.Error) > 0 && string(response.Error) != "null" {
					return errors.New("Codex does not support this conversation operation; update the harness and try again")
				}
				if dst != nil && json.Unmarshal(response.Result, dst) != nil {
					return errors.New("Codex returned invalid conversation metadata")
				}
				return nil
			case <-ctx.Done():
				return errors.New("Codex conversation metadata request timed out or was cancelled")
			}
		}
	}
	if err := send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "relay", "title": "Relay", "version": "1"}}}); err != nil {
		return errors.New("Could not initialize Codex conversation metadata")
	}
	if err := await(1, nil); err != nil {
		return err
	}
	if err := send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return err
	}
	if err := send(map[string]any{"id": 2, "method": method, "params": params}); err != nil {
		return errors.New("Could not send Codex conversation request; its result is unknown")
	}
	return await(2, result)
}

type codexThread struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	UpdatedAt int64  `json:"updatedAt"`
}

func codexEntry(t codexThread, cwd string) (conversations.Entry, bool) {
	if !conversations.ValidID(t.ID) || t.Cwd != cwd || t.UpdatedAt <= 0 {
		return conversations.Entry{}, false
	}
	e := conversations.Entry{ID: t.ID, Harness: "codex", Cwd: cwd, UpdatedAt: time.Unix(t.UpdatedAt, 0).UTC(), Title: "Codex · " + t.ID[:8], TitleSource: "fallback"}
	if conversations.ValidTitle(t.Name) {
		e.Title, e.TitleSource = t.Name, "provider"
	}
	return e, true
}
func (s *Server) codexConversations(ctx context.Context, path string, q conversations.Request) (any, error) {
	if q.Operation == "list" {
		var response struct {
			Data       []codexThread `json:"data"`
			NextCursor *string       `json:"nextCursor"`
		}
		err := s.codexRPC(ctx, path, q.Cwd, "thread/list", map[string]any{"cwd": q.Cwd, "limit": conversations.Limit, "sortKey": "updated_at", "archived": false, "useStateDbOnly": true, "sourceKinds": []string{"cli", "vscode"}}, &response)
		if err != nil {
			return nil, err
		}
		listing := conversations.Listing{Cwd: q.Cwd, Harness: "codex", RenameMode: "provider", Conversations: []conversations.Entry{}, Truncated: response.NextCursor != nil && *response.NextCursor != ""}
		if len(response.Data) > conversations.Limit {
			return nil, errors.New("Codex returned too many conversation records")
		}
		for _, thread := range response.Data {
			if e, ok := codexEntry(thread, q.Cwd); ok {
				listing.Conversations = append(listing.Conversations, e)
			}
		}
		return listing, nil
	}
	var response struct {
		Thread codexThread `json:"thread"`
	}
	if err := s.codexRPC(ctx, path, q.Cwd, "thread/read", map[string]any{"threadId": q.ID, "includeTurns": false}, &response); err != nil {
		return nil, err
	}
	entry, ok := codexEntry(response.Thread, q.Cwd)
	if !ok || entry.ID != q.ID {
		return nil, errors.New("Saved conversation was not found in this project")
	}
	if q.Operation == "rename" {
		if err := s.codexRPC(ctx, path, q.Cwd, "thread/name/set", map[string]string{"threadId": q.ID, "name": q.Title}, nil); err != nil {
			return nil, err
		}
		entry.Title, entry.TitleSource = q.Title, "provider"
	}
	return entry, nil
}
