package utils

import (
	"log/slog"
	"os"
)

// SetupLogger installs a text slog handler at the given level as the default logger.
func SetupLogger(level slog.Level) *slog.Logger {
	l := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(l)
	return l
}
