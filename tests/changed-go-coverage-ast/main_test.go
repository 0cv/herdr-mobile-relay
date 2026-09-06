package main

import (
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"
)

func TestInstrumentFileInventoriesEveryChangedNamedFunctionStatementAndDecision(t *testing.T) {
	source := []byte(`package sample
func thing(ok, left, right bool, values []int) int {
	total := 0
	if ok && (left || right) {
		total++
	}
	for _, value := range values {
		total += value
	}
	switch total {
	case 0:
		return 0
	default:
		return total
	}
}
`)
	changed := make(map[int]bool)
	for line := 1; line <= 16; line++ {
		changed[line] = true
	}

	instrumented, inventory, err := instrumentFile("sample.go", source, changed)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Functions) != 1 || inventory.Functions[0].Name != "thing" {
		t.Fatalf("functions = %#v", inventory.Functions)
	}
	statementLines := make([]int, 0, len(inventory.Statements))
	for _, statement := range inventory.Statements {
		statementLines = append(statementLines, statement.Line)
	}
	sort.Ints(statementLines)
	if want := []int{3, 4, 5, 7, 8, 10, 12, 14}; !reflect.DeepEqual(statementLines, want) {
		t.Fatalf("statement lines = %v, want %v", statementLines, want)
	}
	decisionKinds := make([]string, 0, len(inventory.Decisions))
	for _, decision := range inventory.Decisions {
		decisionKinds = append(decisionKinds, decision.Kind)
	}
	sort.Strings(decisionKinds)
	if want := []string{"&&", "if", "range", "switch", "||"}; !reflect.DeepEqual(decisionKinds, want) {
		t.Fatalf("decision kinds = %v, want %v", decisionKinds, want)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "instrumented.go", instrumented, parser.AllErrors); err != nil {
		t.Fatalf("instrumented source is invalid: %v\n%s", err, instrumented)
	}
}

func TestInstrumentFileDoesNotInventoryAnUnchangedCondition(t *testing.T) {
	source := []byte(`package sample
func thing(ok bool) int {
	if ok {
		return 1
	}
	return 0
}
`)

	_, inventory, err := instrumentFile("sample.go", source, map[int]bool{4: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Decisions) != 0 {
		t.Fatalf("decisions = %#v", inventory.Decisions)
	}
	if len(inventory.Functions) != 1 {
		t.Fatalf("functions = %#v", inventory.Functions)
	}
}

func TestInstrumentFileTreatsOneChangedCaseAsSelectedOrNotSelected(t *testing.T) {
	source := []byte(`package sample
func thing(value string) int {
	switch value {
	case "old":
		return 1
	case "new":
		return 2
	default:
		return 0
	}
}
`)

	_, inventory, err := instrumentFile("sample.go", source, map[int]bool{6: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Decisions) != 1 {
		t.Fatalf("decisions = %#v", inventory.Decisions)
	}
	decision := inventory.Decisions[0]
	if decision.Kind != "case" || decision.Line != 6 {
		t.Fatalf("decision = %#v", decision)
	}
	want := []string{decision.ID + "|true", decision.ID + "|false"}
	if !reflect.DeepEqual(decision.Markers, want) {
		t.Fatalf("markers = %v, want %v", decision.Markers, want)
	}
}

func TestInstrumentFileIsDeterministic(t *testing.T) {
	source := []byte("package sample\nfunc thing(ok bool) { if ok { return } }\n")
	changed := map[int]bool{2: true}

	firstSource, firstInventory, err := instrumentFile("sample.go", source, changed)
	if err != nil {
		t.Fatal(err)
	}
	secondSource, secondInventory, err := instrumentFile("sample.go", source, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstSource, secondSource) || !reflect.DeepEqual(firstInventory, secondInventory) {
		t.Fatal("instrumentation is not deterministic")
	}
}
