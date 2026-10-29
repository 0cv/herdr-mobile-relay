package app

import "context"

// bindControl is called before starting the control goroutine and before Run.
// done covers callback return, retirement, and the callback's cancellation
// bridges. Cancellation is independently callable from Run's entry defer, not
// scheduled by a wrapper waiting for Run to return. Normal callback completion
// does not cancel the service. Even an optional absent upload manager must join.
func (o *installedUploadOwner) bindControl(cancel context.CancelFunc, done <-chan struct{}) func() {
	join := func() {
		cancel()
		<-done
	}
	o.controlJoin = join
	return join
}
