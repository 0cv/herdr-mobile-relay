package coordinator

import (
	"container/heap"
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	coordinatorIngressCapacity    = 128
	coordinatorCompletionCapacity = 32
	coordinatorLedgerCapacity     = 32
	protectedCompletionBatch      = 8
	defaultHerdrCapacity          = 8
	maxLedgerEntries              = 256
	ledgerRetention               = 24 * time.Hour
)

type InFlightOperation struct {
	OperationID OperationID
	Generation  int64
	cancel      context.CancelFunc
}

type PaneSlot struct {
	Generation int64
	Queue      []*scheduledOperation
	InFlight   *InFlightOperation
}

type SchedulerMetrics struct {
	IngressHighWater    uint64 `json:"ingress_high_water"`
	CompletionHighWater uint64 `json:"completion_high_water"`
	RejectedNotStarted  uint64 `json:"rejected_not_started"`
	ExpiredQueued       uint64 `json:"expired_queued"`
	Dispatched          uint64 `json:"dispatched"`
	Completed           uint64 `json:"completed"`
	HerdrInUse          int    `json:"herdr_in_use"`
	HerdrCapacity       int    `json:"herdr_capacity"`
	PaneSlots           int    `json:"pane_slots"`
	LatencySamples      uint64 `json:"latency_samples"`
	LatencyTotalMicros  uint64 `json:"latency_total_micros"`
	LatencyMaxMicros    uint64 `json:"latency_max_micros"`
}

type scheduledOperation struct {
	options    ScheduleOptions
	runner     EffectRunner
	sequence   uint64
	operation  OperationID
	generation int64
	waiters    []scheduleWaiter
	index      int
	cancel     context.CancelFunc
	dispatched bool
	finalize   func(*CommandResult, error) *CommandResult
}

type scheduleWaiter struct {
	response chan scheduleResponse
	replayed bool
}

type scheduleResponse struct {
	result   *CommandResult
	err      error
	replayed bool
}

type completionEvent struct {
	operation *scheduledOperation
	result    EffectResult
}

type ledgerPhaseUpdate struct {
	key        string
	generation int64
	result     *CommandResult
	response   chan bool
}

type ledgerReplayQuery struct {
	key         string
	payloadHash string
	response    chan ledgerReplayResponse
}

type ledgerReplayResponse struct {
	result *CommandResult
	found  bool
	err    error
}

type topologyUpdate struct {
	active      map[string]bool
	generations map[string]int64
	response    chan struct{}
}

type ledgerEntry struct {
	payloadHash string
	operation   *scheduledOperation
	result      *CommandResult
	generation  int64
	paneID      string
	createdAt   time.Time
}

type deadlineHeap []*scheduledOperation

func (h deadlineHeap) Len() int { return len(h) }
func (h deadlineHeap) Less(i, j int) bool {
	return h[i].options.Deadline.Before(h[j].options.Deadline)
}
func (h deadlineHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *deadlineHeap) Push(value any) {
	op := value.(*scheduledOperation)
	op.index = len(*h)
	*h = append(*h, op)
}
func (h *deadlineHeap) Pop() any {
	old := *h
	n := len(old)
	op := old[n-1]
	op.index = -1
	*h = old[:n-1]
	return op
}

type Scheduler struct {
	capacity int

	ingress        chan *scheduledOperation
	completions    chan completionEvent
	ledger         chan ledgerPhaseUpdate
	replays        chan ledgerReplayQuery
	topology       chan topologyUpdate
	cancelInflight chan struct{}
	stop           chan chan struct{}
	done           chan struct{}

	commandSequence atomic.Uint64
	operationSeq    atomic.Uint64
	closed          atomic.Bool

	metricsMu sync.RWMutex
	metrics   SchedulerMetrics

	generationCurrent func(string, int64) bool
}

