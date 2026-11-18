package conversation

func setBrowserObserver(browser *Browser, kind string, callback func(browserObservation)) {
	previous := browser.observer.Load()
	observer := func(event browserObservation) {
		if event.kind == kind {
			if callback != nil {
				callback(event)
			}
			return
		}
		if previous != nil {
			(*previous)(event)
		}
	}
	browser.observer.Store(&observer)
}
