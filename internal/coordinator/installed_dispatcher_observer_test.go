package coordinator

import "sync"

// Observers are joined only after every callback/effect holder has an escape.
// Add from callbacks is finished by joining those callers before observers.Wait.
func dispatcherObserve(observers *sync.WaitGroup, call func()) <-chan struct{} {
	returned := make(chan struct{})
	observers.Add(1)
	go func() { defer observers.Done(); defer close(returned); call() }()
	return returned
}

func dispatcherStopForCleanup(d *Dispatcher) {
	d.installed.cancel()
	// Independently verified prompt scheduler stop; never rely on Dispatcher.Begin
	// to reach this step when testing whether Begin incorrectly waits for callers.
	_ = d.scheduler.beginInstalledDrain()
	d.stopWatchers()
}