func NewScheduler(capacity int) *Scheduler {
	if capacity <= 0 {
		capacity = defaultHerdrCapacity
	}
	s := &Scheduler{
		capacity:       capacity,
		ingress:        make(chan *scheduledOperation, coordinatorIngressCapacity),
		completions:    make(chan completionEvent, coordinatorCompletionCapacity),
		ledger:         make(chan ledgerPhaseUpdate, coordinatorLedgerCapacity),
		replays:        make(chan ledgerReplayQuery, coordinatorLedgerCapacity),
		topology:       make(chan topologyUpdate, 1),
		cancelInflight: make(chan struct{}, 1),
		stop:           make(chan chan struct{}),
		done:           make(chan struct{}),
	}
	s.metrics.HerdrCapacity = capacity
	go s.run()
	return s
}

// ApplyTopology publishes the latest committed pane inventory to the scheduler
// owner. A pane disappearance advances its slot generation before this method
// returns, so queued work from the old pane session cannot reach a replacement
// that later reuses the same pane ID.
func (s *Scheduler) ApplyTopology(active map[string]bool, paneGenerations ...map[string]int64) bool {
	if s.closed.Load() {
		return false
	}
	snapshot := make(map[string]bool, len(active))
	for paneID, present := range active {
		if present {
			snapshot[paneID] = true
		}
	}
	generations := make(map[string]int64)
	if len(paneGenerations) > 0 {
		for paneID, generation := range paneGenerations[0] {
			generations[paneID] = generation
		}
	}
	response := make(chan struct{})
	select {
	case s.topology <- topologyUpdate{active: snapshot, generations: generations, response: response}:
	case <-s.done:
		return false
	}
	select {
	case <-response:
		return true
	case <-s.done:
		return false
	}
}

func (s *Scheduler) SetGenerationCurrent(fn func(string, int64) bool) {
	s.generationCurrent = fn
}

func (s *Scheduler) NextCommandID() CommandID {
	return CommandID(s.commandSequence.Add(1))
}

func (s *Scheduler) Execute(ctx context.Context, options ScheduleOptions, runner EffectRunner) (*CommandResult, error) {
	return s.ExecuteAdmitted(ctx, options, runner, nil)
}

func (s *Scheduler) ExecuteAdmitted(
	ctx context.Context,
	options ScheduleOptions,
	runner EffectRunner,
	admitted func(),
) (*CommandResult, error) {
	if s.closed.Load() {
		if admitted != nil {
			admitted()
		}
		return nil, ErrClosed
	}
	if options.ReceivedAt.IsZero() {
		options.ReceivedAt = time.Now()
	}
	if options.ID == 0 {
		options.ID = s.NextCommandID()
	}
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(options.Deadline) {
		options.Deadline = callerDeadline
	}
	response := make(chan scheduleResponse, 1)
	op := &scheduledOperation{
		options:   options,
		runner:    runner,
		sequence:  uint64(options.ID),
		operation: OperationID(s.operationSeq.Add(1)),
		waiters:   []scheduleWaiter{{response: response}},
		index:     -1,
	}

	select {
	case s.ingress <- op:
		s.observeIngress()
		if admitted != nil {
			admitted()
		}
	default:
		s.addRejected()
		if admitted != nil {
			admitted()
		}
		return nil, ErrIngressFull
	}

	select {
	case reply := <-response:
		if reply.result != nil {
			reply.result.RequestID = options.RequestID
			reply.result.replayed = reply.replayed
		}
		return reply.result, reply.err
	case <-s.done:
		return nil, ErrClosed
	}
}

// UpdateLedgerResult replaces the accepted ledger result with the complete
// terminal confirmation result. Replays must preserve data and errors, not only
// the terminal phase.
func (s *Scheduler) UpdateLedgerResult(key string, generation int64, result *CommandResult) bool {
	if key == "" || s.closed.Load() {
		return false
	}
	response := make(chan bool, 1)
	update := ledgerPhaseUpdate{
		key:        key,
		generation: generation,
		result:     cloneResult(result),
		response:   response,
	}
	select {
	case s.ledger <- update:
	case <-s.done:
		return false
	}
	select {
	case applied := <-response:
		return applied
	case <-s.done:
		return false
	}
}

