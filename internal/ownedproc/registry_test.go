package ownedproc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type fakeInspector struct {
	commands  map[int]string
	rows      map[int]Process
	signals   []string
	lookup    func(int)
	resistant bool
	tableErr  error
}

func (f *fakeInspector) Lookup(ctx context.Context, pid int) (Process, error) {
	if err := ctx.Err(); err != nil {
		return Process{}, err
	}
	if f.lookup != nil {
		f.lookup(pid)
	}
	p, ok := f.rows[pid]
	if !ok {
		return Process{}, ErrAbsent
	}
	return p, nil
}
func (f *fakeInspector) Table(ctx context.Context) ([]Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.tableErr != nil {
		return nil, f.tableErr
	}
	var rows []Process
	for _, p := range f.rows {
		rows = append(rows, p)
	}
	return rows, nil
}
func (f *fakeInspector) Signal(pid int, force bool) error {
	if pid <= 0 {
		panic("group signal")
	}
	f.signals = append(f.signals, fmt.Sprintf("%d:%t", pid, force))
	if force || !f.resistant {
		delete(f.rows, pid)
	}
	return nil
}
func fixture(t *testing.T) (*Registry, *fakeInspector) {
	t.Helper()
	f := &fakeInspector{rows: map[int]Process{1: {PID: 1, Birth: "p"}, 2: {PID: 2, Parent: 1, Birth: "r"}, 3: {PID: 3, Parent: 2, Birth: "c"}}}
	r, err := New(context.Background(), f, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r, f
}
func TestRegistryRefusesChangedOwnership(t *testing.T) {
	for _, name := range []string{"birth reuse", "parent reuse", "missing birth", "reparenting", "leader exit", "missing inspection", "unsupported inspection"} {
		t.Run(name, func(t *testing.T) {
			r, f := fixture(t)
			switch name {
			case "birth reuse":
				p := f.rows[3]
				p.Birth = "replacement"
				f.rows[3] = p
			case "parent reuse":
				p := f.rows[2]
				p.Birth = "replacement"
				f.rows[2] = p
			case "missing birth":
				p := f.rows[3]
				p.Birth = ""
				f.rows[3] = p
			case "reparenting":
				p := f.rows[3]
				p.Parent = 1
				f.rows[3] = p
			case "leader exit":
				delete(f.rows, 2)
			case "missing inspection":
				f.lookup = func(pid int) {
					if pid == 3 {
						p := f.rows[pid]
						p.Birth = ""
						f.rows[pid] = p
					}
				}
			case "unsupported inspection":
				f.tableErr = errors.ErrUnsupported
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := r.Cancel(ctx, 0); !errors.Is(err, ErrIncomplete) {
				t.Fatalf("expected incomplete: %v", err)
			}
			if len(f.signals) != 0 {
				t.Fatalf("unsafe signals: %v", f.signals)
			}
		})
	}
}
func TestRegistrySnapshotBirthRetiresAfterNativeAbsence(t *testing.T) {
	r, f := fixture(t)
	f.rows[4] = Process{PID: 4, Parent: 2, Birth: "short-lived"}
	f.lookup = func(pid int) {
		if pid == 4 {
			delete(f.rows, 4)
		}
	}
	if err := r.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Identity(4); !ok || p.Birth != "short-lived" {
		t.Fatal(p, ok)
	}
	if err := r.Signal(context.Background(), 4, true); err != nil {
		t.Fatal(err)
	}
	if len(f.signals) != 0 {
		t.Fatal(f.signals)
	}
	f.lookup = nil
	f.rows[4] = Process{PID: 4, Parent: 2, Birth: "replacement"}
	if err := r.Sample(context.Background()); !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	if len(f.signals) != 0 {
		t.Fatal(f.signals)
	}
}

func TestRegistryDepthLimit(t *testing.T) {
	r, f := fixture(t)
	for i := 4; i < MaxDepth+6; i++ {
		f.rows[i] = Process{PID: i, Parent: i - 1, Birth: fmt.Sprint(i)}
	}
	if err := r.Sample(context.Background()); !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	if len(f.signals) != 0 {
		t.Fatal(f.signals)
	}
}

func TestRegistryImmediateRecheck(t *testing.T) {
	r, f := fixture(t)
	count := 0
	f.lookup = func(pid int) {
		if pid == 3 {
			count++
			if count == 2 {
				p := f.rows[3]
				p.Birth = "reused"
				f.rows[3] = p
			}
		}
	}
	if err := r.Signal(context.Background(), 3, true); !errors.Is(err, ErrIncomplete) {
		t.Fatal(err)
	}
	if len(f.signals) != 0 {
		t.Fatal(f.signals)
	}
}
func TestRegistryExecPreservesBirth(t *testing.T) {
	r, f := fixture(t)
	f.commands = map[int]string{3: "original command"}
	f.commands[3] = "exec replacement with another command name"
	f.rows[3] = Process{PID: 3, Parent: 2, Birth: "c"}
	if err := r.Signal(context.Background(), 3, false); err != nil {
		t.Fatal(err)
	}
	if len(f.signals) != 1 || f.signals[0] != "3:false" {
		t.Fatal(f.signals)
	}
}

func TestRegistryExecAndChildFirstEscalation(t *testing.T) {
	r, f := fixture(t)
	f.resistant = true
	if err := r.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Cancel(ctx, 0); err != nil {
		t.Fatal(err)
	}
	want := []string{"3:false", "2:false", "3:true", "2:true"}
	if !reflect.DeepEqual(f.signals, want) {
		t.Fatalf("%v != %v", f.signals, want)
	}
	if err := r.Finish(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestRegistryLimitsTimeoutAndNoGroupFallback(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		r, f := fixture(t)
		for i := 10; i < MaxTable+20; i++ {
			f.rows[i] = Process{PID: i, Birth: "x"}
		}
		if err := r.Sample(context.Background()); !errors.Is(err, ErrIncomplete) {
			t.Fatal(err)
		}
		if len(f.signals) != 0 {
			t.Fatal(f.signals)
		}
	})
	t.Run("descendants", func(t *testing.T) {
		r, f := fixture(t)
		for i := 10; i < MaxProcesses+20; i++ {
			f.rows[i] = Process{PID: i, Parent: 2, Birth: "x"}
		}
		if err := r.Sample(context.Background()); !errors.Is(err, ErrIncomplete) {
			t.Fatal(err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		r, f := fixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := r.Cancel(ctx, 0); !errors.Is(err, ErrIncomplete) {
			t.Fatal(err)
		}
		if len(f.signals) != 0 {
			t.Fatal(f.signals)
		}
	})
	t.Run("negative", func(t *testing.T) {
		r, f := fixture(t)
		if err := r.Signal(context.Background(), -2, true); !errors.Is(err, ErrIncomplete) {
			t.Fatal(err)
		}
		if len(f.signals) != 0 {
			t.Fatal(f.signals)
		}
	})
}
func TestRegistryCompletionAndInheritedHolder(t *testing.T) {
	t.Run("both exited", func(t *testing.T) {
		r, f := fixture(t)
		delete(f.rows, 3)
		delete(f.rows, 2)
		if err := r.Finish(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.signals) != 0 {
			t.Fatal(f.signals)
		}
	})
	t.Run("inherited holder escaped", func(t *testing.T) {
		r, f := fixture(t)
		delete(f.rows, 2)
		p := f.rows[3]
		p.Parent = 1
		f.rows[3] = p
		if err := r.Finish(context.Background()); !errors.Is(err, ErrIncomplete) {
			t.Fatal(err)
		}
		if len(f.signals) != 0 {
			t.Fatal(f.signals)
		}
	})
	t.Run("pid retired", func(t *testing.T) {
		r, f := fixture(t)
		delete(f.rows, 3)
		if err := r.Sample(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.rows[3] = Process{PID: 3, Parent: 2, Birth: "replacement"}
		if err := r.Signal(context.Background(), 3, true); err != nil {
			t.Fatal(err)
		}
		if len(f.signals) != 0 {
			t.Fatal(f.signals)
		}
	})
}
