package logging

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Init builds a production JSON logger, installs it as the zap global, and returns it.
func Init() *zap.Logger {
	cfg := zap.NewProductionConfig()
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := cfg.Build()
	if err != nil {
		logger = zap.NewNop()
	}
	zap.ReplaceGlobals(logger)
	return logger
}
