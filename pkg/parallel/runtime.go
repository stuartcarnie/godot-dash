package parallel

// ExecutorRuntime is an interface for executing a loop using a function.
// The loopBody function is called for each iteration of the loop and may be called
// on multiple goroutines.
//
// Executor is an implementation of ExecutorRuntime that executes the loopBody function in parallel.
type ExecutorRuntime interface {
	// For executes a loop with N iterations, calling loopBody for each iteration.
	For(N int, loopBody func(i, grID int) error) error
}

// executorFunc is a function type that implements the ExecutorRuntime interface.
type executorFunc func(N int, loopBody func(i, grID int) error) error

func (f executorFunc) For(N int, loopBody func(i, grID int) error) error {
	return f(N, loopBody)
}

// SerialExecutor returns an ExecutorRuntime that executes the loopBody function serially on the
// current goroutine.
//
// The primary use case for a SerialExecutor is in testing.
func SerialExecutor() ExecutorRuntime {
	return executorFunc(func(N int, loopBody func(i, grID int) error) error {
		for i := 0; i < N; i++ {
			if err := loopBody(i, 0); err != nil {
				return err
			}
		}
		return nil
	})
}
