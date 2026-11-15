package slashcmd

import "strings"

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

func (l *commandList) key(command Command) string {
	if l.foldCase {
		return strings.ToLower(command.Command)
	}
	return command.Command
}

func (l *commandList) addIfAbsent(commands []Command) {
	for _, command := range commands {
		key := l.key(command)
		if _, exists := l.byKey[key]; exists {
			continue
		}
		l.order = append(l.order, key)
		l.byKey[key] = command
	}
}

func (l *commandList) commands() []Command {
	commands := make([]Command, 0, len(l.order))
	for _, key := range l.order {
		commands = append(commands, l.byKey[key])
	}
	return commands
}
