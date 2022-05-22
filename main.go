package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/evsamsonov/fast-slow-ema-robot/internal/fastslowema"
	"github.com/evsamsonov/trengin"
	"github.com/evsamsonov/trengin/broker/tinkoff"
	"github.com/go-playground/validator/v10"
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
	AppName   string
}

type StrategyConfig struct {
	FastEmaSmoothInterval int `validate:"required"`
	SlowEmaSmoothInterval int `validate:"required"`
	FlatMaxDiff           float64
}

func main() {
	config, err := ParseConfig()
	if err != nil {
		log.Fatal(err)
	}

	logger, err := zap.NewDevelopment(
		zap.IncreaseLevel(config.LogLevel),
		zap.AddStacktrace(zap.ErrorLevel),
	)
	if err != nil {
		log.Fatal(err)
	}

	fastSlowEmaStrategy, err := fastslowema.NewStrategy(
		config.Tinkoff.Token,
		config.InstrumentFIGI,
		config.CandleInterval,
		fastslowema.WithLogger(logger),
		fastslowema.WithAppName(config.Tinkoff.AppName),
		fastslowema.WithStopLoss(config.StopLossIndent),
		fastslowema.WithSmoothInterval(config.Strategy.FastEmaSmoothInterval, config.Strategy.SlowEmaSmoothInterval),
		fastslowema.WithFlatMaxDiff(config.Strategy.FlatMaxDiff),
	)
	if err != nil {
		logger.Fatal("Failed to parse config", zap.Error(err))
	}

	tinkoffBroker, err := tinkoff.New(
		config.Tinkoff.Token,
		config.Tinkoff.AccountID,
		config.InstrumentFIGI,
		config.TradedQuantity,
		tinkoff.WithLogger(logger),
		tinkoff.WithAppName(config.Tinkoff.AppName),
	)

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tradingEngine := trengin.New(fastSlowEmaStrategy, tinkoffBroker)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err = tradingEngine.Run(ctx); err != nil {
			if err == context.Canceled {
				return
			}
			logger.Fatal("Failed to run trading engine", zap.Error(err))
		}
	}()

	logger.Info("Robot was started", zap.Any("config", SafeConfig(config)))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	wg.Wait()
	logger.Info("Robot was stopped")
}

func ParseConfig() (Config, error) {
	viper.SetConfigFile("config.yaml")
	viper.AddConfigPath(".")
	viper.SetConfigType("yaml")

	err := viper.ReadInConfig()
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	candleInterval, err := ParseCandleInterval(viper.GetString("candleInterval"))
	if err != nil {
		return Config{}, fmt.Errorf("parse candle interval: %w", err)
	}

	config := Config{
		LogLevel:       ParseLogLevel(viper.GetString("logLevel")),
		InstrumentFIGI: viper.GetString("instrumentFigi"),
		CandleInterval: candleInterval,
		StopLossIndent: viper.GetFloat64("stopLossIndent"),
		TradedQuantity: viper.GetInt64("tradedQuantity"),
		Tinkoff: TinkoffConfig{
			Token:     viper.GetString("tinkoff.token"),
			AccountID: viper.GetString("tinkoff.AccountId"),
			AppName:   viper.GetString("tinkoff.appName"),
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

func ParseCandleInterval(interval string) (investapi.CandleInterval, error) {
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

func ParseLogLevel(l string) zapcore.LevelEnabler {
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

func SafeConfig(config Config) Config {
	config.Tinkoff.Token = "*"
	return config
}
