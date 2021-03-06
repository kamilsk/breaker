package breaker

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"time"
)

// New returns a new breaker, which can be interrupted only by a Close call.
//
//  interrupter := breaker.New()
//  go background.Job().Do(interrupter)
//
//  <-time.After(time.Minute)
//  interrupter.Close()
//
func New() Interface {
	return newBreaker().trigger()
}

// BreakByChannel returns a new breaker based on the channel.
//
//  signal := make(chan struct{})
//  go func() {
//  	<-time.After(time.Minute)
//  	close(signal)
//  }()
//
//  interrupter := breaker.BreakByChannel(signal)
//  defer interrupter.Close()
//
//  background.Job().Do(interrupter)
//
func BreakByChannel(signal <-chan struct{}) Interface {
	return (&channelBreaker{newBreaker(), make(chan struct{}), signal}).trigger()
}

// BreakByContext returns a new breaker based on the Context.
//
//  interrupter := breaker.BreakByContext(context.WithTimeout(req.Context(), time.Minute))
//  defer interrupter.Close()
//
//  background.Job().Do(interrupter)
//
func BreakByContext(ctx context.Context, cancel context.CancelFunc) Interface {
	return (&contextBreaker{ctx, cancel}).trigger()
}

// BreakByDeadline closes the Done channel when the deadline occurs.
//
//  interrupter := breaker.BreakByDeadline(time.Now().Add(time.Minute))
//  defer interrupter.Close()
//
//  background.Job().Do(interrupter)
//
func BreakByDeadline(deadline time.Time) Interface {
	timeout := time.Until(deadline)
	if timeout < 0 {
		return closedBreaker()
	}
	return newTimeoutBreaker(timeout).trigger()
}

// BreakBySignal closes the Done channel when the breaker will receive OS signals.
//
//  interrupter := breaker.BreakBySignal(os.Interrupt)
//  defer interrupter.Close()
//
//  background.Job().Do(interrupter)
//
func BreakBySignal(sig ...os.Signal) Interface {
	if len(sig) == 0 {
		return closedBreaker()
	}
	return newSignalBreaker(sig).trigger()
}

// BreakByTimeout closes the Done channel when the timeout happens.
//
//  interrupter := breaker.BreakByTimeout(time.Minute)
//  defer interrupter.Close()
//
//  background.Job().Do(interrupter)
//
func BreakByTimeout(timeout time.Duration) Interface {
	if timeout < 0 {
		return closedBreaker()
	}
	return newTimeoutBreaker(timeout).trigger()
}

// ToContext converts the breaker into the Context.
//
//  interrupter := breaker.Multiplex(
//  	breaker.BreakBySignal(os.Interrupt),
//  	breaker.BreakByTimeout(time.Minute),
//  )
//  defer interrupter.Close()
//
//  request, err := http.NewRequestWithContext(breaker.ToContext(interrupter), ...)
//  if err != nil { handle(err) }
//
//  response, err := http.DefaultClient.Do(request)
//  if err != nil { handle(err) }
//  handle(response)
//
func ToContext(br Interface) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-br.Done()
		cancel()
	}()
	return ctx
}

func closedBreaker() Interface {
	br := newBreaker()
	br.Close()
	return br
}

func newBreaker() *breaker {
	return &breaker{signal: make(chan struct{})}
}

type breaker struct {
	closer sync.Once
	signal chan struct{}
}

// Close closes the Done channel and releases resources associated with it.
func (br *breaker) Close() {
	br.closer.Do(func() { close(br.signal) })
}

// Done returns a channel that's closed when a cancellation signal occurred.
func (br *breaker) Done() <-chan struct{} {
	return br.signal
}

// Err returns a non-nil error if the Done channel is closed and nil otherwise.
// After Err returns a non-nil error, successive calls to Err return the same error.
func (br *breaker) Err() error {
	select {
	case <-br.signal:
		return Interrupted
	default:
		return nil
	}
}

// IsReleased returns true if resources associated with the breaker were released.
//
// Deprecated: see the extended interface.
func (br *breaker) IsReleased() bool {
	return br.Err() != nil
}

func (br *breaker) trigger() Interface {
	return br
}

type channelBreaker struct {
	*breaker
	internal chan struct{}
	external <-chan struct{}
}

// Close closes the Done channel and releases resources associated with it.
func (br *channelBreaker) Close() {
	br.closer.Do(func() { close(br.internal) })
}

// trigger starts listening to the internal signal to close the Done channel.
func (br *channelBreaker) trigger() Interface {
	go func() {
		select {
		case <-br.external:
			br.Close()
		case <-br.internal:
		}
		close(br.signal)
	}()
	return br
}

type contextBreaker struct {
	context.Context
	cancel context.CancelFunc
}

// Close closes the Done channel and releases resources associated with it.
func (br *contextBreaker) Close() {
	br.cancel()
}

// IsReleased returns true if resources associated with the breaker were released.
//
// Deprecated: see the extended interface.
func (br *contextBreaker) IsReleased() bool {
	return br.Err() != nil
}

func (br *contextBreaker) trigger() Interface {
	return br
}

func newSignalBreaker(signals []os.Signal) *signalBreaker {
	return &signalBreaker{newBreaker(), make(chan struct{}), make(chan os.Signal, len(signals)), signals}
}

type signalBreaker struct {
	*breaker
	internal chan struct{}
	external chan os.Signal
	signals  []os.Signal
}

// Close closes the Done channel and releases resources associated with it.
func (br *signalBreaker) Close() {
	br.closer.Do(func() { close(br.internal) })
}

// trigger starts listening to the required signals to close the Done channel.
func (br *signalBreaker) trigger() Interface {
	go func() {
		signal.Notify(br.external, br.signals...)
		select {
		case <-br.external:
			br.Close()
		case <-br.internal:
		}
		signal.Stop(br.external)
		close(br.external)
		close(br.signal)
	}()
	return br
}

func newTimeoutBreaker(timeout time.Duration) *timeoutBreaker {
	return &timeoutBreaker{newBreaker(), make(chan struct{}), time.NewTimer(timeout)}
}

type timeoutBreaker struct {
	*breaker
	internal chan struct{}
	external *time.Timer
}

// Close closes the Done channel and releases resources associated with it.
func (br *timeoutBreaker) Close() {
	br.closer.Do(func() { close(br.internal) })
}

// trigger starts listening to the internal timer to close the Done channel.
func (br *timeoutBreaker) trigger() Interface {
	go func() {
		select {
		case <-br.external.C:
			br.Close()
		case <-br.internal:
		}
		stop(br.external)
		close(br.signal)
	}()
	return br
}

func stop(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
