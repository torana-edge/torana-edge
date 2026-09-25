package proxy

import (
	"context"
	"fmt"
	"strings"

	"github.com/torana-edge/torana-edge/internal/directive"
	"github.com/torana-edge/torana-edge/internal/metrics"
	"github.com/torana-edge/torana-edge/internal/suggest"
)

// dispatchCoreDirectives handles the small host-owned consent vocabulary.
// Namespace operations and natural-language ask enter the common operation
// registry in a later stage; an unknown command never reaches a plugin.
func dispatchCoreDirectives(store *suggest.Store, conversation string, turn uint64, commands []directive.Command) string {
	var lines []string
	for _, command := range commands {
		if !command.Known {
			metrics.RecordDirective(context.Background(), command.Verb, "invalid")
			lines = append(lines, "Unrecognized Torana command ignored. Try `torana> help`.")
			continue
		}
		if command.Namespace != "torana" {
			metrics.RecordDirective(context.Background(), command.Verb, "unavailable")
			lines = append(lines, "That Torana namespace has no available commands. Try `torana> help`.")
			continue
		}
		switch command.Verb {
		case "accept", "dismiss":
			args := strings.Fields(command.Args)
			if len(args) != 1 {
				metrics.RecordDirective(context.Background(), command.Verb, "invalid")
				lines = append(lines, fmt.Sprintf("Use `torana> %s <code>` with one confirmation code.", command.Verb))
				continue
			}
			status := "accepted"
			if command.Verb == "dismiss" {
				status = "dismissed"
			}
			if store == nil || conversation == "" {
				metrics.RecordDirective(context.Background(), command.Verb, "unavailable")
				lines = append(lines, "Torana could not identify this conversation; use the control plane to manage suggestions.")
				continue
			}
			item, err := store.ResolveCode(conversation, args[0], status, "directive", turn)
			if err != nil {
				metrics.RecordDirective(context.Background(), command.Verb, "not_found")
				lines = append(lines, "That confirmation code is not pending in this conversation.")
				continue
			}
			metrics.RecordSuggestion(context.Background(), item.Kind, item.Status, item.Via)
			metrics.RecordDirective(context.Background(), command.Verb, status)
			lines = append(lines, fmt.Sprintf("%s: %s.", strings.ToUpper(status[:1])+status[1:], item.Title))
		case "status":
			if strings.TrimSpace(command.Args) != "" {
				metrics.RecordDirective(context.Background(), command.Verb, "invalid")
				lines = append(lines, "Use `torana> status` without arguments.")
				continue
			}
			if store == nil || conversation == "" {
				metrics.RecordDirective(context.Background(), command.Verb, "unavailable")
				lines = append(lines, "Torana could not identify this conversation.")
				continue
			}
			items, err := store.List(conversation, "", turn)
			if err != nil {
				metrics.RecordDirective(context.Background(), command.Verb, "unavailable")
				lines = append(lines, "Torana could not read suggestions right now.")
				continue
			}
			metrics.RecordDirective(context.Background(), command.Verb, "shown")
			var pending []string
			for _, item := range items {
				if item.Status == "pending" {
					pending = append(pending, item.Code+": "+item.Title)
				}
			}
			if len(pending) == 0 {
				lines = append(lines, "No pending Torana suggestions for this conversation.")
			} else {
				lines = append(lines, "Pending Torana suggestions:\n"+strings.Join(pending, "\n"))
			}
		case "help":
			metrics.RecordDirective(context.Background(), command.Verb, "shown")
			lines = append(lines, "Torana commands: `torana> status`, `torana> accept <code>`, `torana> dismiss <code>`. You can also manage suggestions in Torana's UI or CLI.")
		default:
			metrics.RecordDirective(context.Background(), command.Verb, "unavailable")
			lines = append(lines, "That Torana command is not available here yet. Try `torana> help`.")
		}
	}
	return strings.Join(lines, "\n\n")
}
