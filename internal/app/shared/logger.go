package shared

import (
	"io"
	"os"
	"time"

	"github.com/DODOEX/web3rpcproxy/utils/config"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp/prefork"
)

// initialize logger
func NewLogger(config *config.Conf) zerolog.Logger {
	// Create log file
	file, err := os.OpenFile("/app/logs/debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		// If failed to open file, use stdout only
		log.Warn().Err(err).Msg("Failed to open log file, using stdout only")
		if config.Bool("logger.prettier", true) {
			log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
		} else {
			log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
		}
	} else {
		// Configure multi-output (both console and file)
		writers := []io.Writer{os.Stdout, file}
		mw := io.MultiWriter(writers...)

		if config.Bool("logger.prettier", true) {
			log.Logger = log.Output(zerolog.ConsoleWriter{Out: mw})
		} else {
			log.Logger = zerolog.New(mw).With().Timestamp().Logger()
		}
	}

	zerolog.TimeFieldFormat = config.String("logger.time-format", time.RFC3339)
	l, err := zerolog.ParseLevel(config.String("logger.level", "debug"))
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to parse log level")
	}
	zerolog.SetGlobalLevel(l)

	return log.Hook(PreforkHook{})
}

// prefer hook for zerologger
type PreforkHook struct{}

func (h PreforkHook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	if prefork.IsChild() {
		e.Discard()
	}
}
