package worker

import "time"

type RetryPolicy struct {
	Initial time.Duration
	Max     time.Duration
}

type RetryOptions struct {
	Policy      RetryPolicy
	ShouldStop  func() bool
	IsPermanent func(error) bool
	OnRetry     func(error, time.Duration)
	Sleep       func(time.Duration)
}

func (p RetryPolicy) Delay(attempt int) time.Duration {
	initial := p.Initial
	if initial <= 0 {
		initial = 250 * time.Millisecond
	}
	maxDelay := p.Max
	if maxDelay <= 0 {
		maxDelay = 5 * time.Second
	}

	delay := initial
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func RunWithRetry(fn func() error, opts RetryOptions) error {
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if opts.IsPermanent != nil && opts.IsPermanent(err) {
			return err
		}
		if opts.ShouldStop != nil && opts.ShouldStop() {
			return err
		}
		delay := opts.Policy.Delay(attempt)
		if opts.OnRetry != nil {
			opts.OnRetry(err, delay)
		}
		sleep(delay)
	}
}
