package parallel

import (
	"context"
	"math"
	"runtime"
	"sync"
)

// Executor is the core type used to execute parallel loops.
// New instances are created using NewExecutor().
type Executor struct {
	numGoroutines    int
	parallelStrategy Strategy
}

// NewExecutor returns a new parallel executor instance.
func NewExecutor() *Executor {
	e := new(Executor)
	e.numGoroutines = DefaultNumGoroutines()
	return e
}

// NumGoroutines returns the number of goroutines that an executor is configured to use.
func (e *Executor) NumGoroutines() int {
	return e.numGoroutines
}

// WithNumGoroutines sets the number of goroutines for a parallel executor to use.
func (e *Executor) WithNumGoroutines(numGoroutines int) *Executor {
	e.numGoroutines = numGoroutines
	return e
}

// WithCPUProportion sets the number of goroutines based on a proportion of number of cores,
// with a minimum of 1.
func (e *Executor) WithCPUProportion(p float64) *Executor {
	numCPU := runtime.NumCPU()
	pCPU := p * float64(numCPU)
	e.numGoroutines = int(math.Max(pCPU, 1.0))
	return e
}

// WithStrategy sets the parallel strategy for execution.
// Different parallel strategies vary on how work items are distributed among goroutines.
// If either StrategyUseDefaults or an unrecognized value is specified, the
// defaults will be used for both For() and ForWithContext().
func (e *Executor) WithStrategy(strategyType StrategyType) *Executor {
	switch strategyType {
	case StrategyPreassignIndices:
		e.parallelStrategy = newContiguousBlocksStrategy()
	case StrategyFetchNextIndex:
		e.parallelStrategy = newAtomicCounterStrategy()
	default:
		e.parallelStrategy = nil
	}
	return e
}

// WithCustomStrategy sets a custom parallel strategy for execution.
// Defining custom strategies is an advanced feature. Most users should instead specify one of the
// strategies built into this package using WithStrategy().
func (e *Executor) WithCustomStrategy(customStrategy Strategy) *Executor {
	e.parallelStrategy = customStrategy
	return e
}

// For executes N iterations of a function body, where the iterations are parallelized among a
// number of goroutines and returns the first observed error from loopBody.
//
// Replacing existing for loops with this construct may accelerate parallelizable workloads.
// The first argument to the loop body function is the loop iteration index.
// If only this argument is used, then this function correlates directly to a for loop of the form:
//
//	for i := 0; i < N; i++ {
//		loopBody(i, _)
//	}
//
// The second argument to the loop body is the ID of the goroutine executing the loop iteration.
// Goroutine IDs range from 0 to NumGoroutines - 1.
// This ID can be used as part of the parallel logic; for example, the goroutine ID may be used
// such that each goroutine computes a partial result independently, and then a final result could
// be computed more quickly from the partial results immediately after the parallel loop.
//
// By default, For() uses the contiguous index blocks strategy.
//
// If loopBody returns an error, it will no longer be called.
func (e *Executor) For(N int, loopBody func(i, grID int) error) error {
	// use default contiguous blocks strategy if strategy has not been specified on executor
	strategy := e.parallelStrategy
	if strategy == nil {
		strategy = newContiguousBlocksStrategy()
	}

	var wg sync.WaitGroup
	wg.Add(e.numGoroutines)

	var (
		errOnce sync.Once
		loopErr error
	)

	for grID := 0; grID < e.numGoroutines; grID++ {
		go func(grID int) {
			defer wg.Done()
			// make index generator for this goroutine
			indexGenerator := strategy.IndexGenerator(e.numGoroutines, grID, N)
			// fetch work indices until work is complete
			for i := indexGenerator.Next(); i < N; i = indexGenerator.Next() {
				if err := loopBody(i, grID); err != nil {
					errOnce.Do(func() {
						loopErr = err
					})
					return
				}
			}
		}(grID)
	}

	wg.Wait()

	return loopErr
}

// ForWithContext is the same as For(), but includes a context argument to enable timeout,
// cancellation, and other context capabilities.
//
// If any loopBody returns an error, all cooperating goroutines will exit on their next iteration.
func (e *Executor) ForWithContext(ctx context.Context, N int,
	loopBody func(ctx context.Context, i, grID int) error) error {

	// use default atomic counter strategy if strategy has not been specified on executor
	strategy := e.parallelStrategy
	if strategy == nil {
		strategy = newContiguousBlocksStrategy()
	}

	var wg sync.WaitGroup
	wg.Add(e.numGoroutines)

	var (
		errOnce sync.Once
		loopErr error
	)

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for grID := 0; grID < e.numGoroutines; grID++ {
		go func(grID int) {
			defer wg.Done()
			// make index generator for this goroutine
			indexGenerator := strategy.IndexGenerator(e.numGoroutines, grID, N)
			// fetch work indices until work is complete
			for i := indexGenerator.Next(); i < N; i = indexGenerator.Next() {
				if loopCtx.Err() != nil {
					// don't do the work if the Context has been canceled
					return
				}

				if err := loopBody(loopCtx, i, grID); err != nil {
					errOnce.Do(func() {
						loopErr = err
						cancel()
					})
					return
				}
			}
		}(grID)
	}

	wg.Wait()

	return loopErr
}
