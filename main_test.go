package main

import (
	"context"
	"errors"
	"testing"
)

type serviceRunnerFunc func(context.Context) error

func (run serviceRunnerFunc) Run(ctx context.Context) error { return run(ctx) }

func TestRunServiceLoopsCancelsPeerAfterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failure := errors.New("polling failed")
	peerStopped := make(chan struct{})

	err := runServiceLoops(ctx, cancel,
		serviceRunnerFunc(func(context.Context) error { return failure }),
		serviceRunnerFunc(func(ctx context.Context) error {
			<-ctx.Done()
			close(peerStopped)
			return ctx.Err()
		}),
	)
	if !errors.Is(err, failure) {
		t.Fatalf("runServiceLoops() = %v, want service failure", err)
	}
	select {
	case <-peerStopped:
	default:
		t.Fatal("peer service was not stopped")
	}
}

func TestRunServiceLoopsTreatsShutdownCancellationAsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitForShutdown := serviceRunnerFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	if err := runServiceLoops(ctx, cancel, waitForShutdown, waitForShutdown); err != nil {
		t.Fatalf("runServiceLoops() = %v, want clean shutdown", err)
	}
}
