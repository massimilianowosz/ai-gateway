package hivetrace

import "testing"

func TestClientSessionIDFromClaudeCode(t *testing.T) {
	// Claude Code nests a JSON document inside metadata.user_id.
	body := []byte(`{"model":"claude-sonnet-5","metadata":{"user_id":
		"{\"device_id\":\"6e2661a82eed9ca0\",\"account_uuid\":\"\",\"session_id\":\"b8a6fc2d-61ea-4f79-b489-2542bfe70fbb\"}"}}`)
	if got := clientSessionID(body); got != "b8a6fc2d-61ea-4f79-b489-2542bfe70fbb" {
		t.Fatalf("got %q", got)
	}
}

func TestClientSessionIDIgnoresDeviceID(t *testing.T) {
	// The device id follows a user across every conversation they ever have,
	// so it must never become the session.
	body := []byte(`{"metadata":{"user_id":"{\"device_id\":\"stable-device\",\"session_id\":\"\"}"}}`)
	if got := clientSessionID(body); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestClientSessionIDPlainString(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"tenant-42-conversation-7"}}`)
	if got := clientSessionID(body); got != "tenant-42-conversation-7" {
		t.Fatalf("got %q", got)
	}
}

func TestClientSessionIDAbsent(t *testing.T) {
	for _, body := range [][]byte{
		nil,
		[]byte(`{}`),
		[]byte(`{"metadata":{}}`),
		[]byte(`{"metadata":{"user_id":"  "}}`),
		[]byte(`not json`),
		[]byte(`{"metadata":{"user_id":"{broken json"}}`),
	} {
		if got := clientSessionID(body); got != "" {
			t.Errorf("clientSessionID(%s) = %q, want empty", body, got)
		}
	}
}
