package conversation

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

func TestInstalledSourceAdmission(t *testing.T) {
	for _, name := range []string{"nil", "zero", "nil-owner", "zero-owner", "foreign", "stale", "missing-open", "missing-eval", "missing-stat", "missing-same", "fenced-child"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			foreign := conversationlife.New()
			t.Cleanup(func() { owner.Begin(); foreign.Begin(); owner.Join(); foreign.Join() })
			f := newSourceFixture()
			d := f.deps()
			var stale *conversationlife.Call
			_ = owner.Do(context.Background(), func(c *conversationlife.Call) error { stale = c; return nil })
			err := owner.Do(context.Background(), func(c *conversationlife.Call) error {
				var e error
				switch name {
				case "nil":
					_, e = d.openIn(owner, nil, sourceFixtureLocation)
				case "zero":
					_, e = d.openIn(owner, &conversationlife.Call{}, sourceFixtureLocation)
				case "nil-owner":
					_, e = d.openIn(nil, c, sourceFixtureLocation)
				case "zero-owner":
					_, e = d.openIn(&conversationlife.Owner{}, c, sourceFixtureLocation)
				case "foreign":
					_, e = d.openIn(foreign, c, sourceFixtureLocation)
				case "stale":
					_, e = d.openIn(owner, stale, sourceFixtureLocation)
				default:
					switch name {
					case "missing-open":
						d.open = nil
					case "missing-eval":
						d.eval = nil
					case "missing-stat":
						d.stat = nil
					case "missing-same":
						d.same = nil
					case "fenced-child":
						owner.Begin()
					}
					_, e = d.openIn(owner, c, sourceFixtureLocation)
				}
				return e
			})
			if err == nil || len(f.calls) != 0 || f.unplanned != 0 || owner.Witnesses() != nil {
				t.Fatalf("denial: %v, calls %v", err, f.calls)
			}
		})
	}
}

func TestInstalledSourceFailures(t *testing.T) {
	for _, name := range []string{"initial-eval", "initial-stat", "nil-info", "nonregular", "nil-handle", "handle-error", "opened-stat", "post-eval", "post-contained", "post-stat", "same", "reserve", "anchor", "observe", "content", "close", "primary-close"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			t.Cleanup(owner.Join)
			f := newSourceFixture()
			var reserve, observe func(int64) error
			wantClose := 1
			switch name {
			case "initial-eval":
				f.fail = "eval"
				f.failAt = 1
				wantClose = 0
			case "initial-stat":
				f.fail = "path-stat"
				f.failAt = 1
				wantClose = 0
			case "nil-info":
				f.nilInfo = true
				wantClose = 0
			case "nonregular":
				f.mode = os.ModeDir
				wantClose = 0
			case "nil-handle":
				f.nilHandle = true
				wantClose = 0
			case "handle-error":
				f.openErr = f.fault
			case "opened-stat":
				f.fail = "stat"
			case "post-eval":
				f.fail = "eval"
				f.failAt = 3
			case "post-contained":
				f.fail = "eval"
				f.failAt = 4
			case "post-stat":
				f.fail = "path-stat"
				f.failAt = 3
			case "same":
				f.fail = "same"
			case "reserve":
				reserve = func(int64) error { return f.fault }
			case "anchor", "content":
				f.fail = "read"
			case "observe":
				observe = func(int64) error { return f.fault }
			case "close":
				f.closeErr = f.fault
			case "primary-close":
				f.fail = "read"
				f.closeErr = errors.New("private cleanup operand")
			}
			err := owner.Do(context.Background(), func(c *conversationlife.Call) error {
				if name == "content" || name == "close" || name == "primary-close" {
					data, _, e := f.deps().readContentIn(owner, c, sourceFixtureLocation, 100, false)
					if e != nil && data != nil {
						return errors.New("failed read published data")
					}
					return e
				}
				s, e := f.deps().captureIn(owner, c, sourceFixtureLocation, reserve, observe)
				if e == nil {
					return s.close()
				}
				return e
			})
			if err == nil || f.calls["close"] != wantClose || f.unplanned != 0 {
				t.Fatalf("failure %v, calls %v", err, f.calls)
			}
			if f.closeErr != nil {
				if !errors.Is(err, ErrSourceCleanupUncertain) || !errors.Is(owner.Witnesses(), f.closeErr) || errors.Is(err, f.closeErr) {
					t.Fatal("cleanup redaction/ledger")
				}
				if name == "primary-close" && !errors.Is(err, f.fault) {
					t.Fatal("primary lost")
				}
			} else if owner.Witnesses() != nil {
				t.Fatal("request error poisoned ledger")
			}
		})
	}
}

