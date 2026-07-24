package coordinator

import (
	"container/heap"
	"context"
	"log/slog"
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
	CommandID   CommandID
	Generation  uint64
	StartedAt   time.Time
	cancel      context.CancelFunc
}

type PaneSlot struct {
	Generation uint64
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

type scheduleRequest struct {
	operation *scheduledOperation
}

type scheduledOperation struct {
	options    ScheduleOptions
	runner     EffectRunner
	sequence   uint64
	operation  OperationID
	generation uint64
	waiters    []scheduleWaiter
	index      int
	cancel     context.CancelFunc
	dispatched bool
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
	generation uint64
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
	generations map[string]uint64
	response    chan struct{}
}

type ledgerEntry struct {
	payloadHash string
	operation   *scheduledOperation
	result      *CommandResult
	generation  uint64
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
	logger   *slog.Logger
	capacity int

	ingress        chan scheduleRequest
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

	generationCurrent func(string, uint64) bool
}

func NewScheduler(capacity int, logger *slog.Logger) *Scheduler {
	if capacity <= 0 {
		capacity = defaultHerdrCapacity
	}
	s := &Scheduler{
		logger:         logger,
		capacity:       capacity,
		ingress:        make(chan scheduleRequest, coordinatorIngressCapacity),
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
func (s *Scheduler) ApplyTopology(active map[string]bool, paneGenerations ...map[string]uint64) bool {
	if s.closed.Load() {
		return false
	}
	snapshot := make(map[string]bool, len(active))
	for paneID, present := range active {
		if present {
			snapshot[paneID] = true
		}
	}
	generations := make(map[string]uint64)
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

func (s *Scheduler) SetGenerationCurrent(fn func(string, uint64) bool) {
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
	case s.ingress <- scheduleRequest{operation: op}:
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

// UpdateLedgerPhase synchronously applies a confirmation-watch result through
// the scheduler owner. A false return means the operation was invalidated by a
// generation change, pruned, or the scheduler is closing.
func (s *Scheduler) UpdateLedgerPhase(key string, generation uint64, phase string) bool {
	result := &CommandResult{OK: phase == "confirmed", Phase: phase}
	return s.UpdateLedgerResult(key, generation, result)
}

// UpdateLedgerResult replaces the accepted ledger result with the complete
// terminal confirmation result. Replays must preserve data and errors, not only
// the terminal phase.
func (s *Scheduler) UpdateLedgerResult(key string, generation uint64, result *CommandResult) bool {
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

func (s *Scheduler) run() {
	defer close(s.done)

	slots := make(map[string]*PaneSlot)
	var relayQueue []*scheduledOperation
	relayInFlight := make(map[OperationID]*scheduledOperation)
	ledger := make(map[string]*ledgerEntry)
	knownActive := make(map[string]bool)
	deadlines := deadlineHeap{}
	heap.Init(&deadlines)
	inUse := 0
	stopping := false
	var stopAck chan struct{}

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		for deadlines.Len() > 0 && deadlines[0].index < 0 {
			heap.Pop(&deadlines)
		}
		if deadlines.Len() == 0 {
			return
		}
		wait := time.Until(deadlines[0].options.Deadline)
		if wait < 0 {
			wait = 0
		}
		timer.Reset(wait)
	}

	var dispatch func()
	dispatch = func() {
		now := time.Now()

		for len(relayQueue) > 0 && inUse < s.capacity && !stopping {
			op := relayQueue[0]
			relayQueue = relayQueue[1:]
			if !now.Before(op.options.Deadline) {
				if op.index >= 0 {
					heap.Remove(&deadlines, op.index)
				}
				s.expire(op, ledger)
				continue
			}
			inUse++
			relayInFlight[op.operation] = op
			op.cancel = s.start(op, 0)
		}

		paneIDs := make([]string, 0, len(slots))
		for paneID := range slots {
			paneIDs = append(paneIDs, paneID)
		}
		sort.Strings(paneIDs)
		progress := true
		for progress && inUse < s.capacity && !stopping {
			progress = false
			for _, paneID := range paneIDs {
				if inUse >= s.capacity {
					break
				}
				slot := slots[paneID]
				if slot.InFlight != nil || len(slot.Queue) == 0 {
					continue
				}
				op := slot.Queue[0]
				slot.Queue = slot.Queue[1:]
				if op.generation != slot.Generation {
					if op.index >= 0 {
						heap.Remove(&deadlines, op.index)
					}
					s.replyStale(op, ledger)
					progress = true
					continue
				}
				if !now.Before(op.options.Deadline) {
					if op.index >= 0 {
						heap.Remove(&deadlines, op.index)
					}
					s.expire(op, ledger)
					progress = true
					continue
				}
				inUse++
				slot.InFlight = &InFlightOperation{
					OperationID: op.operation,
					CommandID:   op.options.ID,
					Generation:  slot.Generation,
					StartedAt:   now,
				}
				slot.InFlight.cancel = s.start(op, slot.Generation)
				progress = true
			}
		}
		s.setOwnerMetrics(inUse, len(slots))
		resetTimer()
	}

	completeAndRedispatch := func(event completionEvent) {
		s.observeCompletion()
		op := event.operation
		if op.options.RelayLevel {
			if _, ok := relayInFlight[op.operation]; !ok {
				return
			}
			delete(relayInFlight, op.operation)
		} else {
			slot := slots[op.options.PaneID]
			if slot == nil || slot.InFlight == nil || slot.InFlight.OperationID != op.operation {
				return
			}
			stale := slot.InFlight.Generation != slot.Generation
			if !stale && s.generationCurrent != nil {
				stale = !s.generationCurrent(op.options.PaneID, slot.InFlight.Generation)
			}
			if event.result.BumpGeneration && !stale {
				slot.Generation++
				for key, entry := range ledger {
					if key != op.options.LedgerKey &&
						entry.paneID == op.options.PaneID &&
						entry.generation < slot.Generation {
						delete(ledger, key)
					}
				}
			}
			slot.InFlight = nil
			if len(slot.Queue) == 0 && event.result.BumpGeneration && !stale && event.result.Result != nil && event.result.Result.Phase == "closed" {
				delete(slots, op.options.PaneID)
			}
			if stale {
				inUse--
				if op.index >= 0 {
					heap.Remove(&deadlines, op.index)
				}
				s.metricsMu.Lock()
				s.metrics.Completed++
				s.metricsMu.Unlock()
				s.setOwnerMetrics(inUse, len(slots))
				s.replyStale(op, ledger)
				dispatch()
				return
			}
		}
		inUse--
		if op.index >= 0 {
			heap.Remove(&deadlines, op.index)
		}
		if key := op.options.LedgerKey; key != "" {
			entry := ledger[key]
			if entry != nil {
				if event.result.Result == nil || event.result.Result.Phase == "not_started" {
					delete(ledger, key)
				} else {
					entry.result = cloneResult(event.result.Result)
					entry.operation = nil
				}
			}
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
		s.setOwnerMetrics(inUse, len(slots))
		s.reply(op, event.result.Result, nil)
		dispatch()
	}

	applyTopology := func(update topologyUpdate) {
		for paneID := range knownActive {
			if update.active[paneID] {
				continue
			}
			slot := slots[paneID]
			if slot == nil {
				continue
			}
			slot.Generation++
			if slot.InFlight != nil && slot.InFlight.cancel != nil {
				slot.InFlight.cancel()
			}
			for _, op := range slot.Queue {
				if op.index >= 0 {
					heap.Remove(&deadlines, op.index)
				}
				s.replyStale(op, ledger)
			}
			slot.Queue = nil
			for key, entry := range ledger {
				if entry.paneID == paneID && entry.generation < slot.Generation {
					delete(ledger, key)
				}
			}
		}
		for paneID, generation := range update.generations {
			slot := slots[paneID]
			if slot == nil {
				slot = &PaneSlot{}
				slots[paneID] = slot
			}
			if generation <= slot.Generation {
				continue
			}
			slot.Generation = generation
			if slot.InFlight != nil && slot.InFlight.cancel != nil {
				slot.InFlight.cancel()
			}
			for _, op := range slot.Queue {
				if op.index >= 0 {
					heap.Remove(&deadlines, op.index)
				}
				s.replyStale(op, ledger)
			}
			slot.Queue = nil
			for key, entry := range ledger {
				if entry.paneID == paneID && entry.generation < slot.Generation {
					delete(ledger, key)
				}
			}
		}
		knownActive = update.active
		close(update.response)
		dispatch()
	}

	pruneLedger := func(now time.Time) {
		for key, entry := range ledger {
			if entry.operation == nil && now.Sub(entry.createdAt) >= ledgerRetention {
				delete(ledger, key)
			}
		}
		for len(ledger) >= maxLedgerEntries {
			var oldestKey string
			var oldest time.Time
			for key, entry := range ledger {
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
			delete(ledger, oldestKey)
		}
	}

	for {
		if stopping && inUse == 0 && stopAck != nil {
			close(stopAck)
			return
		}

		// Prefer pending topology before ordinary completion handling. The
		// state-generation validator below remains the correctness barrier.
		select {
		case update := <-s.topology:
			applyTopology(update)
			continue
		default:
		}

		// Give protected completions priority over ordinary ingress without
		// allowing a completion storm to starve admission or control events.
		for drained := 0; drained < protectedCompletionBatch; drained++ {
			select {
			case event := <-s.completions:
				completeAndRedispatch(event)
			default:
				drained = protectedCompletionBatch
			}
		}
		if stopping && inUse == 0 && stopAck != nil {
			close(stopAck)
			return
		}

		select {
		case request := <-s.ingress:
			op := request.operation
			pruneLedger(time.Now())
			if stopping {
				s.reply(op, nil, ErrClosed)
				continue
			}
			if !time.Now().Before(op.options.Deadline) {
				s.replyNotStarted(op)
				continue
			}
			if !op.options.RelayLevel {
				slot := slots[op.options.PaneID]
				if slot != nil && op.options.PaneGeneration < slot.Generation {
					s.replyStale(op, ledger)
					continue
				}
			}
			if key := op.options.LedgerKey; key != "" {
				if existing, ok := ledger[key]; ok {
					if existing.payloadHash != op.options.PayloadHash {
						s.reply(op, nil, ErrConflict)
						continue
					}
					if existing.result != nil {
						s.replyReplayed(op, cloneResult(existing.result), nil)
						continue
					}
					for index := range op.waiters {
						op.waiters[index].replayed = true
					}
					existing.operation.waiters = append(existing.operation.waiters, op.waiters...)
					continue
				}
				ledger[key] = &ledgerEntry{
					payloadHash: op.options.PayloadHash,
					operation:   op,
					paneID:      op.options.PaneID,
					createdAt:   op.options.ReceivedAt,
				}
			}
			heap.Push(&deadlines, op)
			if op.options.RelayLevel {
				relayQueue = insertBySequence(relayQueue, op)
			} else {
				slot := slots[op.options.PaneID]
				if slot == nil {
					slot = &PaneSlot{}
					slots[op.options.PaneID] = slot
				}
				if op.options.PaneGeneration > slot.Generation {
					slot.Generation = op.options.PaneGeneration
					if slot.InFlight != nil && slot.InFlight.cancel != nil {
						slot.InFlight.cancel()
					}
					for _, queued := range slot.Queue {
						if queued.index >= 0 {
							heap.Remove(&deadlines, queued.index)
						}
						s.replyStale(queued, ledger)
					}
					slot.Queue = nil
					for key, entry := range ledger {
						if key != op.options.LedgerKey &&
							entry.paneID == op.options.PaneID &&
							entry.generation < slot.Generation {
							delete(ledger, key)
						}
					}
				}
				if entry := ledger[op.options.LedgerKey]; entry != nil {
					entry.generation = slot.Generation
				}
				op.generation = slot.Generation
				slot.Queue = insertBySequence(slot.Queue, op)
			}
			dispatch()

		case event := <-s.completions:
			completeAndRedispatch(event)

		case update := <-s.ledger:
			entry := ledger[update.key]
			applied := entry != nil && entry.generation == update.generation &&
				entry.result != nil && update.result != nil
			if applied {
				entry.result = cloneResult(update.result)
			}
			update.response <- applied

		case query := <-s.replays:
			entry := ledger[query.key]
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
			applyTopology(update)

		case <-timer.C:
			now := time.Now()
			for deadlines.Len() > 0 && !now.Before(deadlines[0].options.Deadline) {
				op := heap.Pop(&deadlines).(*scheduledOperation)
				if s.removeQueued(op, slots, &relayQueue) {
					s.expire(op, ledger)
				}
			}
			dispatch()

		case <-s.cancelInflight:
			if !stopping {
				stopping = true
				for _, slot := range slots {
					if slot.InFlight != nil && slot.InFlight.cancel != nil {
						slot.InFlight.cancel()
					}
					for _, op := range slot.Queue {
						s.reply(op, nil, ErrClosed)
					}
					slot.Queue = nil
				}
				for _, op := range relayInFlight {
					if op.cancel != nil {
						op.cancel()
					}
				}
				for _, op := range relayQueue {
					s.reply(op, nil, ErrClosed)
				}
				relayQueue = nil
				resetTimer()
			}

		case ack := <-s.stop:
			stopAck = ack
			if !stopping {
				stopping = true
				for _, slot := range slots {
					if slot.InFlight != nil && slot.InFlight.cancel != nil {
						slot.InFlight.cancel()
					}
					for _, op := range slot.Queue {
						s.reply(op, nil, ErrClosed)
					}
					slot.Queue = nil
				}
				for _, op := range relayInFlight {
					if op.cancel != nil {
						op.cancel()
					}
				}
				for _, op := range relayQueue {
					s.reply(op, nil, ErrClosed)
				}
				relayQueue = nil
			}
			resetTimer()
		}
	}
}

func (s *Scheduler) start(op *scheduledOperation, generation uint64) context.CancelFunc {
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

func (s *Scheduler) removeQueued(op *scheduledOperation, slots map[string]*PaneSlot, relayQueue *[]*scheduledOperation) bool {
	if op.options.RelayLevel {
		var removed bool
		*relayQueue, removed = removeOperation(*relayQueue, op)
		return removed
	}
	slot := slots[op.options.PaneID]
	if slot == nil || (slot.InFlight != nil && slot.InFlight.OperationID == op.operation) {
		return false
	}
	var removed bool
	slot.Queue, removed = removeOperation(slot.Queue, op)
	return removed
}

func (s *Scheduler) expire(op *scheduledOperation, ledger map[string]*ledgerEntry) {
	if op.options.LedgerKey != "" {
		delete(ledger, op.options.LedgerKey)
	}
	s.metricsMu.Lock()
	s.metrics.ExpiredQueued++
	s.metricsMu.Unlock()
	s.replyNotStarted(op)
}

func (s *Scheduler) replyNotStarted(op *scheduledOperation) {
	result := &CommandResult{
		RequestID: op.options.RequestID,
		Action:    string(op.options.Kind),
		OK:        false,
		Phase:     "not_started",
		Error:     "command was not sent; retry is safe",
		PaneID:    op.options.PaneID,
	}
	s.reply(op, result, nil)
}

func (s *Scheduler) replyStale(op *scheduledOperation, ledger map[string]*ledgerEntry) {
	if op.options.LedgerKey != "" {
		delete(ledger, op.options.LedgerKey)
	}
	phase := "failed"
	publicError := ErrPaneReplaced.Error()
	if op.dispatched {
		phase = "dispatched_unknown"
		publicError = "command was dispatched before the pane session was replaced; outcome is unknown"
	}
	result := &CommandResult{
		RequestID: op.options.RequestID,
		Action:    string(op.options.Kind),
		OK:        false,
		Phase:     phase,
		Error:     publicError,
		PaneID:    op.options.PaneID,
	}
	s.reply(op, result, nil)
}

func (s *Scheduler) reply(op *scheduledOperation, result *CommandResult, err error) {
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
