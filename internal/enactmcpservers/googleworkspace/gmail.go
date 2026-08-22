package googleworkspace

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- API shapes -------------------------------------------------------------

type gmailMessagePart struct {
	MimeType string             `json:"mimeType"`
	Filename string             `json:"filename"`
	Headers  []gmailHeader      `json:"headers"`
	Body     gmailBody          `json:"body"`
	Parts    []gmailMessagePart `json:"parts"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailBody struct {
	Size          int    `json:"size"`
	Data          string `json:"data"`
	AttachmentID  string `json:"attachmentId"`
	AttachmentURL string `json:"-"`
}

type gmailMessage struct {
	ID       string           `json:"id"`
	ThreadID string           `json:"threadId"`
	LabelIDs []string         `json:"labelIds"`
	Snippet  string           `json:"snippet"`
	Payload  gmailMessagePart `json:"payload"`
}

type gmailLabel struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	MessagesTotal         int    `json:"messagesTotal"`
	MessagesUnread        int    `json:"messagesUnread"`
	MessageListVisibility string `json:"messageListVisibility"`
}

// --- tools ------------------------------------------------------------------

type searchGmailArgs struct {
	Query      string `json:"query" jsonschema:"Gmail search query, the same syntax as the Gmail search box (e.g. 'from:alice is:unread newer_than:7d')"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"how many messages to return (default 10, max 50)"`
}

type getGmailMessageArgs struct {
	MessageID string `json:"message_id" jsonschema:"the message id, as returned by search_gmail_messages"`
}

type sendGmailArgs struct {
	To      string `json:"to" jsonschema:"recipient address, or several separated by commas"`
	Subject string `json:"subject" jsonschema:"the subject line"`
	Body    string `json:"body" jsonschema:"the plain-text body"`
	Cc      string `json:"cc,omitempty" jsonschema:"carbon-copy addresses, comma separated"`
	Bcc     string `json:"bcc,omitempty" jsonschema:"blind carbon-copy addresses, comma separated"`
}

type modifyLabelsArgs struct {
	MessageID    string   `json:"message_id" jsonschema:"the message to relabel"`
	AddLabelIDs  []string `json:"add_label_ids,omitempty" jsonschema:"label ids to add, from list_gmail_labels"`
	RemoveLabels []string `json:"remove_label_ids,omitempty" jsonschema:"label ids to remove, from list_gmail_labels"`
}

type emptyArgs struct{}

func registerGmail(server *mcp.Server, c *client) {
	addTool(server, &mcp.Tool{
		Name:        "search_gmail_messages",
		Description: "Search the user's Gmail using Gmail query syntax and return matching messages with their subject, sender and snippet.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args searchGmailArgs) (string, any, error) {
		max := args.MaxResults
		if max <= 0 {
			max = 10
		}
		if max > 50 {
			max = 50
		}
		query := url.Values{}
		query.Set("q", args.Query)
		query.Set("maxResults", strconv.Itoa(max))

		var listing struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			ResultSizeEstimate int `json:"resultSizeEstimate"`
		}
		if err := c.get(ctx, gmailBase+"/messages", query, &listing); err != nil {
			return "", nil, err
		}
		if len(listing.Messages) == 0 {
			return fmt.Sprintf("No messages match %q.", args.Query), map[string]any{"messages": []any{}}, nil
		}

		// Metadata format: headers without bodies, which is what a search
		// result needs and a fraction of the payload.
		type hit struct {
			ID      string `json:"id"`
			Subject string `json:"subject"`
			From    string `json:"from"`
			Date    string `json:"date"`
			Snippet string `json:"snippet"`
		}
		hits := make([]hit, 0, len(listing.Messages))
		var summary strings.Builder
		fmt.Fprintf(&summary, "%d message(s) matching %q:\n", len(listing.Messages), args.Query)
		for _, m := range listing.Messages {
			meta := url.Values{}
			meta.Set("format", "metadata")
			meta["metadataHeaders"] = []string{"Subject", "From", "Date"}
			var full gmailMessage
			if err := c.get(ctx, gmailBase+"/messages/"+m.ID, meta, &full); err != nil {
				return "", nil, err
			}
			h := hit{
				ID:      full.ID,
				Subject: header(full.Payload.Headers, "Subject"),
				From:    header(full.Payload.Headers, "From"),
				Date:    header(full.Payload.Headers, "Date"),
				Snippet: full.Snippet,
			}
			hits = append(hits, h)
			fmt.Fprintf(&summary, "- [%s] %s — from %s (%s)\n  %s\n", h.ID, h.Subject, h.From, h.Date, h.Snippet)
		}
		return summary.String(), map[string]any{"messages": hits}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "get_gmail_message_content",
		Description: "Read one Gmail message in full: its headers and its plain-text body.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args getGmailMessageArgs) (string, any, error) {
		if strings.TrimSpace(args.MessageID) == "" {
			return "", nil, fmt.Errorf("message_id is required")
		}
		query := url.Values{}
		query.Set("format", "full")
		var message gmailMessage
		if err := c.get(ctx, gmailBase+"/messages/"+args.MessageID, query, &message); err != nil {
			return "", nil, err
		}
		body, attachments := extractBody(message.Payload)
		structured := map[string]any{
			"id":          message.ID,
			"thread_id":   message.ThreadID,
			"subject":     header(message.Payload.Headers, "Subject"),
			"from":        header(message.Payload.Headers, "From"),
			"to":          header(message.Payload.Headers, "To"),
			"date":        header(message.Payload.Headers, "Date"),
			"label_ids":   message.LabelIDs,
			"body":        body,
			"attachments": attachments,
		}
		summary := fmt.Sprintf("Subject: %s\nFrom: %s\nTo: %s\nDate: %s\n\n%s",
			header(message.Payload.Headers, "Subject"),
			header(message.Payload.Headers, "From"),
			header(message.Payload.Headers, "To"),
			header(message.Payload.Headers, "Date"),
			truncate(body, 20000))
		if len(attachments) > 0 {
			summary += "\n\nAttachments: " + strings.Join(attachments, ", ")
		}
		return summary, structured, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "send_gmail_message",
		Description: "Send an email from the user's Gmail account. The message is sent immediately — use draft_gmail_message to prepare one without sending.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args sendGmailArgs) (string, any, error) {
		raw, err := buildRFC822(args)
		if err != nil {
			return "", nil, err
		}
		var sent struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		}
		if err := c.post(ctx, gmailBase+"/messages/send", map[string]any{"raw": raw}, &sent); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Sent to %s (message id %s).", args.To, sent.ID),
			map[string]any{"id": sent.ID, "thread_id": sent.ThreadID}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "draft_gmail_message",
		Description: "Create a Gmail draft without sending it.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args sendGmailArgs) (string, any, error) {
		raw, err := buildRFC822(args)
		if err != nil {
			return "", nil, err
		}
		var draft struct {
			ID      string `json:"id"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		body := map[string]any{"message": map[string]any{"raw": raw}}
		if err := c.post(ctx, gmailBase+"/drafts", body, &draft); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Draft %s created for %s.", draft.ID, args.To),
			map[string]any{"draft_id": draft.ID, "message_id": draft.Message.ID}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_gmail_labels",
		Description: "List the labels in the user's mailbox, including the system ones (INBOX, UNREAD, STARRED) and their ids.",
		Annotations: readOnly(),
	}, func(ctx context.Context, _ emptyArgs) (string, any, error) {
		var listing struct {
			Labels []gmailLabel `json:"labels"`
		}
		if err := c.get(ctx, gmailBase+"/labels", nil, &listing); err != nil {
			return "", nil, err
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "%d labels:\n", len(listing.Labels))
		for _, l := range listing.Labels {
			fmt.Fprintf(&summary, "- %s (id %s, %s)\n", l.Name, l.ID, strings.ToLower(l.Type))
		}
		return summary.String(), map[string]any{"labels": listing.Labels}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "modify_gmail_message_labels",
		Description: "Add or remove labels on one message — how a message is archived (remove INBOX), marked read (remove UNREAD) or starred (add STARRED).",
		Annotations: mutating(false),
	}, func(ctx context.Context, args modifyLabelsArgs) (string, any, error) {
		if strings.TrimSpace(args.MessageID) == "" {
			return "", nil, fmt.Errorf("message_id is required")
		}
		if len(args.AddLabelIDs) == 0 && len(args.RemoveLabels) == 0 {
			return "", nil, fmt.Errorf("give at least one label to add or remove")
		}
		body := map[string]any{}
		if len(args.AddLabelIDs) > 0 {
			body["addLabelIds"] = args.AddLabelIDs
		}
		if len(args.RemoveLabels) > 0 {
			body["removeLabelIds"] = args.RemoveLabels
		}
		var updated gmailMessage
		if err := c.post(ctx, gmailBase+"/messages/"+args.MessageID+"/modify", body, &updated); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Message %s now has labels: %s.", updated.ID, strings.Join(updated.LabelIDs, ", ")),
			map[string]any{"id": updated.ID, "label_ids": updated.LabelIDs}, nil
	})
}

