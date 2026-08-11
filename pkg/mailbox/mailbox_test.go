package mailbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestMailbox_SendAndRecv(t *testing.T) {
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

	result, err := Recv(root, "i1", 1)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(result.Messages))
	}
	if result.Messages[0].From != "i2" || result.Messages[0].Subject != "hello" || result.Messages[0].Body != "world" {
		t.Errorf("message = %+v", result.Messages[0])
	}

	n, err = Count(root, "i1")
	if err != nil {
		t.Fatalf("Count after Recv: %v", err)
	}
	if n != 0 {
		t.Errorf("Count after Recv = %d, want 0", n)
	}
}

func TestMailbox_RecvAll(t *testing.T) {
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

	result, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	if len(result.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(result.Messages))
	}

	n, _ = Count(root, "i1")
	if n != 0 {
		t.Errorf("Count after RecvAll = %d, want 0", n)
	}
}

func TestMailbox_RecvEmptyMailbox(t *testing.T) {
	root := t.TempDir()

	result, err := Recv(root, "nobody", 1)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(result.Messages))
	}
}

func TestMailbox_CountEmptyMailbox(t *testing.T) {
	root := t.TempDir()

	n, err := Count(root, "nobody")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

func TestMailbox_SendNonExistentDir(t *testing.T) {
	root := t.TempDir()

	_, err := Send(root, "newinst", Message{Body: "hi"})
	if err == nil {
		t.Fatal("expected error for non-existent mailbox directory")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMailbox_RemoveDir(t *testing.T) {
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

func TestMailbox_ChronologicalOrder(t *testing.T) {
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

	result, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	for i := 1; i < len(result.Messages); i++ {
		if result.Messages[i].Timestamp < result.Messages[i-1].Timestamp {
			t.Fatalf("messages out of order at index %d: %s < %s", i, result.Messages[i].Timestamp, result.Messages[i-1].Timestamp)
		}
	}
}

func TestMailbox_RecvMultipleMessages(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	for range 5 {
		if _, err := Send(root, "i1", Message{Body: "x"}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	result, err := Recv(root, "i1", 2)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(result.Messages))
	}

	n, _ := Count(root, "i1")
	if n != 3 {
		t.Errorf("remaining = %d, want 3", n)
	}
}

func TestMailbox_FileSuffixIsRandom(t *testing.T) {
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

func TestMailbox_SendDefaultTimestamp(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	if _, err := Send(root, "i1", Message{From: "test", Body: "no timestamp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	result, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].Timestamp == "" {
		t.Fatal("Timestamp was not filled in")
	}
}

func TestMailbox_SendEmptyBody(t *testing.T) {
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

func TestMailbox_RecvNotExistingInstance(t *testing.T) {
	root := t.TempDir()

	result, err := Recv(root, "nonexistent", 1)
	if err != nil {
		t.Fatalf("Recv on nonexistent instance should not error: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("got %d messages, want 0", len(result.Messages))
	}
}

func TestMailbox_CreateAndRemoveDir(t *testing.T) {
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

func TestMailbox_Dir(t *testing.T) {
	got := Dir("/repo", "i1")
	want := filepath.Join("/repo", ".plax", "mail", "i1")
	if got != want {
		t.Errorf("Dir = %q, want %q", got, want)
	}
}

func TestMailbox_RecvUnreadableFile(t *testing.T) {
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

	result, err := Recv(root, "i1", 2)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(result.Messages))
	}
	if result.Messages[0].Body != "good" {
		t.Errorf("message body = %q, want %q", result.Messages[0].Body, "good")
	}

	n, _ := Count(root, "i1")
	if n != 1 {
		t.Errorf("unreadable file was deleted; remaining = %d, want 1", n)
	}
}

func TestMailbox_RecvZeroCount(t *testing.T) {
	root := t.TempDir()
	_, err := Recv(root, "i1", 0)
	if err == nil || !strings.Contains(err.Error(), "count must be >= 1") {
		t.Errorf("expected count error, got %v", err)
	}
}

func TestMailbox_RecvNegativeCount(t *testing.T) {
	root := t.TempDir()
	_, err := Recv(root, "i1", -1)
	if err == nil || !strings.Contains(err.Error(), "count must be >= 1") {
		t.Errorf("expected count error, got %v", err)
	}
}

func TestMailbox_SendConcurrentNoTornReads(t *testing.T) {
	root := t.TempDir()
	if err := CreateDir(root, "i1"); err != nil {
		t.Fatalf("CreateDir: %v", err)
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Send(root, "i1", Message{Body: "hello"})
			if err != nil {
				t.Errorf("Send: %v", err)
			}
		}()
	}
	wg.Wait()

	result, err := RecvAll(root, "i1")
	if err != nil {
		t.Fatalf("RecvAll: %v", err)
	}
	if len(result.Messages) != 50 {
		t.Fatalf("got %d messages, want 50", len(result.Messages))
	}
	for _, msg := range result.Messages {
		if msg.Body != "hello" {
			t.Errorf("got body %q, want hello", msg.Body)
		}
	}
}

func TestMailbox_RecvAllUnreadable(t *testing.T) {
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
