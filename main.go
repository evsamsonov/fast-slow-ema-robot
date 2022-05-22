package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/evsamsonov/fast-slow-ema-robot/internal/config"
	"github.com/evsamsonov/fast-slow-ema-robot/internal/fastslowema"
	"github.com/evsamsonov/trengin"
	"github.com/evsamsonov/trengin/broker/tinkoff"
	"go.uber.org/zap"
)

func main() {
	cfg, err := config.Parse()
	if err != nil {
		log.Fatal(err)
	}

	logger, err := zap.NewDevelopment(
		zap.IncreaseLevel(cfg.LogLevel),
		zap.AddStacktrace(zap.ErrorLevel),
	)
	if err != nil {
		log.Fatal(err)
	}

	fastSlowEmaStrategy, err := fastslowema.NewStrategy(
		cfg.Tinkoff.Token,
		cfg.InstrumentFIGI,
		cfg.CandleInterval,
		fastslowema.WithLogger(logger),
		fastslowema.WithAppName(cfg.Tinkoff.AppName),
		fastslowema.WithStopLoss(cfg.StopLossIndent),
		fastslowema.WithSmoothInterval(cfg.Strategy.FastEmaSmoothInterval, cfg.Strategy.SlowEmaSmoothInterval),
		fastslowema.WithFlatMaxDiff(cfg.Strategy.FlatMaxDiff),
	)
	if err != nil {
		logger.Fatal("Failed to parse config", zap.Error(err))
	}

	tinkoffBroker, err := tinkoff.New(
		cfg.Tinkoff.Token,
		cfg.Tinkoff.AccountID,
		cfg.InstrumentFIGI,
		cfg.TradedQuantity,
		tinkoff.WithLogger(logger),
		tinkoff.WithAppName(cfg.Tinkoff.AppName),
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

	logger.Info("Robot was started", zap.Any("config", cfg.Safe()))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	wg.Wait()
	logger.Info("Robot was stopped")
}
