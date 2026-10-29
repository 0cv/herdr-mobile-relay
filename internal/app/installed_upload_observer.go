package app

import "context"

// The observer owns no resource release. Its caller joins it before collecting
// the owner, so a started callback cannot outlive physical upload release.
type installedUploadObserver struct {
	stop func() bool
	done <-chan struct{}
	// Private logical wait receipt; nil in production.
	waiting func()
}

func observeInstalledUpload(ctx context.Context, owner *installedUploadOwner) *installedUploadObserver {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		owner.begin()
	})
	return &installedUploadObserver{stop: stop, done: done}
}

func (o *installedUploadObserver) stopOrJoin() {
	if o.stop() {
		return
	}
	if o.waiting != nil {
		o.waiting()
	}
	<-o.done
}