// ReplayLedger returns an existing exact operation without running its effect.
// found with a nil result means the matching operation is still in flight and
// the caller should submit normally so the scheduler can attach as a waiter.
func (s *Scheduler) ReplayLedger(key, payloadHash string) (*CommandResult, bool, error) {
	if key == "" || s.closed.Load() {
		return nil, false, ErrClosed
	}
	response := make(chan ledgerReplayResponse, 1)
	query := ledgerReplayQuery{key: key, payloadHash: payloadHash, response: response}
	select {
	case s.replays <- query:
	case <-s.done:
		return nil, false, ErrClosed
	}
	select {
	case replay := <-response:
		if replay.result != nil {
			replay.result.replayed = true
		}
		return replay.result, replay.found, replay.err
	case <-s.done:
		return nil, false, ErrClosed
	}
}

// CancelInflight signals the scheduler to cancel in-flight effects and reject
// new ingress without waiting for the drain to complete. Safe to call multiple
// times or before Close.
func (s *Scheduler) CancelInflight() {
	select {
	case s.cancelInflight <- struct{}{}:
	default:
	}
}

func (s *Scheduler) Close(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		select {
		case <-s.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ack := make(chan struct{})
	select {
	case s.stop <- ack:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) Metrics() SchedulerMetrics {
	s.metricsMu.RLock()
	defer s.metricsMu.RUnlock()
	return s.metrics
}

type schedulerLoop struct {
	s             *Scheduler
	slots         map[string]*PaneSlot
	relayQueue    []*scheduledOperation
	relayInFlight map[OperationID]*scheduledOperation
	ledger        map[string]*ledgerEntry
	knownActive   map[string]bool
	deadlines     deadlineHeap
	inUse         int
	stopping      bool
	stopAck       chan struct{}
	timer         *time.Timer
}

func (s *Scheduler) run() {
	defer close(s.done)

	l := &schedulerLoop{
		s:             s,
		slots:         make(map[string]*PaneSlot),
		relayInFlight: make(map[OperationID]*scheduledOperation),
		ledger:        make(map[string]*ledgerEntry),
		knownActive:   make(map[string]bool),
		timer:         time.NewTimer(time.Hour),
	}
	l.timer.Stop()
	defer l.timer.Stop()
	l.serve()
}

func (l *schedulerLoop) serve() {
	s := l.s
	for {
		if l.stopCompleted() {
			return
		}

		// Prefer pending topology before ordinary completion handling. The
		// state-generation validator below remains the correctness barrier.
		select {
		case update := <-s.topology:
			l.applyTopology(update)
			continue
		default:
		}

		// Give protected completions priority over ordinary ingress without
		// allowing a completion storm to starve admission or control events.
		for drained := 0; drained < protectedCompletionBatch; drained++ {
			select {
			case event := <-s.completions:
				l.complete(event)
			default:
				drained = protectedCompletionBatch
			}
		}
		if l.stopCompleted() {
			return
		}

		select {
		case op := <-s.ingress:
			l.admit(op)

		case event := <-s.completions:
			l.complete(event)

		case update := <-s.ledger:
			entry := l.ledger[update.key]
			applied := entry != nil && entry.generation == update.generation &&
				entry.result != nil && update.result != nil
			if applied {
				entry.result = cloneResult(update.result)
			}
			update.response <- applied

		case query := <-s.replays:
			entry := l.ledger[query.key]
			response := ledgerReplayResponse{}
			if entry != nil {
				response.found = true
				if entry.payloadHash != query.payloadHash {
					response.err = ErrConflict
				} else if entry.result != nil {
					response.result = cloneResult(entry.result)
				}
			}
			query.response <- response

		case update := <-s.topology:
			l.applyTopology(update)

		case <-l.timer.C:
			l.expireDeadlines()

		case <-s.cancelInflight:
			if !l.stopping {
				l.beginStop()
				l.resetTimer()
			}

		case ack := <-s.stop:
			l.stopAck = ack
			if !l.stopping {
				l.beginStop()
			}
			l.resetTimer()
		}
	}
}

func (l *schedulerLoop) stopCompleted() bool {
	if !l.stopping || l.inUse != 0 || l.stopAck == nil {
		return false
	}
	close(l.stopAck)
	return true
}

func (l *schedulerLoop) dropDeadline(op *scheduledOperation) {
	if op.index >= 0 {
		heap.Remove(&l.deadlines, op.index)
	}
}

func (l *schedulerLoop) resetTimer() {
	l.timer.Stop()
	for l.deadlines.Len() > 0 && l.deadlines[0].index < 0 {
		heap.Pop(&l.deadlines)
	}
	if l.deadlines.Len() == 0 {
		return
	}
	wait := time.Until(l.deadlines[0].options.Deadline)
	if wait < 0 {
		wait = 0
	}
	l.timer.Reset(wait)
}

func (l *schedulerLoop) beginStop() {
	l.stopping = true
	for _, slot := range l.slots {
		if slot.InFlight != nil && slot.InFlight.cancel != nil {
			slot.InFlight.cancel()
		}
		for _, op := range slot.Queue {
			l.s.reply(op, nil, ErrClosed)
		}
		slot.Queue = nil
	}
	for _, op := range l.relayInFlight {
		if op.cancel != nil {
			op.cancel()
		}
	}
	for _, op := range l.relayQueue {
		l.s.reply(op, nil, ErrClosed)
	}
	l.relayQueue = nil
}

func (l *schedulerLoop) invalidateSlot(paneID string, slot *PaneSlot, generation int64, keepLedgerKey string) {
	slot.Generation = generation
	if slot.InFlight != nil && slot.InFlight.cancel != nil {
		slot.InFlight.cancel()
	}
	for _, queued := range slot.Queue {
		l.dropDeadline(queued)
		l.replyStale(queued)
	}
	slot.Queue = nil
	l.pruneLedgerBelow(paneID, generation, keepLedgerKey)
}

func (l *schedulerLoop) pruneLedgerBelow(paneID string, generation int64, keepLedgerKey string) {
	for key, entry := range l.ledger {
		if key != keepLedgerKey && entry.paneID == paneID && entry.generation < generation {
			delete(l.ledger, key)
		}
	}
}

func (l *schedulerLoop) dispatch() {
	s := l.s
	now := time.Now()

	for len(l.relayQueue) > 0 && l.inUse < s.capacity && !l.stopping {
		op := l.relayQueue[0]
		l.relayQueue = l.relayQueue[1:]
		if !now.Before(op.options.Deadline) {
			l.dropDeadline(op)
			l.expire(op)
			continue
		}
		l.inUse++
		l.relayInFlight[op.operation] = op
		op.cancel = s.start(op, 0)
	}

	paneIDs := make([]string, 0, len(l.slots))
	for paneID := range l.slots {
		paneIDs = append(paneIDs, paneID)
	}
	sort.Strings(paneIDs)
	progress := true
	for progress && l.inUse < s.capacity && !l.stopping {
		progress = false
		for _, paneID := range paneIDs {
			if l.inUse >= s.capacity {
				break
			}
			slot := l.slots[paneID]
			if slot.InFlight != nil || len(slot.Queue) == 0 {
				continue
			}
			op := slot.Queue[0]
			slot.Queue = slot.Queue[1:]
			if op.generation != slot.Generation {
				l.dropDeadline(op)
				l.replyStale(op)
				progress = true
				continue
			}
			if !now.Before(op.options.Deadline) {
				l.dropDeadline(op)
				l.expire(op)
				progress = true
				continue
			}
			l.inUse++
			slot.InFlight = &InFlightOperation{
				OperationID: op.operation,
				Generation:  slot.Generation,
			}
			slot.InFlight.cancel = s.start(op, slot.Generation)
			progress = true
		}
	}
	s.setOwnerMetrics(l.inUse, len(l.slots))
	l.resetTimer()
}

func (l *schedulerLoop) complete(event completionEvent) {
	s := l.s
	s.observeCompletion()
	op := event.operation
	if op.options.RelayLevel {
		if _, ok := l.relayInFlight[op.operation]; !ok {
			return
		}
		delete(l.relayInFlight, op.operation)
	} else {
		slot := l.slots[op.options.PaneID]
		if slot == nil || slot.InFlight == nil || slot.InFlight.OperationID != op.operation {
			return
		}
		stale := slot.InFlight.Generation != slot.Generation
		if !stale && !op.options.AllowAbsent && s.generationCurrent != nil {
			stale = !s.generationCurrent(op.options.PaneID, slot.InFlight.Generation)
		}
		if event.result.BumpGeneration && !stale {
			slot.Generation++
			l.pruneLedgerBelow(op.options.PaneID, slot.Generation, op.options.LedgerKey)
		}
		slot.InFlight = nil
		if len(slot.Queue) == 0 && event.result.BumpGeneration && !stale && event.result.Result != nil && event.result.Result.Phase == "closed" {
			delete(l.slots, op.options.PaneID)
		}
		if stale {
			l.inUse--
			l.dropDeadline(op)
			s.metricsMu.Lock()
			s.metrics.Completed++
			s.metricsMu.Unlock()
			s.setOwnerMetrics(l.inUse, len(l.slots))
			l.replyStale(op)
			l.dispatch()
			return
		}
	}
	l.inUse--
	l.dropDeadline(op)
	if next := event.result.Continuation; next != nil {
		op.finalize = next.Finalize
		op.options.RelayLevel = false
		op.options.PaneID = next.PaneID
		op.options.AllowAbsent = next.AllowAbsent
		op.options.Deadline = minDeadline(op.options.Deadline, next.Deadline)
		op.generation = next.Generation
		op.runner = next.Runner
		op.dispatched = false
		slot := l.slots[next.PaneID]
		if slot == nil {
			slot = &PaneSlot{Generation: next.Generation}
			l.slots[next.PaneID] = slot
		}
		if next.Generation > slot.Generation && s.generationCurrent != nil && s.generationCurrent(next.PaneID, next.Generation) {
			l.invalidateSlot(next.PaneID, slot, next.Generation, op.options.LedgerKey)
		}
		if l.stopping {
			l.finish(op, nil, ErrClosed)
		} else if slot.Generation != next.Generation {
			l.replyStale(op)
		} else {
			heap.Push(&l.deadlines, op)
			slot.Queue = insertBySequence(slot.Queue, op)
		}
		l.dispatch()
		return
	}
	s.metricsMu.Lock()
	s.metrics.Completed++
	latency := time.Since(op.options.ReceivedAt)
	if latency > 0 {
		micros := uint64(latency.Microseconds())
		s.metrics.LatencySamples++
		s.metrics.LatencyTotalMicros += micros
		if micros > s.metrics.LatencyMaxMicros {
			s.metrics.LatencyMaxMicros = micros
		}
	}
	s.metricsMu.Unlock()
	s.setOwnerMetrics(l.inUse, len(l.slots))
	l.finish(op, event.result.Result, nil)
	l.dispatch()
}

func (l *schedulerLoop) applyTopology(update topologyUpdate) {
	for paneID := range l.knownActive {
		if update.active[paneID] {
			continue
		}
		slot := l.slots[paneID]
		if slot == nil {
			continue
		}
		l.invalidateSlot(paneID, slot, slot.Generation+1, "")
	}
	for paneID, generation := range update.generations {
		slot := l.slots[paneID]
		if slot == nil {
			slot = &PaneSlot{}
			l.slots[paneID] = slot
		}
		if generation <= slot.Generation {
			continue
		}
		l.invalidateSlot(paneID, slot, generation, "")
	}
	l.knownActive = update.active
	close(update.response)
	l.dispatch()
}

func (l *schedulerLoop) pruneLedger(now time.Time) {
	for key, entry := range l.ledger {
		if entry.operation == nil && now.Sub(entry.createdAt) >= ledgerRetention {
			delete(l.ledger, key)
		}
	}
	for len(l.ledger) >= maxLedgerEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range l.ledger {
			if entry.operation != nil {
				continue
			}
			if oldestKey == "" || entry.createdAt.Before(oldest) {
				oldestKey = key
				oldest = entry.createdAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(l.ledger, oldestKey)
	}
}

func (l *schedulerLoop) admit(op *scheduledOperation) {
	s := l.s
	l.pruneLedger(time.Now())
	if l.stopping {
		s.reply(op, nil, ErrClosed)
		return
	}
	if !time.Now().Before(op.options.Deadline) {
		s.replyNotStarted(op)
		return
	}
	if !op.options.RelayLevel {
		slot := l.slots[op.options.PaneID]
		if slot != nil && op.options.PaneGeneration < slot.Generation {
			l.replyStale(op)
			return
		}
	}
	if key := op.options.LedgerKey; key != "" {
		if existing, ok := l.ledger[key]; ok {
			if existing.payloadHash != op.options.PayloadHash {
				s.reply(op, nil, ErrConflict)
				return
			}
			if existing.result != nil {
				s.replyReplayed(op, cloneResult(existing.result), nil)
				return
			}
			for index := range op.waiters {
				op.waiters[index].replayed = true
			}
			existing.operation.waiters = append(existing.operation.waiters, op.waiters...)
			return
		}
		l.ledger[key] = &ledgerEntry{
			payloadHash: op.options.PayloadHash,
			operation:   op,
			paneID:      op.options.PaneID,
			createdAt:   op.options.ReceivedAt,
		}
	}
	heap.Push(&l.deadlines, op)
	if op.options.RelayLevel {
		l.relayQueue = insertBySequence(l.relayQueue, op)
	} else {
		slot := l.slots[op.options.PaneID]
		if slot == nil {
			slot = &PaneSlot{}
			l.slots[op.options.PaneID] = slot
		}
		if op.options.PaneGeneration > slot.Generation {
			l.invalidateSlot(op.options.PaneID, slot, op.options.PaneGeneration, op.options.LedgerKey)
		}
		if entry := l.ledger[op.options.LedgerKey]; entry != nil {
			entry.generation = slot.Generation
		}
		op.generation = slot.Generation
		slot.Queue = insertBySequence(slot.Queue, op)
	}
	l.dispatch()
}

func (l *schedulerLoop) expireDeadlines() {
	now := time.Now()
	for l.deadlines.Len() > 0 && !now.Before(l.deadlines[0].options.Deadline) {
		op := heap.Pop(&l.deadlines).(*scheduledOperation)
		if l.removeQueued(op) {
			l.expire(op)
		}
	}
	l.dispatch()
}

func (s *Scheduler) start(op *scheduledOperation, generation int64) context.CancelFunc {
	token := WorkerToken{
		OperationID: op.operation,
		PaneID:      op.options.PaneID,
		Generation:  generation,
		AllowAbsent: op.options.AllowAbsent,
		Deadline:    op.options.Deadline,
	}
	s.metricsMu.Lock()
	s.metrics.Dispatched++
	s.metricsMu.Unlock()
	ctx, cancel := context.WithDeadline(context.Background(), token.Deadline)
	op.dispatched = true
	go func() {
		defer cancel()
		result := op.runner.Run(ctx, token)
		s.completions <- completionEvent{operation: op, result: result}
	}()
	return cancel
}

func (l *schedulerLoop) removeQueued(op *scheduledOperation) bool {
	if op.options.RelayLevel {
		var removed bool
		l.relayQueue, removed = removeOperation(l.relayQueue, op)
		return removed
	}
	slot := l.slots[op.options.PaneID]
	if slot == nil || (slot.InFlight != nil && slot.InFlight.OperationID == op.operation) {
		return false
	}
	var removed bool
	slot.Queue, removed = removeOperation(slot.Queue, op)
	return removed
}

func (l *schedulerLoop) expire(op *scheduledOperation) {
	l.s.metricsMu.Lock()
	l.s.metrics.ExpiredQueued++
	l.s.metricsMu.Unlock()
	l.finish(op, notStartedResult(op), nil)
}

func (s *Scheduler) replyNotStarted(op *scheduledOperation) {
	s.reply(op, notStartedResult(op), nil)
}

func notStartedResult(op *scheduledOperation) *CommandResult {
	return &CommandResult{
		RequestID: op.options.RequestID,
		Action:    op.options.Action,
		OK:        false,
		Phase:     "not_started",
		Error:     queueRejectedRetrySafe,
		PaneID:    op.options.PaneID,
	}
}

func (l *schedulerLoop) replyStale(op *scheduledOperation) {
	if op.finalize == nil && op.options.LedgerKey != "" {
		delete(l.ledger, op.options.LedgerKey)
	}
	phase := "failed"
	publicError := ErrPaneReplaced.Error()
	if op.dispatched {
		phase = "dispatched_unknown"
		publicError = "command was dispatched before the pane session was replaced; outcome is unknown"
	}
	result := &CommandResult{
		RequestID: op.options.RequestID,
		Action:    op.options.Action,
		OK:        false,
		Phase:     phase,
		Error:     publicError,
		PaneID:    op.options.PaneID,
	}
	l.finish(op, result, nil)
}

func minDeadline(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (l *schedulerLoop) finish(op *scheduledOperation, result *CommandResult, err error) {
	if op.finalize != nil {
		result = op.finalize(result, err)
		op.finalize = nil
		err = nil
	}
	if entry := l.ledger[op.options.LedgerKey]; entry != nil {
		if result == nil || result.Phase == "not_started" {
			delete(l.ledger, op.options.LedgerKey)
		} else {
			entry.result = cloneResult(result)
			entry.operation = nil
		}
	}
	l.s.reply(op, result, err)
}

func (s *Scheduler) reply(op *scheduledOperation, result *CommandResult, err error) {
	if op.finalize != nil {
		result = op.finalize(result, err)
		op.finalize = nil
		err = nil
	}
	s.replyWithReplay(op, result, err, false)
}

func (s *Scheduler) replyReplayed(op *scheduledOperation, result *CommandResult, err error) {
	s.replyWithReplay(op, result, err, true)
}

func (s *Scheduler) replyWithReplay(op *scheduledOperation, result *CommandResult, err error, forceReplay bool) {
	for _, waiter := range op.waiters {
		reply := result
		if result != nil {
			reply = cloneResult(result)
		}
		waiter.response <- scheduleResponse{result: reply, err: err, replayed: forceReplay || waiter.replayed}
	}
}

func insertBySequence(queue []*scheduledOperation, op *scheduledOperation) []*scheduledOperation {
	at := sort.Search(len(queue), func(i int) bool { return queue[i].sequence > op.sequence })
	queue = append(queue, nil)
	copy(queue[at+1:], queue[at:])
	queue[at] = op
	return queue
}

func removeOperation(queue []*scheduledOperation, target *scheduledOperation) ([]*scheduledOperation, bool) {
	for i, op := range queue {
		if op == target {
			copy(queue[i:], queue[i+1:])
			queue[len(queue)-1] = nil
			return queue[:len(queue)-1], true
		}
	}
	return queue, false
}

func cloneResult(result *CommandResult) *CommandResult {
	if result == nil {
		return nil
	}
	cloned := *result
	return &cloned
}

func (s *Scheduler) observeIngress() {
	depth := uint64(len(s.ingress))
	s.metricsMu.Lock()
	if depth > s.metrics.IngressHighWater {
		s.metrics.IngressHighWater = depth
	}
	s.metricsMu.Unlock()
}

func (s *Scheduler) observeCompletion() {
	depth := uint64(len(s.completions))
	s.metricsMu.Lock()
	if depth > s.metrics.CompletionHighWater {
		s.metrics.CompletionHighWater = depth
	}
	s.metricsMu.Unlock()
}

func (s *Scheduler) addRejected() {
	s.metricsMu.Lock()
	s.metrics.RejectedNotStarted++
	s.metricsMu.Unlock()
}

func (s *Scheduler) setOwnerMetrics(inUse, slots int) {
	s.metricsMu.Lock()
	s.metrics.HerdrInUse = inUse
	s.metrics.PaneSlots = slots
	s.metricsMu.Unlock()
}
