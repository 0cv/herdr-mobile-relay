package sqliteexec

import (
	"context"
	"errors"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// Rows recording really precedes the DB-close holder. This is an accounting
// and unwind proof, not a Join-wait proof or escape from a live-gate-held Join:
// operation retirement still needs the admission gate after DB Close returns.
func TestA1InstalledWitnessBeforeHeldClose(t *testing.T) {
	owner := conversationlife.New()
	deps := deniedInstalledDeps()
	rowErr, dbErr := errors.New("rows witness"), errors.New("db witness")
	rows := &logicalRows{closeErr: rowErr}
	db := &logicalDB{rows: rows, closeErr: dbErr}
	f := newInstalledFixture(t, owner)
	hold, release := f.hold()
	entered := newInstalledCapture[struct{}]()
	var data []byte
	var result error
	db.closeHook = func() { entered.notify(struct{}{}); <-hold }
	deps.open = func(string) (installedDB, error) { return db, nil }
	e, err := resolveInstalled(context.Background(), owner, BackendNative, "denied", deps)
	if err != nil {
		t.Fatal(err)
	}
	returned := f.launch(func() {
		data, result = e.QueryJSON(context.Background(), "/logical/db", "logical", 32)
	})
	f.require(t, entered.ch, "held DB Close entry")
	var staged error
	probe := f.launch(func() { staged = owner.Witnesses() })
	f.require(t, probe, "pre-release witness snapshot")
	receipt := f.fence(t, owner)
	countHeld := installedHeld(receipt)
	queryHeld := installedHeld(returned)
	release()
	f.require(t, returned, "DB Close and query return")
	f.require(t, receipt, "close operation-zero completion")
	f.finish()
	if !errors.Is(staged, rowErr) || errors.Is(staged, dbErr) {
		t.Error("rows witness not independently staged before DB release", staged)
	}
	if entered.count.Load() != 1 {
		t.Error("unexpected DB Close entries", entered.count.Load())
	}
	if !countHeld || !queryHeld {
		t.Error("actual operation receipt completed while resource held")
	}
	if data != nil || !errors.Is(result, ErrCleanupUncertain) || errors.Is(result, rowErr) || errors.Is(result, dbErr) {
		t.Error("public cleanup projection", result)
	}
	ledger := owner.Witnesses()
	if !errors.Is(ledger, rowErr) || !errors.Is(ledger, dbErr) || db.closes != 1 || rows.closes != 1 {
		t.Error("independent close accounting")
	}
	joined, ok := ledger.(interface{ Unwrap() []error })
	if !ok {
		t.Error("missing raw witness siblings")
	} else {
		siblings := joined.Unwrap()
		if len(siblings) != 2 || siblings[0] != rowErr || siblings[1] != dbErr {
			t.Error("raw identities must occur once in close order", siblings)
		}
	}
	if result != ErrCleanupUncertain {
		// errors.Join may wrap the sole marker, but must have no other leaf.
		public, ok := result.(interface{ Unwrap() []error })
		if !ok || len(public.Unwrap()) != 1 || public.Unwrap()[0] != ErrCleanupUncertain {
			t.Error("unexpected primary beside cleanup marker", result)
		}
	}
}

func TestA1InstalledJSONCompatibility(t *testing.T) {
	cases := []struct {
		name    string
		columns []string
		values  [][]any
		want    string
	}{
		{"types", []string{"a", "b", "c", "d"}, [][]any{{nil, int64(12), true, []byte("<&>\n")}}, `[{"a":null,"b":12,"c":true,"d":"<&>\n"}]`},
		{"multiple", []string{"x"}, [][]any{{"one"}, {"two"}}, `[{"x":"one"},{"x":"two"}]`},
		{"duplicate_column", []string{"x", "x"}, [][]any{{"first", "last"}}, `[{"x":"last"}]`},
		{"zero_columns", nil, [][]any{{}}, `[{}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := conversationlife.New()
			t.Cleanup(owner.Join)
			deps := deniedInstalledDeps()
			rows := &logicalRows{columns: tc.columns, values: tc.values}
			db := &logicalDB{rows: rows}
			deps.open = func(string) (installedDB, error) { return db, nil }
			e, err := resolveInstalled(context.Background(), owner, BackendNative, "denied", deps)
			if err != nil {
				t.Fatal(err)
			}
			data, err := e.QueryJSON(context.Background(), "/logical/db", "logical", len(tc.want))
			if err != nil || string(data) != tc.want || db.closes != 1 || rows.closes != 1 {
				t.Error("exact bounded JSON", string(data), err)
			}
		})
	}
}
