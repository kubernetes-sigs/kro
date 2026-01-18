package view

import (
	"io"
	"log/slog"
	"time"

	"github.com/fatih/color"
	"github.com/lmittmann/tint"
)

type LogLevel int

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
	LogLevelSilent
)

// convertToSlogLevel converts our LogLevel to slog.Level
func (l LogLevel) toSlogLevel() slog.Level {
	switch l {
	case LogLevelDebug:
		return slog.LevelDebug
	case LogLevelInfo:
		return slog.LevelInfo
	case LogLevelWarn:
		return slog.LevelWarn
	case LogLevelError:
		return slog.LevelError
	case LogLevelSilent:
		return slog.Level(100)
	default:
		return slog.Level(100)
	}
}

type humanLogger struct {
	logger *slog.Logger
}

type jsonLogger struct {
	logger *slog.Logger
}

// Compile-time assertions to ensure our types implement the Logger interface
var _ Logger = (*humanLogger)(nil)
var _ Logger = (*jsonLogger)(nil)

func rewriteLogLevel(groups []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey && len(groups) == 0 {
		level := a.Value.Any().(slog.Level)

		var levelText string
		switch level {
		case slog.LevelDebug:
			levelText = "DEBUG"
		case slog.LevelInfo:
			levelText = color.GreenString("INFO")
		case slog.LevelWarn:
			levelText = color.YellowString("WARN")
		case slog.LevelError:
			levelText = color.RedString("ERROR")
		default:
			levelText = level.String()
		}
		a.Value = slog.StringValue(levelText)
	}

	return a
}

func (l *humanLogger) Debug(msg string, args ...any) {
	l.logger.Debug(msg, args...)
}

func (l *humanLogger) Info(msg string, args ...any) {
	l.logger.Info(msg, args...)
}

func (l *humanLogger) Warn(msg string, args ...any) {
	l.logger.Warn(msg, args...)
}

func (l *humanLogger) Error(msg string, args ...any) {
	l.logger.Error(msg, args...)
}

func (l *jsonLogger) Debug(msg string, args ...any) {
	l.logger.Debug(msg, args...)
}

func (l *jsonLogger) Info(msg string, args ...any) {
	l.logger.Info(msg, args...)
}

func (l *jsonLogger) Warn(msg string, args ...any) {
	l.logger.Warn(msg, args...)
}

func (l *jsonLogger) Error(msg string, args ...any) {
	l.logger.Error(msg, args...)
}

// NewHumanLogger creates a human-readable slog logger
func NewHumanLogger(w io.Writer, level LogLevel) Logger {
	opts := &tint.Options{
		Level:       level.toSlogLevel(),
		TimeFormat:  time.DateTime,
		ReplaceAttr: rewriteLogLevel,
	}
	handler := tint.NewHandler(w, opts)
	logger := slog.New(handler)
	return &humanLogger{logger: logger}
}

// NewJSONLogger creates a JSON-structured slog logger
func NewJSONLogger(w io.Writer, level LogLevel) Logger {
	opts := &slog.HandlerOptions{
		Level: level.toSlogLevel(),
	}
	handler := slog.NewJSONHandler(w, opts)
	logger := slog.New(handler)
	return &jsonLogger{logger: logger}
}

// NewNopLogger creates a no-op logger that discards all output
func NewNopLogger() Logger {
	opts := &slog.HandlerOptions{
		Level: slog.Level(100), // Higher than any real level
	}
	handler := slog.NewJSONHandler(io.Discard, opts)
	logger := slog.New(handler)
	return &jsonLogger{logger: logger}
}
