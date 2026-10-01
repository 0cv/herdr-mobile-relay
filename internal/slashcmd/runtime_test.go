package slashcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestRuntimeCatalogContract(t *testing.T) {
	data, err := os.ReadFile("../../contracts/fixtures/pi_command_catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture Catalog
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	entries := make([]RuntimeCommand, 0, len(fixture.Commands))
	for _, command := range fixture.Commands {
		entries = append(entries, RuntimeCommand{Command: command, Metadata: fixture.Metadata[command.Command]})
	}
	entries = append(entries, RuntimeCommand{Command: Command{Command: "/model", Description: "Cannot replace builtin"}, Metadata: Metadata{Kind: "extension"}})
	catalog := MergePiRuntime(entries, fixture.Status, false)
	if !hasCommand(catalog, "/orchestrate") || !hasCommand(catalog, "/review:2") || !hasCommand(catalog, "/model") {
		t.Fatalf("missing command: %+v", catalog)
	}
	if catalog.Metadata["/model"].Kind != "builtin" || catalog.Metadata["/summary"].Kind != "prompt" {
		t.Fatal("wrong precedence or kind")
	}
	if catalog.Metadata["/review:2"].Provenance.Scope != "temporary" {
		t.Fatal("lost provenance")
	}
	if catalog.Revision != Revise(catalog).Revision {
		t.Fatal("unstable revision")
	}
	changed := catalog
	changed.Status = "partial"
	if catalog.Revision == Revise(changed).Revision {
		t.Fatal("status missing from revision")
	}
	entries = make([]RuntimeCommand, 5000)
	for i := range entries {
		entries[i].Command.Command = fmt.Sprintf("/command-%d", i)
	}
	limited := MergePiRuntime(entries, "available", false)
	if len(limited.Commands) != 4096 || !limited.Truncated || limited.Status != "available" {
		t.Fatal("size and availability conflated")
	}
}