// --- helpers ----------------------------------------------------------------

// header reads one header value, case-insensitively.
func header(headers []gmailHeader, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// extractBody walks the MIME tree for readable text and the attachment names.
//
// Gmail returns a nested part tree, not a body: a plain message has its text
// on the payload itself, a multipart/alternative has text/plain and text/html
// siblings, and a message with attachments nests those inside another
// multipart. Preferring text/plain and falling back to stripped HTML is what
// makes the result readable to a model rather than a wall of markup.
func extractBody(part gmailMessagePart) (string, []string) {
	var plain, html strings.Builder
	var attachments []string

	var walk func(p gmailMessagePart)
	walk = func(p gmailMessagePart) {
		if p.Filename != "" {
			attachments = append(attachments, p.Filename)
		}
		switch {
		case strings.HasPrefix(p.MimeType, "text/plain") && p.Body.Data != "":
			plain.WriteString(decodeBase64URL(p.Body.Data))
		case strings.HasPrefix(p.MimeType, "text/html") && p.Body.Data != "":
			html.WriteString(decodeBase64URL(p.Body.Data))
		}
		for _, child := range p.Parts {
			walk(child)
		}
	}
	walk(part)

	if text := strings.TrimSpace(plain.String()); text != "" {
		return text, attachments
	}
	if markup := strings.TrimSpace(html.String()); markup != "" {
		return stripHTML(markup), attachments
	}
	return "", attachments
}

// decodeBase64URL decodes Gmail's base64url payloads, tolerating the missing
// padding Gmail sometimes omits.
func decodeBase64URL(data string) string {
	if decoded, err := base64.URLEncoding.DecodeString(data); err == nil {
		return string(decoded)
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(data); err == nil {
		return string(decoded)
	}
	return ""
}

// stripHTML is a crude tag remover for the HTML-only messages. It is not a
// parser and does not try to be: the goal is readable text for a model, not
// fidelity.
func stripHTML(markup string) string {
	var out strings.Builder
	depth := 0
	for _, r := range markup {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			out.WriteRune(r)
		}
	}
	// Collapse the whitespace the markup leaves behind.
	fields := strings.Fields(out.String())
	return strings.Join(fields, " ")
}

// buildRFC822 assembles the message Gmail wants: an RFC 822 document,
// base64url encoded.
func buildRFC822(args sendGmailArgs) (string, error) {
	if strings.TrimSpace(args.To) == "" {
		return "", fmt.Errorf("to is required")
	}
	headers := []string{
		"To: " + args.To,
		"Subject: " + args.Subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=\"UTF-8\"",
	}
	if strings.TrimSpace(args.Cc) != "" {
		headers = append(headers, "Cc: "+args.Cc)
	}
	if strings.TrimSpace(args.Bcc) != "" {
		headers = append(headers, "Bcc: "+args.Bcc)
	}
	message := strings.Join(headers, "\r\n") + "\r\n\r\n" + args.Body
	return base64.URLEncoding.EncodeToString([]byte(message)), nil
}
