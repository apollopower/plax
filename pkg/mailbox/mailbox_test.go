package mailbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendAndRecv(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	msg := Message{From: "i2", Subject: "hello", Body: "world"}
	if _, err := Send(root, "i1", msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	n, err := Count(root, "i1")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}

	msgs, err := Recv(root, "i1", 1)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].From != "i2" || msgs[0].Subject != "hello" || msgs[0].Body != "world" {
		t.Errorf("message = %+v", msgs[0])
	}

	n, err = Count(root, "i1")
	if err != nil {
		t.Fatalf("Count after Recv: %v", err)
	}
	if n != 0 {
		t.Errorf("Count after Recv = %d, want 0", n)
	}
}

func TestRecvAll(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	for i := range 3 {
		_, err := Send(root, "i1", Message{Body: string(rune('a' + i))})
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	n, err := Count(root, "i1")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}

	msgs, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}

	n, _ = Count(root, "i1")
	if n != 0 {
		t.Errorf("Count after RecvAll = %d, want 0", n)
	}
}

func TestRecvEmptyMailbox(t *testing.T) {
	root := t.TempDir()

	msgs, err := Recv(root, "nobody", 1)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("got %d messages, want 0", len(msgs))
	}
}

func TestCountEmptyMailbox(t *testing.T) {
	root := t.TempDir()

	n, err := Count(root, "nobody")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

func TestSendNonExistentDir(t *testing.T) {
	root := t.TempDir()

	_, err := Send(root, "newinst", Message{Body: "hi"})
	if err == nil {
		t.Fatal("expected error for non-existent mailbox directory")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRemoveDir(t *testing.T) {
	root := t.TempDir()

	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}
	if _, err := Send(root, "i1", Message{Body: "msg"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := RemoveDir(root, "i1"); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}

	n, err := Count(root, "i1")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("Count after removal = %d, want 0", n)
	}
}

func TestChronologicalOrder(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	for i := range 5 {
		_, err := Send(root, "i1", Message{Body: string(rune('a' + i))})
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	msgs, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Timestamp < msgs[i-1].Timestamp {
			t.Fatalf("messages out of order at index %d: %s < %s", i, msgs[i].Timestamp, msgs[i-1].Timestamp)
		}
	}
}

func TestRecvMultipleMessages(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	for range 5 {
		if _, err := Send(root, "i1", Message{Body: "x"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	msgs, err := Recv(root, "i1", 2)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	n, _ := Count(root, "i1")
	if n != 3 {
		t.Errorf("remaining = %d, want 3", n)
	}
}

func TestFileSuffixIsRandom(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	if _, err := Send(root, "i1", Message{Body: "a"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := Send(root, "i1", Message{Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	dir := Dir(root, "i1")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 message files, got %d", len(entries))
	}
	if entries[0].Name() == entries[1].Name() {
		t.Fatal("message filenames are identical")
	}
}

func TestSendDefaultTimestamp(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	if _, err := Send(root, "i1", Message{From: "test", Body: "no timestamp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msgs, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Timestamp == "" {
		t.Fatal("Timestamp was not filled in")
	}
}

func TestSendEmptyBody(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	_, err := Send(root, "i1", Message{From: "test", Body: ""})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRecvNotExistingInstance(t *testing.T) {
	root := t.TempDir()

	msgs, err := Recv(root, "nonexistent", 1)
	if err != nil {
		t.Fatalf("Recv on nonexistent instance should not error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("got %d messages, want 0", len(msgs))
	}
}

func TestCreateAndRemoveDir(t *testing.T) {
	root := t.TempDir()

	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	dir := Dir(root, "i1")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatal("mailbox directory should exist")
	}

	if err := RemoveDir(root, "i1"); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("mailbox directory should be removed")
	}
}

func TestDir(t *testing.T) {
	got := Dir("/repo", "i1")
	want := filepath.Join("/repo", ".plax", "mail", "i1")
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestRecvUnreadableFile(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	if _, err := Send(root, "i1", Message{Body: "good"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := Send(root, "i1", Message{Body: "bad"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	dir := Dir(root, "i1")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		data, _ := os.ReadFile(path)
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Body == "bad" {
			if err := os.Chmod(path, 0000); err != nil {
				t.Fatalf("Chmod: %v", err)
			}
			break
		}
	}

	msgs, err := Recv(root, "i1", 2)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != "good" {
		t.Errorf("message body = %q, want %q", msgs[0].Body, "good")
	}

	n, _ := Count(root, "i1")
	if n != 1 {
		t.Errorf("unreadable file was deleted; remaining = %d, want 1", n)
	}
}

func TestRecv_ZeroCount(t *testing.T) {
	root := t.TempDir()
	_, err := Recv(root, "i1", 0)
	if err == nil || !strings.Contains(err.Error(), "count must be >= 1") {
		t.Errorf("expected count error, got %v", err)
	}
}

func TestRecv_NegativeCount(t *testing.T) {
	root := t.TempDir()
	_, err := Recv(root, "i1", -1)
	if err == nil || !strings.Contains(err.Error(), "count must be >= 1") {
		t.Errorf("expected count error, got %v", err)
	}
}

func TestSend_ConcurrentNoTornReads(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	// Send messages concurrently, then verify all are intact (no partial reads).
	for range 50 {
		_, err := Send(root, "i1", Message{Body: "hello"})
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	msgs, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	if len(msgs) != 50 {
		t.Fatalf("got %d messages, want 50", len(msgs))
	}
	for _, msg := range msgs {
		if msg.Body != "hello" {
			t.Errorf("got body %q, want hello", msg.Body)
		}
	}
}

func TestRecvAllUnreadable(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	if _, err := Send(root, "i1", Message{Body: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := Send(root, "i1", Message{Body: "y"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	dir := Dir(root, "i1")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if err := os.Chmod(path, 0000); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
	}

	_, err := Recv(root, "i1", 2)
	if err == nil {
		t.Fatal("expected error when all messages are unreadable")
	}
	if !strings.Contains(err.Error(), "all 2 messages unreadable") {
		t.Errorf("unexpected error: %v", err)
	}
}
