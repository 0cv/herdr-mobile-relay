package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

var errFilterState = errors.New("unsupported or changed Cursor filter; inspect the picker")

func cursorFilterValue(screen string) (string, error) {
	rows := strings.Split(strings.TrimSpace(screen), "\n")
	footer := ""
	end := len(rows)
	for end > 0 && len(footer) < 160 {
		end--
		footer = strings.TrimSpace(rows[end]) + " " + footer
		if strings.HasPrefix(footer, "Type to filter") {
			break
		}
	}
	if strings.Join(strings.Fields(footer), " ") != "Type to filter • Enter to select • Tab to edit" {
		return "", errFilterState
	}
	field := -1
	for i := end - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(rows[i], " "), "Filter:") {
			field = i
			break
		}
	}
	if field < 1 || field+1 >= end || strings.TrimSpace(rows[field+1]) != "" {
		return "", errFilterState
	}
	header := strings.Join(rows[:field], " ")
	if !strings.Contains(header, "Max mode:") || (!strings.Contains(header, "Available models") && !strings.Contains(header, "Models")) {
		return "", errFilterState
	}
	value := strings.TrimPrefix(strings.TrimLeft(rows[field], " "), "Filter:")
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}
	return strings.TrimRight(value, " \r"), nil
}

func (d *Dispatcher) handleFilterText(ctx context.Context, receivedAt time.Time, requestID, paneID string, message map[string]any) *CommandResult {
	const action = "send_filter_text"
	text := stringValue(message, "text")
	if err := herdr.ValidateFilterText(text); err != nil || paneID == "" {
		return d.fail(requestID, action, paneID, "Filter text requires an agent and 1–32 ASCII letters, digits, spaces or .-_/+:()[]; no trailing space")
	}
	var gate sync.Mutex
	dispatched := false
	stopped := false
	result := d.schedule(ctx, ScheduleOptions{Command: d.command(ctx, receivedAt, requestID, CommandFilterText, paneID, commandDeadline, text)}, EffectFunc(func(effectCtx context.Context, token WorkerToken) EffectResult {
		run := func() error {
			check := func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := effectCtx.Err(); err != nil {
					return err
				}
				return d.paneSessionError(token)
			}
			read := func() (string, error) {
				if err := check(); err != nil {
					return "", err
				}
				pane, err := d.herdr.ReadPaneVisible(effectCtx, paneID, 200, "text")
				if err != nil {
					return "", err
				}
				if pane.Truncated {
					return "", errFilterState
				}
				return cursorFilterValue(string(pane.Content))
			}
			wait := func(duration time.Duration) error {
				if d.testFilterWait != nil {
					if err := d.testFilterWait(effectCtx, duration); err != nil {
						return err
					}
					return check()
				}
				timer := time.NewTimer(duration)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-effectCtx.Done():
					return effectCtx.Err()
				case <-timer.C:
					return check()
				}
			}
			if deadline, ok := effectCtx.Deadline(); !ok || time.Until(deadline) < time.Duration(len(text))*300*time.Millisecond+time.Second {
				return errors.New("insufficient time remaining for filter delivery")
			}
			expected, err := read()
			if err != nil {
				return err
			}
			visible := expected
			for _, character := range text {
				current, err := read()
				if err != nil {
					return err
				}
				if current != visible {
					return errFilterState
				}
				gate.Lock()
				if stopped {
					gate.Unlock()
					return context.Canceled
				}
				if err := check(); err != nil {
					gate.Unlock()
					return err
				}
				alreadyDispatched := dispatched
				dispatched = true
				gate.Unlock()
				if err := d.herdr.SendLiteralKey(effectCtx, paneID, string(character)); err != nil {
					if !alreadyDispatched && errors.Is(err, herdr.ErrNotStarted) && !errors.Is(err, herdr.ErrDispatchedUnknown) {
						gate.Lock()
						dispatched = false
						gate.Unlock()
					}
					return err
				}
				expected += string(character)
				if err := wait(150 * time.Millisecond); err != nil {
					return err
				}
				if character == ' ' {
					continue
				}
				until := time.Now().Add(150 * time.Millisecond)
				for {
					current, err = read()
					if err != nil {
						return err
					}
					if current == expected {
						visible = current
						break
					}
					if current != visible || !time.Now().Before(until) {
						return errFilterState
					}
					if err := wait(25 * time.Millisecond); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := run(); err != nil {
			gate.Lock()
			if dispatched {
				err = partiallyApplied("filter delivery was attempted", err)
			} else {
				err = errors.Join(herdr.ErrNotStarted, err)
			}
			gate.Unlock()
			return EffectResult{Result: d.failErr(requestID, action, paneID, err)}
		}
		return EffectResult{Result: completed(requestID, action, paneID, nil)}
	}))
	gate.Lock()
	stopped = true
	uncertain := dispatched && !result.OK
	gate.Unlock()
	if uncertain {
		result.Phase = "dispatched_unknown"
		result.Error = "Filter text may be partially delivered; inspect the picker before sending more"
		result.Data = map[string]any{"dispatched_unknown": true}
	}
	if result.OK {
		d.recordActivity(action, "sent", "Filter text verified; select separately", paneID, requestID)
	}
	d.wake()
	return result
}
