package panesize

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type runnerCall struct {
	name string
	args []string
}

type readRunner struct {
	calls []runnerCall
	tty   string
	size  string
	work  func(context.Context, string) error
}

func (r *readRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, runnerCall{name: name, args: append([]string(nil), args...)})
	if r.work != nil {
		if err := r.work(ctx, name); err != nil {
			return nil, err
		}
	}
	if name == "ps" {
		return []byte(r.tty), nil
	}
	return []byte(r.size), nil
}

func TestReadGridPlatformAndFreshness(t *testing.T) {
	for _, platform := range []struct{ name, tty, flag, path string }{
		{"darwin", "ttys012\n", "-f", "/dev/ttys012"},
		{"linux", "pts/7\n", "-F", "/dev/pts/7"},
	} {
		t.Run(platform.name, func(t *testing.T) {
			runner := &readRunner{tty: platform.tty, size: "30 80\n"}
			reader := NewReader(runner, platform.name)
			grid, err := reader.Read(context.Background(), 321)
			if err != nil || grid != (Grid{TTY: platform.path, Rows: 30, Columns: 80}) {
				t.Fatalf("grid = %+v, %v", grid, err)
			}
			runner.size = "25 41\n"
			grid, err = reader.Read(context.Background(), 321)
			if err != nil || grid.Rows != 25 || grid.Columns != 41 {
				t.Fatalf("cached old geometry: %+v, %v", grid, err)
			}
			want := []runnerCall{{name: "ps", args: []string{"-o", "tty=", "-p", "321"}}, {name: "stty", args: []string{platform.flag, platform.path, "size"}}}
			if !reflect.DeepEqual(runner.calls[:2], want) || !reflect.DeepEqual(runner.calls[2:], want) {
				t.Fatalf("expected two read-only subprocesses each time, got %+v", runner.calls)
			}
		})
	}
}

func TestReadGridRejectsTTY(t *testing.T) {
	for _, tty := range []string{"", "?", "??", "-", "pts/7 pts/8", "../tmp/tty", "/tmp/tty", "/dev/../tmp/tty", "pts/../tty", "pts//7", "pts/7/", "pts/7\x00", "pts/7;cmd", "pts/$x", ".", ".."} {
		t.Run(tty, func(t *testing.T) {
			runner := &readRunner{tty: tty, size: "30 80"}
			if _, err := NewReader(runner, "linux").Read(context.Background(), 321); !errors.Is(err, ErrTTYUnavailable) {
				t.Fatalf("accepted TTY %q: %v", tty, err)
			}
			if len(runner.calls) != 1 {
				t.Fatal("invalid TTY reached stty")
			}
		})
	}
}

func TestReadGridRejectsSize(t *testing.T) {
	for _, size := range []string{"", "30", "30 80 1", "0 80", "30 0", "-1 80", "+30 80", "30 1.5", "65536 80", "30 65536", "999999999999999999999 80", "30 80\x00"} {
		t.Run(size, func(t *testing.T) {
			runner := &readRunner{tty: "pts/7", size: size}
			if _, err := NewReader(runner, "linux").Read(context.Background(), 321); !errors.Is(err, ErrSizeUnavailable) {
				t.Fatalf("accepted size %q: %v", size, err)
			}
		})
	}
	for _, size := range []string{"1 1", "65535 65535", " 30\t80\n"} {
		runner := &readRunner{tty: "pts/7", size: size}
		if _, err := NewReader(runner, "linux").Read(context.Background(), 321); err != nil {
			t.Fatalf("valid size %q: %v", size, err)
		}
	}
}

func TestReadGridDeadlineAndErrors(t *testing.T) {
	for _, slow := range []string{"ps", "stty"} {
		t.Run(slow, func(t *testing.T) {
			runner := &readRunner{tty: "pts/7", size: "30 80", work: func(ctx context.Context, name string) error {
				if name == slow {
					<-ctx.Done()
					return nil
				}
				return nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if _, err := NewReader(runner, "linux").Read(ctx, 321); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("late geometry accepted: %v", err)
			}
			if slow == "ps" && len(runner.calls) != 1 {
				t.Fatal("continued after ps admission expiry")
			}
		})
	}
	for _, failed := range []string{"ps", "stty"} {
		failure := errors.New("fixture failure")
		runner := &readRunner{tty: "pts/7", size: "30 80", work: func(_ context.Context, name string) error {
			if name == failed {
				return failure
			}
			return nil
		}}
		if _, err := NewReader(runner, "linux").Read(context.Background(), 321); !errors.Is(err, failure) {
			t.Fatalf("lost subprocess failure: %v", err)
		}
	}
	runner := &readRunner{}
	if _, err := NewReader(runner, "windows").Read(context.Background(), 321); !errors.Is(err, ErrUnsupportedOS) || len(runner.calls) != 0 {
		t.Fatalf("unsupported OS: %v, calls = %v", err, runner.calls)
	}
	for _, pid := range []int{0, -1} {
		if _, err := NewReader(runner, "linux").Read(context.Background(), pid); !errors.Is(err, ErrProcessUnavailable) || len(runner.calls) != 0 {
			t.Fatalf("invalid PID: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewReader(runner, "linux").Read(ctx, 321); !errors.Is(err, context.Canceled) || len(runner.calls) != 0 {
		t.Fatalf("cancelled geometry read: %v", err)
	}
}
