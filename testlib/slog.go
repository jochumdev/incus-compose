package testlib

import (
	"log/slog"
	"os"

	"github.com/lxc/incus-compose/shared"
)

func InitSlog() {
	logger := slog.New(slog.NewTextHandler(
		os.Stderr,
		&slog.HandlerOptions{Level: shared.LevelTrace}),
	)

	slog.SetDefault(logger)
}
