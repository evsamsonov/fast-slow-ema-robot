package fastslowema

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/evsamsonov/trading-indicators/indicator"
	"github.com/evsamsonov/trading-timeseries/timeseries"
	"github.com/evsamsonov/trengin"
	"github.com/evsamsonov/trengin/broker/tinkoff"
	investapi "github.com/tinkoff/invest-api-go-sdk"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	tinkoffHost = "invest-public-api.tinkoff.ru:443"
)

const (
	defaultFastSmoothInterval = 14
	defaultSlowSmoothInterval = 50
	defaultFlatMaxDiff        = 0
)

type Strategy struct {
	tinkoffToken       string
	instrumentFIGI     string
	marketDataClient   investapi.MarketDataServiceClient
	candleInterval     investapi.CandleInterval
	trendIndicator     *indicator.Trend
	actions            chan interface{}
	errors             chan error
	logger             *zap.Logger
	stopLossIndent     float64
	appName            string
	fastSmoothInterval int
	slowSmoothInterval int
	flatMaxDiff        float64
}

type Option func(*Strategy)

func WithLogger(logger *zap.Logger) Option {
	return func(s *Strategy) {
		s.logger = logger
	}
}

func WithAppName(appName string) Option {
	return func(s *Strategy) {
		s.appName = appName
	}
}

func WithStopLoss(indent float64) Option {
	return func(s *Strategy) {
		s.stopLossIndent = indent
	}
}

func WithSmoothInterval(fast, slow int) Option {
	return func(s *Strategy) {
		s.fastSmoothInterval = fast
		s.slowSmoothInterval = slow
	}
}

func WithFlatMaxDiff(diff float64) Option {
	return func(s *Strategy) {
		s.flatMaxDiff = diff
	}
}

type Trend float64

func (t Trend) IsUp() bool {
	return float64(t) == indicator.UpTrend
}

func (t Trend) IsDown() bool {
	return float64(t) == indicator.DownTrend
}

func (t Trend) IsFlat() bool {
	return float64(t) == indicator.FlatTrend
}

func NewStrategy(
	tinkoffToken,
	instrumentFIGI string,
	candleInterval investapi.CandleInterval,
	opts ...Option,
) (*Strategy, error) {
	conn, err := grpc.Dial(
		tinkoffHost,
		grpc.WithTransportCredentials(
			credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}), //nolint: gosec
		),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial: %w", err)
	}

	strategy := &Strategy{
		tinkoffToken:       tinkoffToken,
		instrumentFIGI:     instrumentFIGI,
		marketDataClient:   investapi.NewMarketDataServiceClient(conn),
		actions:            make(chan interface{}),
		errors:             make(chan error),
		logger:             zap.NewNop(),
		fastSmoothInterval: defaultFastSmoothInterval,
		slowSmoothInterval: defaultSlowSmoothInterval,
		flatMaxDiff:        defaultFlatMaxDiff,
		candleInterval:     candleInterval,
	}
	for _, opt := range opts {
		opt(strategy)
	}
	return strategy, nil
}

func (f *Strategy) Run(ctx context.Context) {
	ctx = f.ctxWithMetadata(ctx)
	series := timeseries.New()
	err := f.fillSeries(ctx, series, time.Now().AddDate(0, 0, -30), time.Now())
	if err != nil {
		f.sendError(fmt.Errorf("fill series: %w", err))
		return
	}
	fastEMA, err := indicator.NewExponentialMovingAverage(series, f.fastSmoothInterval)
	if err != nil {
		f.sendError(fmt.Errorf("new fast ema: %w", err))
		return
	}
	slowEMA, err := indicator.NewExponentialMovingAverage(series, f.slowSmoothInterval)
	if err != nil {
		f.sendError(fmt.Errorf("new slow ema: %w", err))
		return
	}
	f.trendIndicator = indicator.NewTrend(fastEMA, slowEMA, f.flatMaxDiff)

	getOpenSignal := f.getOpenSignalFunc()
	for {
		delay, err := f.getDelayByCandleInterval(f.candleInterval)
		if err != nil {
			f.sendError(fmt.Errorf("delay by candle interval: %w", err))
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			f.logger.Debug("Process trend")

			positionType, err := getOpenSignal(ctx, series)
			if err != nil {
				f.sendError(fmt.Errorf("get open signal: %w", err))
				return
			}
			if !positionType.IsValid() {
				continue
			}

			result, err := f.openPosition(ctx, positionType)
			if err != nil {
				f.sendError(fmt.Errorf("open position: %w", err))
				return
			}

			f.logger.Info("Position was opened", zap.Any("position", result.Position))

			if err := f.trackPosition(ctx, result, series); err != nil {
				f.sendError(fmt.Errorf("track position: %w", err))
				return
			}
		}
	}
}

