// Package parallel provides a looping construct that enables parallel execution,
// inspired by OpenMP's "parallel for" pragmas in C, C++ and Fortran.
//
//	// without parallel construct ~290ms (N = 10000000)
//	for i := 0; i < N; i++ {
//	    outputArray[i] = sinc(inputArray[i] * math.Pi)
//	}
//
//	// with parallel construct ~90ms on 4 cores (N = 10000000)
//	parallel.For(N, func(i, _ int) {
//		outputArray[i] = sinc(inputArray[i] * math.Pi)
//	})
package parallel
