package agent

import (
	"os"
	"sync"
)

// maxLogSize is when agent.log is moved to agent.log.1 and started again, so
// a PC that runs for years keeps at most two files of this size.
const maxLogSize = 5 << 20

// LogFile is an append-only log file that rotates itself once.
type LogFile struct {
	path string
	mu   sync.Mutex
	file *os.File
	size int64
}

// OpenLogFile opens (or creates) the log for appending.
func OpenLogFile(path string) (*LogFile, error) {
	l := &LogFile{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *LogFile) open() error {
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	l.file, l.size = file, info.Size()
	return nil
}

func (l *LogFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > maxLogSize {
		l.file.Close()
		// If the rename fails the old file is appended to again: a log
		// that grows is better than an agent that stops.
		_ = os.Rename(l.path, l.path+".1")
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

// Close closes the file.
func (l *LogFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
