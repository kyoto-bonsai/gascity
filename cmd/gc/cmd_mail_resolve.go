package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// cmd_mail_resolve.go implements `gc mail resolve`, the ga-eibq22 S5 escape
// hatch: clears the --expects-reply exemption on a message answered outside
// "gc mail reply" (dashboard, direct bead edit, a side conversation).
// HasReply can only see replies created through the reply path (it is a
// reply-to: label query), so without this an ask answered another way would
// stay in "gc mail sent --outstanding" forever (ga-eibq22 F3).

// expectsReplyResolver is the optional beadmail-specific capability
// newMailResolveCmd requires (mirrors expectsReplyMarker in cmd_mail.go).
type expectsReplyResolver interface {
	ClearExpectsReply(id string) error
}

func newMailResolveCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "resolve <id>",
		Short: "Clear the expects-reply exemption on a message you sent",
		Long: `Clear the expects-reply marker on a message, releasing it back to
ordinary read-mail retention.

Use this when an ask sent with --expects-reply was answered outside "gc mail
reply" -- a dashboard reply, a direct bead edit, or a side conversation leave
no reply-to: label, so "gc mail sent --outstanding" would otherwise keep
listing it as unanswered indefinitely.`,
		Example: `  gc mail resolve ga-wisp-x1vw0g`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdMailResolve(args[0], jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL result")
	return cmd
}

func cmdMailResolve(id string, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail resolve")
	if mp == nil {
		return code
	}
	resolver, ok := mp.(expectsReplyResolver)
	if !ok {
		fmt.Fprintln(stderr, "gc mail resolve: requires the beadmail provider") //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := resolver.ClearExpectsReply(id); err != nil {
		telemetry.RecordMailOp(context.Background(), "resolve", err)
		fmt.Fprintf(stderr, "gc mail resolve: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	telemetry.RecordMailOp(context.Background(), "resolve", nil)

	rec := openCityRecorder(stderr)
	rec.Record(events.Event{
		Type:    events.MailResolved,
		Actor:   eventActor(),
		Subject: id,
		Payload: mailEventPayload(nil),
	})

	if jsonOut {
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail resolve", mailActionResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       "mail.resolve",
			Action:        "resolve",
			ID:            id,
			IDs:           []string{id},
			Count:         intRef(1),
		})
	}
	fmt.Fprintf(stdout, "Resolved %s (expects-reply exemption cleared)\n", id) //nolint:errcheck // best-effort stdout
	return 0
}
