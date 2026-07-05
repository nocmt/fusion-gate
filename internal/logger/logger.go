package logger

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Level is a log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ANSI color codes (dark-theme friendly).
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
)

// Logger is a minimal leveled logger writing to stderr.
type Logger struct {
	level Level
}

// New creates a logger from a level string (debug/info/warn/error).
func New(level string) *Logger {
	switch strings.ToLower(level) {
	case "debug":
		return &Logger{level: LevelDebug}
	case "warn", "warning":
		return &Logger{level: LevelWarn}
	case "error":
		return &Logger{level: LevelError}
	default:
		return &Logger{level: LevelInfo}
	}
}

func (l *Logger) should(lv Level) bool { return lv >= l.level }

func (l *Logger) log(lv Level, color, tag, format string, args ...any) {
	if !l.should(lv) {
		return
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s%s%s  %s%s%s  %s\n", colorGray, ts, colorReset, color, tag, colorReset, msg)
}

func (l *Logger) Debug(format string, args ...any) { l.log(LevelDebug, colorGray, "DEBUG", format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.log(LevelInfo, colorCyan, "INFO ", format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.log(LevelWarn, colorYellow, "WARN ", format, args...) }
func (l *Logger) Error(format string, args ...any) { l.log(LevelError, colorRed, "ERROR", format, args...) }