func TestInstalledSourceRevisionAndCopies(t *testing.T) {
	owner := conversationlife.New()
	t.Cleanup(owner.Join)
	f := newSourceFixture()
	err := owner.Do(context.Background(), func(c *conversationlife.Call) error {
		s, err := f.deps().openIn(owner, c, sourceFixtureLocation)
		if err != nil {
			return err
		}
		if f.calls["read"] != 0 {
			return errors.New("metadata read anchor")
		}
		if err := s.close(); err != nil {
			return err
		}
		var reserved, observed int64
		s, err = f.deps().captureIn(owner, c, sourceFixtureLocation, func(n int64) error { reserved += n; return nil }, func(n int64) error { observed += n; return nil })
		if err != nil {
			return err
		}
		defer s.close()
		ordinary, err := fileRevisionWithReservation(bytes.NewReader(f.data), f.info(), nil, nil)
		if err != nil || ordinary != s.revision || reserved != int64(len(f.data)) || observed != reserved {
			return errors.New("revision mismatch")
		}
		copy := s
		if err := copy.borrowIn(owner, c, func(r io.ReaderAt) error { _, e := r.ReadAt(make([]byte, 1), 0); return e }); err != nil {
			return err
		}
		if f.calls["close"] != 1 {
			return errors.New("borrow closed")
		}
		f.closeErr = f.fault
		if !errors.Is(s.close(), ErrSourceCleanupUncertain) || !errors.Is(copy.close(), ErrSourceCleanupUncertain) {
			return errors.New("cached close missing")
		}
		if f.calls["close"] != 2 {
			return errors.New("duplicate physical close")
		}
		if err := copy.borrowIn(owner, c, func(io.ReaderAt) error { return nil }); !errors.Is(err, conversationlife.ErrRefused) {
			return errors.New("closed borrow admitted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInstalledSourceBoundaries(t *testing.T) {
	for _, name := range []string{"empty-location", "outside", "retarget", "opened-nil", "opened-directory", "post-nil", "anchor-eof", "anchor-bound", "tail", "oversize", "content-short", "invalid-range", "stale-borrow", "abnormal-stat"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			t.Cleanup(owner.Join)
			f := newSourceFixture()
			d := f.deps()
			location := sourceFixtureLocation
			switch name {
			case "empty-location":
				location = Location{}
			case "outside", "retarget":
				original := d.eval
				d.eval = func(path string) (string, error) {
					resolved, err := original(path)
					if name == "outside" && f.calls["eval"] == 2 {
						return "/elsewhere/source", nil
					}
					if name == "retarget" && f.calls["eval"] == 3 {
						return "/planned/replaced", nil
					}
					return resolved, err
				}
			case "opened-nil", "opened-directory":
				f.after = func(stage string) {
					if stage == "open" {
						if name == "opened-nil" {
							f.nilInfo = true
						} else {
							f.mode = os.ModeDir
						}
					}
				}
			case "post-nil":
				f.after = func(stage string) {
					if stage == "stat" {
						f.nilInfo = true
					}
				}
			case "anchor-eof", "content-short":
				f.after = func(stage string) {
					if stage == "same" {
						f.data = []byte("short\n")
					}
				}
			case "anchor-bound":
				f.data = bytes.Repeat([]byte("x"), 128*1024)
			case "abnormal-stat":
				f.after = func(stage string) {
					if stage == "stat" {
						panic("guarded metadata")
					}
				}
			}
			var recovered any
			var stale *conversationlife.Call
			_ = owner.Do(context.Background(), func(c *conversationlife.Call) error { stale = c; return nil })
			var err error
			func() {
				defer func() { recovered = recover() }()
				err = owner.Do(context.Background(), func(c *conversationlife.Call) error {
					if name == "tail" || name == "oversize" || name == "content-short" {
						data, clipped, e := d.readContentIn(owner, c, location, func() int64 {
							if name == "content-short" {
								return 100
							}
							return 8
						}(), name == "tail")
						if name == "tail" && (string(data) != "second\n" || !clipped) {
							return errors.New("tail semantics")
						}
						if e != nil && data != nil {
							return errors.New("partial data published")
						}
						return e
					}
					var reserved int64
					s, e := d.captureIn(owner, c, location, func(n int64) error { reserved = n; return nil }, nil)
					if e != nil {
						return e
					}
					defer s.close()
					switch name {
					case "anchor-bound":
						if reserved != 64*1024 {
							return errors.New("unbounded anchor")
						}
					case "anchor-eof":
						ordinary, e := fileRevisionWithReservation(bytes.NewReader(f.data), s.info, nil, nil)
						if e != nil || ordinary != s.revision {
							return errors.New("EOF semantics")
						}
					case "invalid-range":
						_, e = s.readRangeIn(owner, c, -1, s.end, 100)
						return e
					case "stale-borrow":
						return s.borrowIn(owner, stale, func(io.ReaderAt) error { return nil })
					}
					return nil
				})
			}()
			wantSuccess := name == "tail" || name == "anchor-eof" || name == "anchor-bound"
			if name == "abnormal-stat" {
				if recovered == nil {
					t.Fatal("abnormal unwind not observed")
				}
			} else if (err == nil) != wantSuccess {
				t.Fatalf("result: %v", err)
			}
			if f.calls["open"] != f.calls["close"] || f.unplanned != 0 || owner.Witnesses() != nil {
				t.Fatalf("boundary cleanup: %v", f.calls)
			}
		})
	}
}

func TestInstalledSourceWitnessOverflow(t *testing.T) {
	owner := conversationlife.New()
	t.Cleanup(owner.Join)
	shared := errors.New("equal sibling cleanup")
	err := owner.Do(context.Background(), func(c *conversationlife.Call) error {
		for i := 0; i < 19; i++ {
			f := newSourceFixture()
			f.closeErr = shared
			s, e := f.deps().openIn(owner, c, sourceFixtureLocation)
			if e != nil {
				return e
			}
			_ = s.close()
			_ = s.close()
			if f.calls["close"] != 1 {
				return errors.New("sibling close count")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	witness := owner.Witnesses()
	var overflow *conversationlife.Overflow
	if !errors.As(witness, &overflow) || overflow.Count != 3 {
		t.Fatalf("overflow: %v", witness)
	}
	joined, ok := witness.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 17 {
		t.Fatal("independent witnesses collapsed")
	}
}

func TestInstalledSourceRawTransfer(t *testing.T) {
	for _, name := range []string{"success", "no-raw", "cancel-raw", "open-error", "nil-adapt", "refuse", "error", "handle-error", "panic", "goexit"} {
		t.Run(name, func(t *testing.T) {
			owner := conversationlife.New()
			f := newSourceFixture()
			f.closeErr = f.fault
			rawCloses := 0
			done := make(chan struct{})
			var result error
			var recovered any
			t.Cleanup(func() { owner.Begin(); <-done; owner.Join() })
			go func() {
				defer close(done)
				defer func() { recovered = recover() }()
				result = owner.Do(context.Background(), func(c *conversationlife.Call) error {
					return owner.DoIn(c, func(*conversationlife.Call) error {
						s, err := acquireSourceRaw(c.Context(), owner, nil, func() (func() error, func() (sourceHandle, error), error) {
							if name == "no-raw" {
								return nil, nil, f.fault
							}
							if name == "cancel-raw" {
								owner.Begin()
							}
							closeRaw := func() error { rawCloses++; return f.fault }
							adapt := func() (sourceHandle, error) {
								switch name {
								case "refuse":
									return nil, nil
								case "error":
									return nil, f.fault
								case "handle-error":
									return f, f.fault
								case "panic":
									panic("guarded logical adaptation")
								case "goexit":
									runtime.Goexit()
								}
								return f, nil
							}
							if name == "nil-adapt" {
								adapt = nil
							}
							if name == "open-error" {
								return closeRaw, adapt, f.fault
							}
							return closeRaw, adapt, nil
						})
						if s != nil {
							return s.close()
						}
						return err
					})
				})
			}()
			<-done
			owner.Join()
			if name == "no-raw" {
				if rawCloses != 0 || f.calls["close"] != 0 || owner.Witnesses() != nil || !errors.Is(result, f.fault) {
					t.Fatal("raw refusal effects")
				}
				return
			}
			if name == "cancel-raw" && !errors.Is(result, context.Canceled) {
				t.Fatal("raw cancellation lost")
			}
			if name == "success" || name == "handle-error" {
				if rawCloses != 0 || f.calls["close"] != 1 {
					t.Fatal("duplicate authority after transfer")
				}
			} else if rawCloses != 1 || f.calls["close"] != 0 {
				t.Fatal("raw unwind missing")
			}
			if name == "panic" && recovered == nil {
				t.Fatal("panic not observed")
			}
			if name != "panic" && name != "goexit" && !errors.Is(result, ErrSourceCleanupUncertain) {
				t.Fatalf("uncertainty lost: %v", result)
			}
			if !errors.Is(owner.Witnesses(), f.fault) {
				t.Fatal("raw witness lost")
			}
		})
	}
}

func TestInstalledSourceCancellation(t *testing.T) {
	for _, stage := range []string{"eval", "path-stat", "open", "stat", "same", "read", "close", "second-chunk"} {
		t.Run(stage, func(t *testing.T) {
			owner := conversationlife.New()
			t.Cleanup(owner.Join)
			f := newSourceFixture()
			f.data = bytes.Repeat([]byte("x"), 3*64*1024)
			f.after = func(name string) {
				if name == stage || stage == "second-chunk" && name == "read" && f.calls[name] == 2 {
					owner.Begin()
				}
			}
			err := owner.Do(context.Background(), func(c *conversationlife.Call) error {
				data, _, err := f.deps().readContentIn(owner, c, sourceFixtureLocation, int64(len(f.data)), false)
				if data != nil {
					return errors.New("cancelled data published")
				}
				return err
			})
			if !errors.Is(err, context.Canceled) || owner.Witnesses() != nil || f.unplanned != 0 {
				t.Fatalf("cancellation: %v", err)
			}
			if f.calls["open"] != f.calls["close"] {
				t.Fatal("cancelled acquisition leaked")
			}
			if stage == "second-chunk" && f.calls["read"] != 2 {
				t.Fatal("continued chunk work")
			}
		})
	}
}

// Actual work holds the admitted caller. Begin's receipt is the real operation
// count boundary, NOT an observation inside Join. The timed window is only a
// bounded nonreturn sample; cleanup always releases and joins, even on failure.
func TestInstalledSourceHeldLifetime(t *testing.T) {
	for _, stage := range []string{"open", "read", "close"} {
		t.Run(stage, func(t *testing.T) {
			owner := conversationlife.New()
			f := newSourceFixture()
			held := make(chan struct{})
			release := make(chan struct{})
			startJoin := make(chan struct{})
			done := make(chan struct{})
			joined := make(chan struct{})
			var once, joinOnce sync.Once
			var notices int
			var result error
			f.after = func(name string) {
				if name == stage {
					notices++
					if notices == 1 {
						close(held)
					}
					<-release
				}
			}
			// Both launches are sealed here; cleanup exists before either launch.
			t.Cleanup(func() {
				owner.Begin()
				once.Do(func() { close(release) })
				joinOnce.Do(func() { close(startJoin) })
				<-done
				<-joined
				owner.Join()
			})
			go func() {
				defer close(done)
				result = owner.Do(context.Background(), func(c *conversationlife.Call) error {
					_, _, err := f.deps().readContentIn(owner, c, sourceFixtureLocation, 100, false)
					return err
				})
			}()
			go func() { defer close(joined); <-startJoin; owner.Join() }()
			select {
			case <-held:
				joinOnce.Do(func() { close(startJoin) })
			case <-done:
				t.Fatal("caller exited before held work entry")
			}
			receipt := owner.Begin()
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-receipt:
				timer.Stop()
				t.Fatal("fenced count completed during held work")
			case <-done:
				timer.Stop()
				t.Fatal("caller returned during held work")
			case <-joined:
				timer.Stop()
				t.Fatal("Join returned during held work")
			case <-timer.C:
			}
			once.Do(func() { close(release) })
			<-done
			<-joined
			<-receipt
			if !errors.Is(result, context.Canceled) || notices != 1 || f.calls["close"] != 1 || f.unplanned != 0 {
				t.Fatalf("held lifecycle: %v, %v", result, f.calls)
			}
		})
	}
}
