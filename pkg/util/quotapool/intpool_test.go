// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package quotapool_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/quotapool"
	"golang.org/x/sync/errgroup"
)

// TestQuotaPoolBasic tests the minimal expected behavior of the quota pool
// with different sized quota pool and a varying number of goroutines, each
// acquiring a unit quota and releasing it immediately after.
func TestQuotaPoolBasic(t *testing.T) {
	defer leaktest.AfterTest(t)()

	quotas := []int64{1, 10, 100, 1000}
	goroutineCounts := []int{1, 10, 100}

	for _, quota := range quotas {
		for _, numGoroutines := range goroutineCounts {
			qp := quotapool.NewIntPool("test", quota)
			ctx := context.Background()
			resCh := make(chan error, numGoroutines)

			for i := 0; i < numGoroutines; i++ {
				go func() {
					alloc, err := qp.Acquire(ctx, 1)
					if err != nil {
						resCh <- err
						return
					}
					alloc.Release()
					resCh <- nil
				}()
			}

			for i := 0; i < numGoroutines; i++ {
				select {
				case <-time.After(5 * time.Second):
					t.Fatal("did not complete acquisitions within 5s")
				case err := <-resCh:
					if err != nil {
						t.Fatal(err)
					}
				}
			}

			if q := qp.ApproximateQuota(); q != quota {
				t.Fatalf("expected quota: %d, got: %d", quota, q)
			}
		}
	}
}

// TestQuotaPoolContextCancellation tests the behavior that for an ongoing
// blocked acquisition, if the context passed in gets canceled the acquisition
// gets canceled too with an error indicating so. This should not affect the
// available quota in the pool.
func TestQuotaPoolContextCancellation(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx, cancel := context.WithCancel(context.Background())
	qp := quotapool.NewIntPool("test", 1)
	alloc, err := qp.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error)
	go func() {
		_, canceledErr := qp.Acquire(ctx, 1)
		errCh <- canceledErr
	}()

	cancel()

	select {
	case <-time.After(5 * time.Second):
		t.Fatal("context cancellation did not unblock acquisitions within 5s")
	case err := <-errCh:
		if err != context.Canceled {
			t.Fatalf("expected context cancellation error, got %v", err)
		}
	}

	alloc.Release()

	if q := qp.ApproximateQuota(); q != 1 {
		t.Fatalf("expected quota: 1, got: %d", q)
	}
}

// TestQuotaPoolClose tests the behavior that for an ongoing blocked
// acquisition if the quota pool gets closed, all ongoing and subsequent
// acquisitions return an *ErrClosed.
func TestQuotaPoolClose(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	qp := quotapool.NewIntPool("test", 1)
	if _, err := qp.Acquire(ctx, 1); err != nil {
		t.Fatal(err)
	}
	const numGoroutines = 5
	resCh := make(chan error, numGoroutines)

	tryAcquire := func() {
		_, err := qp.Acquire(ctx, 1)
		resCh <- err
	}
	for i := 0; i < numGoroutines; i++ {
		go tryAcquire()
	}

	qp.Close("")

	// Second call should be a no-op.
	qp.Close("")

	for i := 0; i < numGoroutines; i++ {
		select {
		case <-time.After(5 * time.Second):
			t.Fatal("quota pool closing did not unblock acquisitions within 5s")
		case err := <-resCh:
			if _, isErrClosed := err.(*quotapool.ErrClosed); !isErrClosed {
				t.Fatal(err)
			}
		}
	}

	go tryAcquire()

	select {
	case <-time.After(5 * time.Second):
		t.Fatal("quota pool closing did not unblock acquisitions within 5s")
	case err := <-resCh:
		if _, isErrClosed := err.(*quotapool.ErrClosed); !isErrClosed {
			t.Fatal(err)
		}
	}
}

