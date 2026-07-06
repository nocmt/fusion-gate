package logger

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

type Logger struct {
	level    Level
	file     *os.File
	fileMu   sync.Mutex
	filePath string
}

// New creates a logger. If logToFile is true, writes to logs/YYYY-MM-DD-HHMMSS.log.
func New(level string) *Logger {
	l := &Logger{level: parseLevel(level)}
	return l
}

// NewWithFile creates a logger that also writes to a timestamped log file.
func NewWithFile(level string) *Logger {
	l := New(level)
	if err := os.MkdirAll("logs", 0755); err == nil {
		path := fmt.Sprintf("logs/%s.log", time.Now().Format("2006-01-02-150405"))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			l.file = f
			l.filePath = path
		}
	}
	return l
}

func (l *Logger) FilePath() string { return l.filePath }

func parseLevel(s string) Level {
	switch strings.ToLower(s) {
	case "debug": return LevelDebug
	case "warn", "warning": return LevelWarn
	case "error": return LevelError
	default: return LevelInfo
	}
}

func (l *Logger) should(lv Level) bool { return lv >= l.level }
func (l *Logger) always() bool         { return true } // for Raw which ignores level

func (l *Logger) logToStderr(lv Level, color, tag, format string, args ...any) {
	if !l.should(lv) { return }
	ts := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s%s%s  %s%s%s  %s\n", colorGray, ts, colorReset, color, tag, colorReset, msg)
}

func (l *Logger) logToFile(tag, format string, args ...any) {
	if l.file == nil { return }
	l.fileMu.Lock()
	defer l.fileMu.Unlock()
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	// truncate very long messages
	if len(msg) > 8000 { msg = msg[:8000] + "\n...[truncated]" }
	fmt.Fprintf(l.file, "%s [%s] %s\n", ts, tag, msg)
	l.file.Sync()
}

// --- public API ---

func (l *Logger) Debug(format string, args ...any) {
	l.logToStderr(LevelDebug, colorGray, "DEBUG", format, args...)
	l.logToFile("DEBUG", format, args...)
}
func (l *Logger) Info(format string, args ...any) {
	l.logToStderr(LevelInfo, colorCyan, "INFO ", format, args...)
	l.logToFile("INFO", format, args...)
}
func (l *Logger) Warn(format string, args ...any) {
	l.logToStderr(LevelWarn, colorYellow, "WARN ", format, args...)
	l.logToFile("WARN", format, args...)
}
func (l *Logger) Error(format string, args ...any) {
	l.logToStderr(LevelError, colorRed, "ERROR", format, args...)
	l.logToFile("ERROR", format, args...)
}

// Raw always writes to file regardless of log level (used for full request/response dumps).
func (l *Logger) Raw(tag, format string, args ...any) {
	l.logToFile(tag, format, args...)
}

// Close flushes and closes the log file.
func (l *Logger) Close() {
	if l.file != nil { l.file.Close() }
}
