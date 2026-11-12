package conversation

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type CapacityEvent struct {
	Stage           string
	Kind            string
	Quota           string
	AggregateBytes  int64
	SnapshotBytes   int64
	AggregateQuota  int64
	SnapshotQuota   int64
	Files           int
	Eligible        int
	Protected       int
	Skipped         int
	Evicted         int
	ReclaimedBytes  int64
	CleanupFailures int
	Suppressed      int
}

func (b *Browser) unlockCapacity() {
	event := b.capacityEvent
	b.capacityEvent = nil
	b.capacityMu.Unlock()
	if event != nil && b.options.CapacityObserver != nil {
		b.options.CapacityObserver(*event)
	}
}

func (b *Browser) capacityDenied(event CapacityEvent, code string) string {
	event.Kind = code
	event.AggregateQuota = b.options.AggregateQuota
	event.SnapshotQuota = b.options.SnapshotQuota
	now := time.Now()
	if b.capacityNow != nil {
		now = b.capacityNow()
	}
	if !b.capacityLast.IsZero() && now.Sub(b.capacityLast) < time.Minute {
		b.capacitySuppressed++
	} else {
		event.Suppressed = b.capacitySuppressed
		b.capacitySuppressed = 0
		b.capacityLast = now
		b.capacityEvent = &event
	}
	return code
}

func (b *Browser) cacheUsage() (int64, int, error) {
	entries, err := os.ReadDir(b.cacheRoot)
	if err != nil {
		return 0, 0, err
	}
	return b.cacheEntryUsage(entries)
}

func (b *Browser) cacheEntryUsage(entries []os.DirEntry) (int64, int, error) {
	var total int64
	var files int
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "snapshot-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		info, err := os.Lstat(filepath.Join(b.cacheRoot, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return total, files, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files++
		if info.Size() > math.MaxInt64-total {
			return math.MaxInt64, files, errors.New("cache size overflow")
		}
		total += info.Size()
	}
	return total, files, nil
}

// Lock order is capacityMu, mu, job.mu. Disk operations run without registry locks.
func (b *Browser) capacityEligibleLocked(job *browseJob) bool {
	if job.retired || job.running || job.queued || job.readers != 0 || job.chainValidationRunning || (job.state != BrowseReady && job.state != BrowseFailed) {
		return false
	}
	for _, chain := range b.chains {
		if chain.refs == 0 {
			continue
		}
		for _, member := range chain.jobs {
			if member == job {
				return false
			}
		}
		if job.chainContextID == chain.id {
			return false
		}
	}
	return true
}

func (b *Browser) detachJobLocked(job *browseJob) expiredJob {
	job.retired = true
	delete(b.jobs, job.id)
	if b.dedup[job.dedupKey] == job.id {
		delete(b.dedup, job.dedupKey)
	}
	for _, chain := range b.chains {
		referenced := job.chainContextID == chain.id
		for _, member := range chain.jobs {
			referenced = referenced || member == job
		}
		if referenced && b.chainByKey[chain.key] == chain.id {
			delete(b.chainByKey, chain.key)
		}
	}
	item := expiredJob{index: job.index, sourceFile: job.source.file}
	job.index = nil
	job.source.file = nil
	return item
}

func (b *Browser) removeCapacityItem(item expiredJob) error {
	if item.sourceFile != nil {
		_ = item.sourceFile.Close()
	}
	if item.index == nil {
		return nil
	}
	if b.capacityRemove != nil {
		return b.capacityRemove(item.index)
	}
	err := item.index.remove()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (b *Browser) ensureCapacity(request *browseJob, index *snapshotIndex, stage string, admission bool) string {
	event := CapacityEvent{Stage: stage, Quota: "aggregate"}
	total, files, err := b.cacheUsage()
	event.AggregateBytes, event.Files = total, files
	if err != nil {
		return b.capacityDenied(event, "index_storage_unavailable")
	}
	if index != nil {
		info, err := os.Stat(index.path)
		if err != nil {
			return b.capacityDenied(event, "index_storage_unavailable")
		}
		event.SnapshotBytes = info.Size()
		if info.Size() > b.options.SnapshotQuota {
			event.Quota = "snapshot"
			return b.capacityDenied(event, "index_capacity_exceeded")
		}
	}
	type candidate struct {
		job       *browseJob
		preferred bool
		accessed  time.Time
	}
	var candidates []candidate
	fits := func() bool {
		return total < b.options.AggregateQuota || (!admission && total == b.options.AggregateQuota)
	}
	if fits() {
		return ""
	}
	b.mu.Lock()
	for _, job := range b.jobs {
		job.mu.Lock()
		if job.index == nil {
			event.Skipped++
		} else if job != request && b.capacityEligibleLocked(job) {
			preferred := request != nil && job.scopeID == request.scopeID && job.source.revision == request.source.revision && job.source.end < request.source.end
			candidates = append(candidates, candidate{job, preferred, job.lastAccess})
			event.Eligible++
		} else {
			event.Protected++
		}
		job.mu.Unlock()
	}
	b.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		a, c := candidates[i], candidates[j]
		if a.preferred != c.preferred {
			return a.preferred
		}
		if !a.accessed.Equal(c.accessed) {
			return a.accessed.Before(c.accessed)
		}
		return a.job.id < c.job.id
	})
	for _, candidate := range candidates {
		job := candidate.job
		b.mu.Lock()
		job.mu.Lock()
		if b.jobs[job.id] != job || job.index == nil || !b.capacityEligibleLocked(job) {
			job.mu.Unlock()
			b.mu.Unlock()
			continue
		}
		item := b.detachJobLocked(job)
		job.mu.Unlock()
		b.mu.Unlock()
		if err := b.removeCapacityItem(item); err != nil {
			event.CleanupFailures++
		}
		event.Evicted++
		before := total
		total, files, err = b.cacheUsage()
		event.AggregateBytes, event.Files = total, files
		if err != nil {
			return b.capacityDenied(event, "index_storage_unavailable")
		}
		if total < before {
			event.ReclaimedBytes += before - total
		}
		if fits() {
			return ""
		}
	}
	return b.capacityDenied(event, "index_capacity_exceeded")
}

func (b *Browser) observeCapacityOperation(stage string, job *browseJob, locked bool) {
	b.mu.Lock()
	observer := b.capacityOperationObserver
	b.mu.Unlock()
	if observer != nil {
		observer(stage, job, locked)
	}
}

func (b *Browser) writeWithCapacity(job *browseJob, stage, failureMessage string, write func() error) string {
	b.observeCapacityOperation(stage, job, false)
	b.capacityMu.Lock()
	defer b.unlockCapacity()
	b.observeCapacityOperation(stage, job, true)
	code := b.ensureCapacity(job, job.index, stage, false)
	if code == "" {
		if err := write(); err != nil {
			code = "index_failed"
		} else {
			code = b.ensureCapacity(job, job.index, stage, false)
		}
	}
	if code != "" {
		if code != "index_failed" {
			failureMessage = capacityFailurePage(code).Reason
		}
		b.failJobCapacityLocked(job, code, failureMessage, true)
	}
	return code
}

func capacityFailurePage(code string) *BrowsePage {
	message := "History preparation failed on this computer."
	if code == "index_capacity_exceeded" {
		message = "The conversation history cache reached its storage limit."
	}
	if code == "index_storage_unavailable" {
		message = "Older history cannot be prepared on this computer right now."
	}
	page := failPage(code, message, true)
	return &page
}
