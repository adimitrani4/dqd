package pipe

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/soluto/dqd/metrics"
	v1 "github.com/soluto/dqd/v1"
)

func (w *Worker) handleErrorRequest(ctx *requestContext, err error) {
	m := ctx.Message()
	if !m.Abort() {
		if w.writeToErrorSource {
			err = w.errorSource.Produce(ctx, &v1.RawMessage{m.Data()})
		}
		if err != nil {
			logger.Error().Err(err).Msg("Failed to process message")
		}
	}
}

func (w *Worker) handleRequest(ctx *requestContext) (_ *v1.RawMessage, err error) {
	defer func() {
		source := ctx.Source()
		t := float64(time.Since(ctx.StartTime())) / float64(time.Second)
		metrics.HandlerProcessingHistogram.WithLabelValues(w.name, source, strconv.FormatBool(err != nil)).Observe(t)
	}()
	return w.handler.Handle(ctx, ctx.Message())
}

func (w *Worker) handleResults(ctx context.Context, results chan *requestContext) error {
	done := make(chan error)
	defer close(done)
	for reqCtx := range results {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return nil
		default:
		}
		go func() {
			m, err := reqCtx.Result()
			defer func() {
				t := float64(time.Since(reqCtx.StartTime())) / float64(time.Second)
				metrics.PipeProcessingMessagesHistogram.WithLabelValues(w.name, reqCtx.Source(), strconv.FormatBool(err != nil)).Observe(t)
			}()
			if err != nil {
				w.handleErrorRequest(reqCtx, err)
			} else if m != nil && w.output != nil {
				err := w.output.Produce(reqCtx, m)
				if err != nil {
					w.handleErrorRequest(reqCtx, err)
				}
			}

		}()
	}
	return nil
}

func (w *Worker) readMessages(ctx context.Context, messages chan *requestContext, results chan *requestContext) error {
	maxConcurrencyGauge := metrics.WorkerMaxConcurrencyGauge.WithLabelValues(w.name)
	batchSizeGauge := metrics.WorkerBatchSizeGauge.WithLabelValues(w.name)

	var count, lastBatch int64
	maxItems := int64(w.concurrencyStartingPoint)
	minConcurrency := int64(w.minConcurrency)
	defer close(messages)

	maxConcurrencyGauge.Set(float64(maxItems))

	// Handle messages
	go func() {
		for message := range messages {
			select {
			case <-ctx.Done():
				return
			default:
			}
			for count >= maxItems {
				time.Sleep(10 * time.Millisecond)
			}

			atomic.AddInt64(&count, 1)

			go func(r *requestContext) {
				result, err := w.handler.Handle(r, r.Message())
				atomic.AddInt64(&count, -1)
				if !w.fixedRate {
					atomic.AddInt64(&lastBatch, 1)
				}
				results <- r.WithResult(result, err)
			}(message)
		}
	}()

	// Handle throughput
	if !w.fixedRate {
		go func() {
			var prev int64
			timer := time.NewTimer(w.dynamicRateBatchWindow)
			shouldUpscale := true
			logger.Debug().Int64("concurrency", maxItems).Msg("Using dynamic concurrency")
			for {
				timer.Reset(w.dynamicRateBatchWindow)

				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}

				curr := atomic.SwapInt64(&lastBatch, 0)
				batchSizeGauge.Set(float64(curr))

				if curr == 0 {
					continue
				}
				if curr < prev {
					shouldUpscale = !shouldUpscale
				}
				if shouldUpscale {
					atomic.AddInt64(&maxItems, 1)
				} else if maxItems > minConcurrency {
					atomic.AddInt64(&maxItems, -1)
				}
				maxConcurrencyGauge.Set(float64(maxItems))

				prev = curr
				logger.Debug().Int64("concurrency", maxItems).Float64("rate", float64(curr)/w.dynamicRateBatchWindow.Seconds()).Msg("tuning concurrency")
			}
		}()
	}
	done := make(chan error)
	defer close(done)
	for _, s := range w.sources {
		go func(s *v1.Source) {
			logger.Info().Str("source", s.Name).Msg("Start reading from source")
			consumer := s.CreateConsumer()
			err := consumer.Iter(ctx, v1.NextMessage(func(m v1.Message) {
				messages <- createRequestContext(ctx, s.Name, m)
			}))
			if err != nil {
				done <- err
			}
		}(s)
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (w *Worker) Start(ctx context.Context) error {
	logger.Info().Msg("Starting pipe")
	messages := make(chan *requestContext, w.minConcurrency)
	defer close(messages)
	results := make(chan *requestContext, w.minConcurrency)
	defer close(results)
	done := make(chan error)

	innerContext, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		done <- w.readMessages(innerContext, messages, results)
	}()

	go func() {
		done <- w.handleResults(innerContext, results)
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}
