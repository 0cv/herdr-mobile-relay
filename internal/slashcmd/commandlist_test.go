package slashcmd

import (
	"slices"
	"testing"
)

func TestCommandListFirstWinsInOrder(t *testing.T) {
	list := newCommandList([]Command{{Command: "/a", Source: "builtin"}, {Command: "/b", Source: "builtin"}}, false)
	list.addIfAbsent([]Command{{Command: "/c", Source: "one"}, {Command: "/a", Source: "one"}})
	list.addIfAbsent([]Command{{Command: "/c", Source: "two"}, {Command: "/d", Source: "two"}})

	got := []string{}
	for _, command := range list.commands() {
		got = append(got, command.Command+"|"+command.Source)
	}
	want := []string{"/a|builtin", "/b|builtin", "/c|one", "/d|two"}
	if !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestCommandListFoldsCaseAndKeepsSpelling(t *testing.T) {
	list := newCommandList([]Command{{Command: "/Skill:Review", Source: "builtin"}}, true)
	list.addIfAbsent([]Command{{Command: "/skill:review", Source: "custom"}, {Command: "/Skill:Deploy", Source: "custom"}, {Command: "/SKILL:DEPLOY", Source: "other"}})

	got := []string{}
	for _, command := range list.commands() {
		got = append(got, command.Command+"|"+command.Source)
	}
	want := []string{"/Skill:Review|builtin", "/Skill:Deploy|custom"}
	if !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestCommandListExactKeysAreCaseSensitiveByDefault(t *testing.T) {
	list := newCommandList(nil, false)
	list.addIfAbsent([]Command{{Command: "/a"}, {Command: "/A"}})
	if got := len(list.commands()); got != 2 {
		t.Fatalf("commands = %d, want 2", got)
	}
}

func TestCommandListCommandsReturnsFreshSlice(t *testing.T) {
	builtins := []Command{{Command: "/a"}}
	list := newCommandList(builtins, false)
	list.commands()[0].Command = "/changed"
	if builtins[0].Command != "/a" || list.commands()[0].Command != "/a" {
		t.Fatal("commands() must not share storage with the list or the builtins")
	}
}

func commandListNames(list *commandList) []string {
	names := []string{}
	for _, command := range list.commands() {
		names = append(names, command.Command+"|"+command.Source)
	}
	return names
}

func TestCommandListSetReplacesInPlace(t *testing.T) {
	list := newCommandList([]Command{{Command: "/a", Source: "builtin"}, {Command: "/b", Source: "builtin"}}, false)
	list.set([]Command{{Command: "/a", Source: "custom"}, {Command: "/c", Source: "custom"}})

	want := []string{"/a|custom", "/b|builtin", "/c|custom"}
	if got := commandListNames(list); !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

func TestCommandListRemoveThenSetAppendsAtEnd(t *testing.T) {
	list := newCommandList([]Command{{Command: "/a", Source: "builtin"}, {Command: "/b", Source: "builtin"}, {Command: "/c", Source: "builtin"}}, false)
	list.remove([]string{"/a", "/missing"})
	if got, want := commandListNames(list), []string{"/b|builtin", "/c|builtin"}; !slices.Equal(got, want) {
		t.Fatalf("after remove commands = %v, want %v", got, want)
	}
	list.set([]Command{{Command: "/a", Source: "custom"}})

	want := []string{"/b|builtin", "/c|builtin", "/a|custom"}
	if got := commandListNames(list); !slices.Equal(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}
