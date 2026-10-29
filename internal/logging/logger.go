// Package logging provides a small JSON-lines logger using the Go 1.18 standard library.
package logging

import (
	"encoding/json"
	"io"
	"log"
	"sync"
	"time"
)

// Logger writes structured records without including request credentials.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
}

// New returns a JSON-lines logger writing to out.
func New(out io.Writer) *Logger { return &Logger{out: out} }

// Info writes an informational event and its structured fields.
func (l *Logger) Info(message string, fields map[string]interface{}) {
	l.write("info", message, fields)
}

// Error writes an error event and its structured fields.
func (l *Logger) Error(message string, fields map[string]interface{}) {
	l.write("error", message, fields)
}

func (l *Logger) write(level, message string, fields map[string]interface{}) {
	record := make(map[string]interface{}, len(fields)+3)
	record["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	record["level"] = level
	record["message"] = message
	for key, value := range fields {
		record[key] = value
	}
	data, err := json.Marshal(record)
	if err != nil {
		log.Printf("structured logging failed: %v", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(data, '\n'))
}
