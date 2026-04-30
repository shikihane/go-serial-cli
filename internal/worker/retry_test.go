package worker_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"go-serial-cli/internal/worker"
)

func TestRetryPolicyDelayGrowsToCap(t *testing.T) {
	policy := worker.RetryPolicy{Initial: 250 * time.Millisecond, Max: 5 * time.Second}

	got := []time.Duration{
		policy.Delay(0),
		policy.Delay(1),
		policy.Delay(2),
		policy.Delay(10),
	}
	want := []time.Duration{
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		5 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays = %v, want %v", got, want)
	}
}

func TestRunWithRetryRetriesTransientErrors(t *testing.T) {
	var attempts int
	var sleeps []time.Duration
	err := worker.RunWithRetry(func() error {
		attempts++
		if attempts < 3 {
			return errors.New("temporary")
		}
		return nil
	}, worker.RetryOptions{
		Policy: worker.RetryPolicy{Initial: time.Millisecond, Max: 10 * time.Millisecond},
		Sleep: func(delay time.Duration) {
			sleeps = append(sleeps, delay)
		},
	})

	if err != nil {
		t.Fatalf("RunWithRetry returned error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Millisecond, 2 * time.Millisecond}) {
		t.Fatalf("sleeps = %v", sleeps)
	}
}

func TestRunWithRetryReportsRetries(t *testing.T) {
	var reported []time.Duration
	err := worker.RunWithRetry(func() error {
		if len(reported) == 0 {
			return errors.New("temporary")
		}
		return nil
	}, worker.RetryOptions{
		Policy: worker.RetryPolicy{Initial: time.Millisecond, Max: 10 * time.Millisecond},
		OnRetry: func(err error, delay time.Duration) {
			if err == nil {
				t.Fatal("OnRetry received nil error")
			}
			reported = append(reported, delay)
		},
		Sleep: func(time.Duration) {},
	})

	if err != nil {
		t.Fatalf("RunWithRetry returned error: %v", err)
	}
	if !reflect.DeepEqual(reported, []time.Duration{time.Millisecond}) {
		t.Fatalf("reported = %v", reported)
	}
}

func TestRunWithRetryStopsWhenStopConditionIsTrue(t *testing.T) {
	var attempts int
	err := worker.RunWithRetry(func() error {
		attempts++
		return errors.New("temporary")
	}, worker.RetryOptions{
		Policy:     worker.RetryPolicy{Initial: time.Millisecond, Max: 10 * time.Millisecond},
		ShouldStop: func() bool { return true },
		Sleep:      func(time.Duration) { t.Fatal("Sleep should not be called") },
	})

	if err == nil {
		t.Fatal("expected last error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRunWithRetryDoesNotRetryPermanentErrors(t *testing.T) {
	permanent := errors.New("bad config")
	var attempts int
	err := worker.RunWithRetry(func() error {
		attempts++
		return permanent
	}, worker.RetryOptions{
		Policy:      worker.RetryPolicy{Initial: time.Millisecond, Max: 10 * time.Millisecond},
		IsPermanent: func(err error) bool { return errors.Is(err, permanent) },
		Sleep:       func(time.Duration) { t.Fatal("Sleep should not be called") },
	})

	if !errors.Is(err, permanent) {
		t.Fatalf("RunWithRetry error = %v, want %v", err, permanent)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
