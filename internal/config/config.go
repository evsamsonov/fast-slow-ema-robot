package config

import (
	"errors"
	"fmt"

	"github.com/go-playground/validator"
	"github.com/spf13/viper"
	investapi "github.com/tinkoff/invest-api-go-sdk"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Config struct {
	LogLevel       zapcore.LevelEnabler
	InstrumentFIGI string                   `validate:"required"`
	CandleInterval investapi.CandleInterval `validate:"required"`
	StopLossIndent float64
	TradedQuantity int64 `validate:"gte=1"`
	Tinkoff        TinkoffConfig
	Strategy       StrategyConfig
}

type TinkoffConfig struct {
	Token     string `validate:"required"`
	AccountID string `validate:"required"`
}

type StrategyConfig struct {
	FastEmaSmoothInterval int `validate:"required"`
	SlowEmaSmoothInterval int `validate:"required"`
	FlatMaxDiff           float64
}

func Parse() (Config, error) {
	viper.SetConfigFile("config.yaml")
	viper.AddConfigPath(".")
	viper.SetConfigType("yaml")

	err := viper.ReadInConfig()
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	candleInterval, err := parseCandleInterval(viper.GetString("candleInterval"))
	if err != nil {
		return Config{}, fmt.Errorf("parse candle interval: %w", err)
	}

	config := Config{
		LogLevel:       parseLogLevel(viper.GetString("logLevel")),
		InstrumentFIGI: viper.GetString("instrumentFigi"),
		CandleInterval: candleInterval,
		StopLossIndent: viper.GetFloat64("stopLossIndent"),
		TradedQuantity: viper.GetInt64("tradedQuantity"),
		Tinkoff: TinkoffConfig{
			Token:     viper.GetString("tinkoff.token"),
			AccountID: viper.GetString("tinkoff.AccountId"),
		},
		Strategy: StrategyConfig{
			FastEmaSmoothInterval: viper.GetInt("strategy.fastEmaSmoothInterval"),
			SlowEmaSmoothInterval: viper.GetInt("strategy.slowEmaSmoothInterval"),
			FlatMaxDiff:           viper.GetFloat64("strategy.flatMaxDiff"),
		},
	}

	if err := validator.New().Struct(config); err != nil {
		return Config{}, fmt.Errorf("validate struct: %w", err)
	}
	if config.Strategy.FastEmaSmoothInterval >= config.Strategy.SlowEmaSmoothInterval {
		return Config{}, errors.New("fast smooth interval must be less than slow")
	}
	return config, nil
}

func (c Config) Safe() Config {
	c.Tinkoff.Token = "*"
	return c
}

func parseCandleInterval(interval string) (investapi.CandleInterval, error) {
	switch interval {
	case "1min":
		return investapi.CandleInterval_CANDLE_INTERVAL_1_MIN, nil
	case "5min":
		return investapi.CandleInterval_CANDLE_INTERVAL_5_MIN, nil
	case "15min":
		return investapi.CandleInterval_CANDLE_INTERVAL_15_MIN, nil
	case "1hour":
		return investapi.CandleInterval_CANDLE_INTERVAL_HOUR, nil
	case "1day":
		return investapi.CandleInterval_CANDLE_INTERVAL_DAY, nil
	default:
		return 0, errors.New("unexpected interval")
	}
}

func parseLogLevel(l string) zapcore.LevelEnabler {
	switch l {
	case "debug":
		return zap.DebugLevel
	case "info":
		return zap.InfoLevel
	case "error":
		return zap.ErrorLevel
	default:
		return zap.DebugLevel
	}
}
