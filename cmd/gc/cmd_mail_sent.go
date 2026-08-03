package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// cmd_mail_sent.go implements `gc mail sent`, the sender-side disposition
// surface ga-eibq22 S4 specifies. gc mail inbox is recipient-scoped and
// unread-only, so a sender has no instrument to ask "was my message seen or
// answered?" -- this is that instrument.

// sentLister is the optional beadmail-specific capability newMailSentCmd
// requires (mirrors archiveMatchingProvider/expectsReplyMarker in
// cmd_mail.go): sender-side disposition is not part of [mail.Provider].
type sentLister interface {
	ListSent(sender string, outstandingOnly bool) ([]beadmail.SentMessage, error)
}

type mailSentJSONResult struct {
	SchemaVersion string            `json:"schema_version"`
	Sender        string            `json:"sender"`
	Outstanding   bool              `json:"outstanding"`
	Messages      []mailSentJSONRow `json:"messages"`
}

type mailSentJSONRow struct {
	ID           string `json:"id"`
	To           string `json:"to,omitempty"`
	Subject      string `json:"subject,omitempty"`
	ExpectsReply bool   `json:"expects_reply"`
	Disposition  string `json:"disposition"`
	ReadEvidence string `json:"read_evidence"`
	CreatedAt    string `json:"created_at,omitempty"`
}

func newMailSentCmd(stdout, stderr io.Writer) *cobra.Command {
	var outstanding bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "sent",
		Short: "List messages you sent, with answered/unanswered disposition",
		Long: `List messages sent by you, with a disposition column so you can tell
whether an ask was seen or answered instead of guessing from silence.

Disposition is "answered"/"unanswered" for messages sent with
--expects-reply (see "gc mail send --expects-reply"); ordinary messages show
"-". The read-evidence column reads "seen" when the recipient has read the
message, or "no evidence" -- never "unread", since a message may have been
read through a surface that does not stamp mail.read (dashboard, direct bead
inspection), so absence of the flag is not proof of absence of reading.

Use --outstanding to show only unanswered --expects-reply asks: the exact set
the read-mail retention sweep and wisp purge are currently retaining on your
behalf. If one was answered outside "gc mail reply" (dashboard, direct bead
edit), use "gc mail resolve <id>" to clear it manually.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdMailSent(outstanding, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&outstanding, "outstanding", false, "show only unanswered --expects-reply asks")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON result")
	return cmd
}

func cmdMailSent(outstanding, jsonOut bool, stdout, stderr io.Writer) int {
	mp, code := openCityMailProvider(stderr, "gc mail sent")
	if mp == nil {
		return code
	}
	lister, ok := mp.(sentLister)
	if !ok {
		fmt.Fprintln(stderr, "gc mail sent: requires the beadmail provider") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc mail sent: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, _ := loadCityConfig(cityPath, stderr)
	store, storeCode := openCityStore(stderr, "gc mail sent")
	if store == nil {
		return storeCode
	}
	sender, ok := resolveDefaultMailSenderForCommand(cityPath, cfg, store, stderr, "gc mail sent")
	if !ok {
		return 1
	}

	messages, err := lister.ListSent(sender, outstanding)
	if err != nil {
		fmt.Fprintf(stderr, "gc mail sent: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		rows := make([]mailSentJSONRow, len(messages))
		for i, m := range messages {
			rows[i] = mailSentJSONRow{
				ID:           m.ID,
				To:           m.To,
				Subject:      m.Subject,
				ExpectsReply: m.ExpectsReply,
				Disposition:  mailDisposition(m),
				ReadEvidence: mailReadEvidence(m),
				CreatedAt:    m.CreatedAt.Format(time.RFC3339),
			}
		}
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail sent", mailSentJSONResult{
			SchemaVersion: "1",
			Sender:        sender,
			Outstanding:   outstanding,
			Messages:      rows,
		})
	}

	if len(messages) == 0 {
		if outstanding {
			fmt.Fprintf(stdout, "No outstanding asks for %s\n", sender) //nolint:errcheck // best-effort stdout
		} else {
			fmt.Fprintf(stdout, "No sent messages for %s\n", sender) //nolint:errcheck // best-effort stdout
		}
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTO\tSUBJECT\tDISPOSITION\tREAD-EVIDENCE") //nolint:errcheck // best-effort stdout
	for _, m := range messages {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.To, m.Subject, mailDisposition(m), mailReadEvidence(m)) //nolint:errcheck // best-effort stdout
	}
	tw.Flush() //nolint:errcheck // best-effort stdout
	return 0
}

func mailDisposition(m beadmail.SentMessage) string {
	if !m.ExpectsReply {
		return "-"
	}
	if m.Answered {
		return "answered"
	}
	return "unanswered"
}

func mailReadEvidence(m beadmail.SentMessage) string {
	if m.Read {
		return "seen"
	}
	return "no evidence"
}
