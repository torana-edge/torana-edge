// Package directive parses Torana's in-conversation command syntax. It does
// not execute commands or decide whether an operation is authorized.
package directive

import "strings"

const maxPerMessage = 4

type Command struct {
	Raw       string
	Namespace string
	Verb      string
	Args      string
	Known     bool
	Reason    string
}

type Result struct {
	// Text is the user text with reserved-prefix lines removed and escaped
	// literal prefixes unescaped. It is byte-identical when no line matched.
	Text          string
	Commands      []Command
	DirectiveOnly bool
	Changed       bool
}

var coreVerbs = map[string]bool{
	"accept": true, "dismiss": true, "undo": true, "status": true,
	"help": true, "ask": true, "setup": true,
}

// ParseText removes every unescaped reserved-prefix line outside a fenced
// code block, including unknown commands. The caller supplies the set of
// active namespace names and aliases. Unknown lines must be reported to the
// user by the caller; they are never silently forwarded to the provider.
func ParseText(input string, knownNamespace func(string) bool) Result {
	result := Result{Text: input}
	if !strings.Contains(input, "torana>") {
		return result
	}
	var output strings.Builder
	output.Grow(len(input))
	fence := byte(0)
	fenceWidth := 0
	for rest := input; rest != ""; {
		line, tail, hasNewline := strings.Cut(rest, "\n")
		rest = tail
		ending := ""
		if hasNewline {
			ending = "\n"
		}
		trimmed := strings.TrimLeft(line, " \t")
		if marker, width := fenceMarker(trimmed); marker != 0 {
			if fence == 0 {
				fence, fenceWidth = marker, width
			} else if marker == fence && width >= fenceWidth && strings.TrimSpace(trimmed[width:]) == "" {
				fence, fenceWidth = 0, 0
			}
			output.WriteString(line)
			output.WriteString(ending)
			continue
		}
		if fence == 0 && strings.HasPrefix(line, `\torana>`) {
			output.WriteString(strings.TrimPrefix(line, `\`))
			output.WriteString(ending)
			result.Changed = true
			continue
		}
		if fence == 0 && strings.HasPrefix(line, "torana> ") {
			command := parseLine(strings.TrimPrefix(line, "torana> "), knownNamespace)
			command.Raw = line
			if len(result.Commands) >= maxPerMessage {
				command.Known, command.Reason = false, "too_many_directives"
			}
			result.Commands = append(result.Commands, command)
			result.Changed = true
			continue
		}
		output.WriteString(line)
		output.WriteString(ending)
	}
	if !result.Changed {
		return result
	}
	result.Text = output.String()
	result.DirectiveOnly = len(result.Commands) > 0 && strings.TrimSpace(result.Text) == ""
	return result
}

func parseLine(body string, knownNamespace func(string) bool) Command {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return Command{Reason: "empty"}
	}
	if coreVerbs[fields[0]] {
		return Command{Namespace: "torana", Verb: fields[0], Args: strings.TrimSpace(strings.TrimPrefix(body, fields[0])), Known: true}
	}
	if fields[0] == "torana" || knownNamespace != nil && knownNamespace(fields[0]) {
		if len(fields) < 2 {
			return Command{Namespace: fields[0], Reason: "missing_verb"}
		}
		verb := fields[1]
		args := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(body, fields[0])), verb))
		return Command{Namespace: fields[0], Verb: verb, Args: args, Known: true}
	}
	return Command{Reason: "unknown_command"}
}

func fenceMarker(line string) (byte, int) {
	if len(line) < 3 || line[0] != '`' && line[0] != '~' {
		return 0, 0
	}
	width := 0
	for width < len(line) && line[width] == line[0] {
		width++
	}
	if width < 3 {
		return 0, 0
	}
	return line[0], width
}