type GetOpenSignal func(ctx context.Context, series *timeseries.TimeSeries) (trengin.PositionType, error)

func (f *Strategy) getOpenSignalFunc() GetOpenSignal {
	var previousTrend Trend
	return func(ctx context.Context, series *timeseries.TimeSeries) (trengin.PositionType, error) {
		currentTrend, err := f.getCurrentTrend(ctx, series)
		if err != nil {
			return 0, fmt.Errorf("get current trend: %w", err)
		}

		if currentTrend.IsFlat() {
			f.logger.Debug("Current trend is flat")
			return 0, nil
		}
		if previousTrend.IsFlat() {
			f.logger.Debug("Previous trend is flat")
			previousTrend = currentTrend
			return 0, nil
		}

		var positionType trengin.PositionType
		if previousTrend.IsUp() && currentTrend.IsDown() {
			positionType = trengin.Short
		} else if previousTrend.IsDown() && currentTrend.IsUp() {
			positionType = trengin.Long
		}
		return positionType, nil
	}
}

func (f *Strategy) openPosition(
	ctx context.Context,
	positionType trengin.PositionType,
) (trengin.OpenPositionActionResult, error) {
	action := trengin.NewOpenPositionAction(positionType, f.stopLossIndent, 0)
	if err := f.sendActionOrDone(ctx, action); err != nil {
		return trengin.OpenPositionActionResult{}, fmt.Errorf("open position action: %w", err)
	}

	result, err := action.Result(ctx)
	if err != nil {
		f.sendError(fmt.Errorf("open position result: %w", err))
		return trengin.OpenPositionActionResult{}, fmt.Errorf("open position result: %w", err)
	}
	return result, nil
}

