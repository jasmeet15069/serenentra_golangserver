package logger

import (
	"fmt"

	"go.uber.org/zap"

	"github.com/hotelharmony/api/internal/config"
)

func New(cfg *config.Config) (*zap.Logger, error) {
	var log *zap.Logger
	var err error
	if cfg.IsProd() {
		log, err = zap.NewProduction()
	} else {
		log, err = zap.NewDevelopment()
	}
	if err != nil {
		return nil, fmt.Errorf("logger: %w", err)
	}
	return log, nil
}
