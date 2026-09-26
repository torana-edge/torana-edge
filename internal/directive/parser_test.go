package directive

import (
	"fmt"
	"strings"
	"testing"
)

func namespace(name string) bool { return name == "router" }

func TestParseTextRecognitionMatrix(t *testing.T) {
	prefixes := []struct {
		text  string
		match bool
	}{
		{"", true}, {" ", false}, {"\t", false}, {">", false}, {"x ", false}, {"\\", false},
	}
	bodies := []struct {
		text  string
		known bool
	}{
		{"accept 7f3k", true}, {"dismiss 7f3k", true}, {"status", true}, {"help", true},
		{"ask why", true}, {"setup", true}, {"router pin opus-hi", true},
		{"torana help", true}, {"unknown x", false}, {"", false},
	}
	for _, prefix := range prefixes {
		for _, body := range bodies {
			t.Run(fmt.Sprintf("%q_%q", prefix.text, body.text), func(t *testing.T) {
				input := prefix.text + "torana> " + body.text + "\nordinary text"
				got := ParseText(input, namespace)
				if len(got.Commands) != btoi(prefix.match) {
					t.Fatalf("commands = %+v, expected match %v", got.Commands, prefix.match)
				}
				if prefix.match {
					if got.Commands[0].Known != body.known || got.Text != "ordinary text" {
						t.Fatalf("parsed = %+v", got)
					}
				} else if prefix.text == "\\" {
					if got.Text != strings.TrimPrefix(input, "\\") || !got.Changed {
						t.Fatalf("escaped literal not unescaped: %q", got.Text)
					}
				} else if got.Text != input {
					t.Fatalf("non-directive changed: %q", got.Text)
				}
			})
		}
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestParseTextFencesEscapesAndLimit(t *testing.T) {
	input := strings.Join([]string{
		"```shell", "torana> accept abcd", "```",
		"\\torana> status", "torana> router pin opus-hi 3", "torana> status",
		"torana> help", "torana> accept 7f3k", "torana> dismiss 7f3k",
	}, "\n")
	got := ParseText(input, namespace)
	if len(got.Commands) != 5 || got.Commands[4].Reason != "too_many_directives" {
		t.Fatalf("directive limit: %+v", got.Commands)
	}
	if !strings.Contains(got.Text, "torana> accept abcd") || !strings.Contains(got.Text, "torana> status") || strings.Contains(got.Text, "\\torana> status") {
		t.Fatalf("fence or escape mishandled: %q", got.Text)
	}
	if got.Commands[0].Namespace != "router" || got.Commands[0].Verb != "pin" || got.Commands[0].Args != "opus-hi 3" {
		t.Fatalf("namespace command: %+v", got.Commands[0])
	}
	if got.DirectiveOnly {
		t.Fatal("fenced and escaped text is ordinary user content")
	}
}

func TestParseTextDirectiveOnly(t *testing.T) {
	got := ParseText("torana> status\ntorana> help\n", namespace)
	if !got.DirectiveOnly || got.Text != "" {
		t.Fatalf("directive-only result: %+v", got)
	}
	plain := "ordinary text\n"
	if got := ParseText(plain, namespace); got.Changed || got.Text != plain {
		t.Fatalf("ordinary text changed: %+v", got)
	}
}

func TestFenceWithTrailingTextDoesNotExposeDirective(t *testing.T) {
	input := "```text\n```not-a-closing-fence\ntorana> accept abcd\n```\ntorana> status"
	got := ParseText(input, namespace)
	if len(got.Commands) != 1 || got.Commands[0].Verb != "status" || !strings.Contains(got.Text, "torana> accept abcd") {
		t.Fatalf("code-fenced command was exposed: %+v", got)
	}
}
