package slashcmd

import (
	"slices"
	"strings"
)

type commandList struct {
	foldCase bool
	order    []string
	byKey    map[string]Command
}

func newCommandList(builtins []Command, foldCase bool) *commandList {
	list := &commandList{
		foldCase: foldCase,
		order:    make([]string, 0, len(builtins)),
		byKey:    make(map[string]Command, len(builtins)),
	}
	list.addIfAbsent(builtins)
	return list
}

func (l *commandList) key(name string) string {
	if l.foldCase {
		return strings.ToLower(name)
	}
	return name
}

func (l *commandList) addIfAbsent(commands []Command) {
	for _, command := range commands {
		key := l.key(command.Command)
		if _, exists := l.byKey[key]; exists {
			continue
		}
		l.order = append(l.order, key)
		l.byKey[key] = command
	}
}

func (l *commandList) set(commands []Command) {
	for _, command := range commands {
		key := l.key(command.Command)
		if _, exists := l.byKey[key]; !exists {
			l.order = append(l.order, key)
		}
		l.byKey[key] = command
	}
}

func (l *commandList) remove(names []string) {
	for _, name := range names {
		key := l.key(name)
		if _, exists := l.byKey[key]; !exists {
			continue
		}
		delete(l.byKey, key)
		l.order = slices.DeleteFunc(l.order, func(existing string) bool { return existing == key })
	}
}

func (l *commandList) commands() []Command {
	commands := make([]Command, 0, len(l.order))
	for _, key := range l.order {
		commands = append(commands, l.byKey[key])
	}
	return commands
}