// TestQuotaPoolCanceledAcquisitions tests the behavior where we enqueue
// multiple acquisitions with canceled contexts and expect any subsequent
// acquisition with a valid context to proceed without error.
func TestQuotaPoolCanceledAcquisitions(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx, cancel := context.WithCancel(context.Background())
	qp := quotapool.NewIntPool("test", 1)
	alloc, err := qp.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	const numGoroutines = 5

	errCh := make(chan error)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			_, err := qp.Acquire(ctx, 1)
			errCh <- err
		}()
	}

	for i := 0; i < numGoroutines; i++ {
		select {
		case <-time.After(5 * time.Second):
			t.Fatal("context cancellations did not unblock acquisitions within 5s")
		case err := <-errCh:
			if err != context.Canceled {
				t.Fatalf("expected context cancellation error, got %v", err)
			}
		}
	}

	alloc.Release()
	go func() {
		_, err := qp.Acquire(context.Background(), 1)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition didn't go through within 5s")
	}
}

// TestQuotaPoolNoops tests that quota pool operations that should be noops are
// so, e.g. quotaPool.acquire(0) and quotaPool.release(0).
func TestQuotaPoolNoops(t *testing.T) {
	defer leaktest.AfterTest(t)()

	qp := quotapool.NewIntPool("test", 1)
	ctx := context.Background()
	initialAlloc, err := qp.Acquire(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	// Acquisition of blockedAlloc will block until initialAlloc is released.
	errCh := make(chan error)
	var blockedAlloc *quotapool.IntAlloc
	go func() {
		blockedAlloc, err = qp.Acquire(ctx, 1)
		errCh <- err
	}()

	// Allocation of zero should not block.
	emptyAlloc, err := qp.Acquire(ctx, 0)
	if err != nil {
		t.Fatalf("failed to acquire 0 quota: %v", err)
	}
	emptyAlloc.Release() // Release of 0 should do nothing

	initialAlloc.Release()
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("context cancellations did not unblock acquisitions within 5s")
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	}
	if q := qp.ApproximateQuota(); q != 0 {
		t.Fatalf("expected quota: 0, got: %d", q)
	}
	blockedAlloc.Release()
	if q := qp.ApproximateQuota(); q != 1 {
		t.Fatalf("expected quota: 1, got: %d", q)
	}
}

// TestQuotaPoolMaxQuota tests that Acquire cannot acquire more than the
// maximum amount with which the pool was initialized.
func TestQuotaPoolMaxQuota(t *testing.T) {
	defer leaktest.AfterTest(t)()

	const quota = 100
	qp := quotapool.NewIntPool("test", quota)
	ctx := context.Background()
	alloc, err := qp.Acquire(ctx, 2*quota)
	if err != nil {
		t.Fatal(err)
	}
	if got := alloc.Acquired(); got != quota {
		t.Fatalf("expected to acquire the max quota %d, instead got %d", quota, got)
	}
	alloc.Release()
	if q := qp.ApproximateQuota(); q != quota {
		t.Fatalf("expected quota: %d, got: %d", quota, q)
	}
}

// TestQuotaPoolCappedAcquisition verifies that when an acquisition request
// greater than the maximum quota is placed, we still allow the acquisition to
// proceed but after having acquired the maximum quota amount.
func TestQuotaPoolCappedAcquisition(t *testing.T) {
	defer leaktest.AfterTest(t)()

	const quota = 1
	qp := quotapool.NewIntPool("test", quota)
	alloc, err := qp.Acquire(context.Background(), quota*100)
	if err != nil {
		t.Fatal(err)
	}

	if q := qp.ApproximateQuota(); q != 0 {
		t.Fatalf("expected quota: %d, got: %d", 0, q)
	}

	alloc.Release()
	if q := qp.ApproximateQuota(); q != quota {
		t.Fatalf("expected quota: %d, got: %d", quota, q)
	}
}

