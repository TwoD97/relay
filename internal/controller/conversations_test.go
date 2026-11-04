package controller

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConversationQueryRejectsAmbiguousAndUnsafeRequests(t *testing.T) {
	for _, query := range []string{"harness=claude", "harness=shell&cwd=/test", "harness=claude&cwd=/a&cwd=/b", "harness=claude&cwd=/test&extra=1", "harness=claude&cwd=%00", "harness=claude&cwd=/test%0aother"} {
		r := httptest.NewRequest("GET", "http://relay/api?"+query, nil)
		if _, err := conversationRequest(httptest.NewRecorder(), r); err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
	r := httptest.NewRequest("PATCH", "http://relay/api", strings.NewReader(`{"harness":"claude","cwd":"/test","title":"Name","operation":"launch"}`))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("conversation", "1a2b3c4d-1234-4567-89ab-0123456789ab")
	if _, err := conversationRequest(httptest.NewRecorder(), r); err == nil {
		t.Fatal("accepted unexpected mutation fields")
	}
}