func (f *Strategy) trackPosition(
	ctx context.Context,
	openPositionResult trengin.OpenPositionActionResult,
	series *timeseries.TimeSeries,
) error {
	trend, err := f.getCurrentTrend(ctx, series)
	if err != nil {
		return fmt.Errorf("get current trend: %w", err)
	}

	for {
		delay, err := f.getDelayByCandleInterval(f.candleInterval)
		if err != nil {
			return fmt.Errorf("delay by candle interval: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case p := <-openPositionResult.Closed:
			f.logger.Info("Position was closed", zap.Any("position", p))
			return nil
		case <-time.After(delay):
			f.logger.Debug("Track position")

			// todo перестановка стоп-лосса

			currentTrend, err := f.getCurrentTrend(ctx, series)
			if err != nil {
				return fmt.Errorf("get current trend: %w", err)
			}
			if currentTrend == trend {
				continue
			}

			f.logger.Info("Close position by changing trend",
				zap.Any("currentTrend", currentTrend),
				zap.Any("trend", trend),
			)

			action := trengin.NewClosePositionAction(openPositionResult.Position.ID)
			if err := f.sendActionOrDone(ctx, action); err != nil {
				return fmt.Errorf("close position action: %w", err)
			}
			_, err = action.Result(ctx)
			if err != nil {
				return fmt.Errorf("close position result: %w", err)
			}
		}
	}
}

func (f *Strategy) Actions() trengin.Actions {
	return f.actions
}

func (f *Strategy) Errors() <-chan error {
	return f.errors
}

func (f *Strategy) sendError(err error) {
	f.errors <- err
}

func (f *Strategy) ctxWithMetadata(ctx context.Context) context.Context {
	md := metadata.New(map[string]string{
		"Authorization": "Bearer " + f.tinkoffToken,
		"x-app-name":    f.appName,
	})
	return metadata.NewOutgoingContext(ctx, md)
}

func (f *Strategy) fillSeries(
	ctx context.Context,
	series *timeseries.TimeSeries,
	from time.Time,
	to time.Time,
) error {
	for ; from.Before(to); from = from.AddDate(0, 0, 1) {
		request := &investapi.GetCandlesRequest{
			Figi:     f.instrumentFIGI,
			From:     timestamppb.New(from),
			To:       timestamppb.New(from.AddDate(0, 0, 1)),
			Interval: f.candleInterval,
		}

		response, err := f.marketDataClient.GetCandles(ctx, request)
		if err != nil {
			f.logger.Error(
				"Failed to get candles",
				zap.Any("request", request),
				zap.Any("from", request.From.AsTime()),
				zap.Any("to", request.To.AsTime()),
			)
			return fmt.Errorf("get candles: %w", err)
		}

		f.logger.Debug(
			"Get candles",
			zap.Any("from", request.From.AsTime()),
			zap.Any("to", request.To.AsTime()),
			zap.Int("length", len(response.GetCandles())),
		)

		for _, candle := range response.GetCandles() {
			if !candle.IsComplete {
				f.logger.Debug("Skip incomplete candle", zap.Any("candleTime", candle.Time.AsTime()))
				continue
			}
			err := series.AddCandle(&timeseries.Candle{
				Time:   candle.Time.AsTime(),
				High:   tinkoff.NewMoneyValue(candle.High).ToFloat(),
				Low:    tinkoff.NewMoneyValue(candle.Low).ToFloat(),
				Open:   tinkoff.NewMoneyValue(candle.Open).ToFloat(),
				Close:  tinkoff.NewMoneyValue(candle.Close).ToFloat(),
				Volume: candle.Volume,
			})
			if err != nil {
				if err == timeseries.ErrUnexpectedTime {
					f.logger.Debug(
						"Skip candle with unexpected time",
						zap.Any("candleTime", candle.Time.AsTime()),
					)
					continue
				}
				return fmt.Errorf("add candle: %w", err)
			}
		}
	}
	return nil
}

func (f *Strategy) getCurrentTrend(ctx context.Context, series *timeseries.TimeSeries) (Trend, error) {
	if err := f.fillSeries(ctx, series, series.LastCandle().Time.Add(time.Minute), time.Now()); err != nil {
		return 0, fmt.Errorf("fill series: %w", err)
	}
	return Trend(f.trendIndicator.Calculate(series.Length() - 1)), nil
}

func (f *Strategy) sendActionOrDone(ctx context.Context, action interface{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case f.actions <- action:
	}
	return nil
}

func (f *Strategy) getDelayByCandleInterval(candleInterval investapi.CandleInterval) (time.Duration, error) {
	var t time.Time
	switch candleInterval {
	case investapi.CandleInterval_CANDLE_INTERVAL_1_MIN:
		t = time.Now().Add(time.Minute).Truncate(time.Minute).Add(30 * time.Second)
	case investapi.CandleInterval_CANDLE_INTERVAL_5_MIN:
		t = time.Now().Add(5 * time.Minute).Truncate(5 * time.Minute).Add(30 * time.Second)
	case investapi.CandleInterval_CANDLE_INTERVAL_15_MIN:
		t = time.Now().Add(15 * time.Minute).Truncate(15 * time.Minute).Add(30 * time.Second)
	case investapi.CandleInterval_CANDLE_INTERVAL_HOUR:
		t = time.Now().Add(time.Hour).Truncate(time.Hour).Add(30 * time.Second)
	default:
		return 0, fmt.Errorf("unexpected interval %v", candleInterval)
	}
	return time.Until(t), nil
}