// TestSlowAcquisition ensures that the SlowAcquisition callback is called
// when an Acquire call takes longer than the configured timeout.
func TestSlowAcquisition(t *testing.T) {
	// The test will set up an IntPool with 1 quota and a SlowAcquisition callback
	// which closes channels when called by the second goroutine. An initial call
	// to Acquire will take all of the quota. Then a second call with go should be
	// blocked leading to the callback being triggered.

	// In order to prevent the first call to Acquire from triggering the callback
	// we mark its context with a value.
	ctx := context.Background()
	type ctxKey int
	firstKey := ctxKey(1)
	firstCtx := context.WithValue(ctx, firstKey, "foo")
	slowCalled, acquiredCalled := make(chan struct{}), make(chan struct{})
	f := func(ctx context.Context, _ string, _ quotapool.Request, _ time.Time) func() {
		if ctx.Value(firstKey) != nil {
			return func() {}
		}
		close(slowCalled)
		return func() {
			close(acquiredCalled)
		}
	}
	qp := quotapool.NewIntPool("test", 1, quotapool.OnSlowAcquisition(time.Microsecond, f))
	alloc, err := qp.Acquire(firstCtx, 1)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = qp.Acquire(ctx, 1)
	}()
	select {
	case <-slowCalled:
	case <-time.After(time.Second):
		t.Fatalf("OnSlowAcquisition not called long after timeout")
	}
	select {
	case <-acquiredCalled:
		t.Fatalf("acquired callback called when insufficient quota was available")
	default:
	}
	alloc.Release()
	select {
	case <-slowCalled:
	case <-time.After(time.Second):
		t.Fatalf("OnSlowAcquisition acquired callback not called long after timeout")
	}
}

// BenchmarkIntQuotaPool benchmarks the common case where we have sufficient
// quota available in the pool and we repeatedly acquire and release quota.
func BenchmarkIntQuotaPool(b *testing.B) {
	qp := quotapool.NewIntPool("test", 1)
	ctx := context.Background()
	for n := 0; n < b.N; n++ {
		alloc, err := qp.Acquire(ctx, 1)
		if err != nil {
			b.Fatal(err)
		}
		alloc.Release()
	}
	qp.Close("")
}

// BenchmarkConcurrentIntQuotaPool benchmarks concurrent workers in a variety
// of ratios between adequate and inadequate quota to concurrently serve all
// workers.
func BenchmarkConcurrentIntQuotaPool(b *testing.B) {
	// test returns the arguments to b.Run for a given number of workers and
	// quantity of quota.
	test := func(workers, quota int) (string, func(b *testing.B)) {
		return fmt.Sprintf("workers=%d,quota=%d", workers, quota), func(b *testing.B) {
			qp := quotapool.NewIntPool("test", int64(quota), quotapool.LogSlowAcquisition)
			g, ctx := errgroup.WithContext(context.Background())
			runWorker := func(workerNum int) {
				g.Go(func() error {
					for i := workerNum; i < b.N; i += workers {
						alloc, err := qp.Acquire(ctx, 1)
						if err != nil {
							b.Fatal(err)
						}
						runtime.Gosched()
						alloc.Release()
					}
					return nil
				})
			}
			for i := 0; i < workers; i++ {
				runWorker(i)
			}
			if err := g.Wait(); err != nil {
				b.Fatal(err)
			}
			qp.Close("")
		}
	}
	for _, c := range []struct {
		workers, quota int
	}{
		{1, 1},
		{2, 2},
		{8, 4},
		{128, 4},
		{512, 128},
		{512, 513},
	} {
		b.Run(test(c.workers, c.quota))
	}
}

// BenchmarkIntQuotaPoolFunc benchmarks the common case where we have sufficient
// quota available in the pool and we repeatedly acquire and release quota.
func BenchmarkIntQuotaPoolFunc(b *testing.B) {
	qp := quotapool.NewIntPool("test", 1, quotapool.LogSlowAcquisition)
	ctx := context.Background()
	toAcquire := intRequest(1)
	for n := 0; n < b.N; n++ {
		alloc, err := qp.AcquireFunc(ctx, toAcquire.acquire)
		if err != nil {
			b.Fatal(err)
		} else if acquired := alloc.Acquired(); acquired != 1 {
			b.Fatalf("expected to acquire %d, got %d", 1, acquired)
		}
		alloc.Release()
	}
	qp.Close("")
}

// intRequest is a wrapper to create a IntRequestFunc from an int64.
type intRequest int64

func (ir intRequest) acquire(_ context.Context, v int64) (fulfilled bool, took int64) {
	if int64(ir) < v {
		return false, 0
	}
	return true, int64(ir)
}
