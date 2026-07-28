package gateway

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

type AccessLogEntry struct {
	Timestamp  time.Time
	RemoteIP   string
	UserID     string
	Operation  string
	Bucket     string
	Key        string
	StatusCode int
	BytesSent  int64
	RequestID  string
	Latency    time.Duration
}

type AccessLogger struct {
	mu            sync.Mutex
	dir           string
	file          *os.File
	writer        *bufio.Writer
	currentSize   int64
	maxSize       int64
	flushInterval time.Duration
	done          chan struct{}
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
}

func NewAccessLogger(dir string, maxSize int64) (*AccessLogger, error) {
	if dir == "" {
		dir = "./data/logs"
	}
	if maxSize <= 0 {
		maxSize = 100 * 1024 * 1024
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create access log directory: %w", err)
	}

	l := &AccessLogger{
		dir:           dir,
		maxSize:       maxSize,
		flushInterval: 5 * time.Second,
		done:          make(chan struct{}),
	}

	if err := l.rotate(); err != nil {
		return nil, fmt.Errorf("failed to initialize access log file: %w", err)
	}

	l.wg.Add(1)
	go l.flushLoop()

	return l, nil
}

func (l *AccessLogger) Log(entry AccessLogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.writer == nil {
		return
	}

	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	line := fmt.Sprintf("%s %s %s %s %s %s %d %d %s %s\n",
		entry.Timestamp.UTC().Format(time.RFC3339Nano),
		entry.RemoteIP,
		entry.UserID,
		entry.Operation,
		entry.Bucket,
		entry.Key,
		entry.StatusCode,
		entry.BytesSent,
		entry.RequestID,
		entry.Latency,
	)

	n, err := l.writer.WriteString(line)
	if err != nil {
		zap.L().Warn("access log: failed to write entry", zap.Error(err))
		return
	}
	l.currentSize += int64(n)

	if l.currentSize >= l.maxSize {
		if rotateErr := l.rotate(); rotateErr != nil {
			zap.L().Warn("access log: failed to rotate after write", zap.Error(rotateErr))
		}
	}
}

func (l *AccessLogger) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.wg.Wait()

		l.mu.Lock()
		defer l.mu.Unlock()

		l.closeErr = l.flushAndClose()
	})
	return l.closeErr
}

func (l *AccessLogger) flushLoop() {
	defer l.wg.Done()

	ticker := time.NewTicker(l.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			l.mu.Lock()
			if l.writer != nil {
				if err := l.writer.Flush(); err != nil {
					zap.L().Warn("access log: flush failed", zap.Error(err))
				}
			}
			l.mu.Unlock()
		case <-l.done:
			return
		}
	}
}

func (l *AccessLogger) rotate() error {
	if l.writer != nil {
		l.writer.Flush()
	}
	if l.file != nil {
		l.file.Close()
	}

	filename := fmt.Sprintf("access-%s.log", time.Now().Format("2006-01-02-150405"))
	path := filepath.Join(l.dir, filename)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open access log file: %w", err)
	}

	l.file = f
	l.writer = bufio.NewWriterSize(f, 64*1024)
	l.currentSize = 0

	if fi, err := f.Stat(); err == nil {
		l.currentSize = fi.Size()
	}

	return nil
}

func (l *AccessLogger) flushAndClose() error {
	var err error
	if l.writer != nil {
		if flushErr := l.writer.Flush(); flushErr != nil {
			err = flushErr
		}
		l.writer = nil
	}
	if l.file != nil {
		if closeErr := l.file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		l.file = nil
	}
	return err
}
