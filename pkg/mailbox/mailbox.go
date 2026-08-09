package mailbox

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Message struct {
	From      string `json:"from,omitempty"`
	Subject   string `json:"subject,omitempty"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp,omitempty"`
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

func Send(root, name string, msg Message) error {
	dir := Dir(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mailbox: mkdir: %w", err)
	}

	if msg.Timestamp == "" {
		msg.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("mailbox: rand: %w", err)
	}
	filename := fmt.Sprintf("%d_%x.json", time.Now().UnixNano(), suffix)
	path := filepath.Join(dir, filename)

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mailbox: marshal: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("mailbox: write: %w", err)
	}

	return nil
}

func Recv(root, name string, count int) ([]Message, error) {
	return recvFile(root, name, count, false)
}

func RecvAll(root, name string) ([]Message, error) {
	return recvFile(root, name, 0, true)
}

func recvFile(root, name string, count int, all bool) ([]Message, error) {
	files, err := listFiles(root, name)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}

	if !all && count > 0 && count < len(files) {
		files = files[:count]
	}

	messages := make([]Message, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		messages = append(messages, msg)
	}

	for _, f := range files {
		_ = os.Remove(f)
	}

	return messages, nil
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

type CountWriter struct {
	io.Writer
	N int
}

func (cw *CountWriter) Write(p []byte) (int, error) {
	n, err := cw.Writer.Write(p)
	cw.N += n
	return n, err
}
