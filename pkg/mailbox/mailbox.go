// Package mailbox provides file-based inter-instance message passing
// under .plax/mail/<name>/.
package mailbox

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Message is a single inter-instance notification.
type Message struct {
	From      string `json:"from"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
}

// RecvResult holds the result of a Recv or RecvAll call.
type RecvResult struct {
	Messages []Message
	Skipped  []string // filenames that could not be parsed
}

func Dir(root, name string) string {
	return filepath.Join(root, ".plax", "mail", name)
}

func CreateDir(root, name string) error {
	return os.MkdirAll(Dir(root, name), 0755)
}

func RemoveDir(root, name string) error {
	return os.RemoveAll(Dir(root, name))
}

// Send writes a message to the named instance's mailbox.
// The write is atomic: the message appears fully formed or not at all.
// Returns an error if the mailbox directory does not exist.
func Send(root, name string, msg Message) (string, error) {
	dir := Dir(root, name)
	ok, err := exists(dir)
	if err != nil {
		return "", fmt.Errorf("mailbox: stat dir: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("mailbox: directory %s does not exist", dir)
	}

	if msg.Body == "" {
		return "", fmt.Errorf("mailbox: message must have a body")
	}

	if msg.Timestamp == "" {
		msg.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("mailbox: rand: %w", err)
	}
	now := time.Now().UnixNano()
	tmpName := fmt.Sprintf(".tmp_%d_%x.json", now, nonce)
	tmpPath := filepath.Join(dir, tmpName)
	finalName := fmt.Sprintf("%d_%x.json", now, nonce)
	finalPath := filepath.Join(dir, finalName)

	data, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("mailbox: marshal: %w", err)
	}

	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return "", fmt.Errorf("mailbox: write: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("mailbox: rename: %w", err)
	}

	return finalName, nil
}

// Recv reads up to count messages from the named instance's mailbox,
// oldest first, and removes them. Delivery is at-least-once: a message
// whose removal fails will be redelivered on the next call.
func Recv(root, name string, count int) (*RecvResult, error) {
	if count < 1 {
		return nil, fmt.Errorf("mailbox: recv count must be >= 1, got %d", count)
	}
	return recvFile(root, name, count, false)
}

// RecvAll reads all messages from the named instance's mailbox, oldest
// first, and removes them. Delivery is at-least-once.
func RecvAll(root, name string) (*RecvResult, error) {
	return recvFile(root, name, 0, true)
}

func recvFile(root, name string, count int, all bool) (*RecvResult, error) {
	files, err := listFiles(root, name)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return &RecvResult{}, nil
	}

	if !all && count > 0 && count < len(files) {
		files = files[:count]
	}

	var messages []Message
	var skipped []string
	var removed []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			skipped = append(skipped, filepath.Base(f))
			continue
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			skipped = append(skipped, filepath.Base(f))
			continue
		}
		messages = append(messages, msg)
		removed = append(removed, f)
	}

	for _, f := range removed {
		if err := os.Remove(f); err != nil {
			skipped = append(skipped, filepath.Base(f))
		}
	}

	if len(messages) == 0 && len(files) > 0 && len(skipped) == len(files) {
		return &RecvResult{Skipped: skipped}, fmt.Errorf("recv: all %d messages unreadable", len(files))
	}

	return &RecvResult{Messages: messages, Skipped: skipped}, nil
}

func Count(root, name string) (int, error) {
	files, err := listFiles(root, name)
	if err != nil {
		return 0, err
	}
	return len(files), nil
}

func listFiles(root, name string) ([]string, error) {
	dir := Dir(root, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mailbox: read dir: %w", err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}

	sort.Strings(files)
	return files, nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
