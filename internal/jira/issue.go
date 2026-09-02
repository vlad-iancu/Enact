package jira

import (
	"encoding/json"
	"strings"
)

// issueResponse is the slice of an issue this crawl reads. Deliberately
// partial: the API returns a great deal that is not text.
type issueResponse struct {
	Key    string      `json:"key"`
	Fields issueFields `json:"fields"`
}

type issueFields struct {
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description"`
	IssueType   named           `json:"issuetype"`
	Status      named           `json:"status"`
	Priority    named           `json:"priority"`
	Labels      []string        `json:"labels"`
	Components  []named         `json:"components"`
	Parent      *linkedIssue    `json:"parent"`
	Subtasks    []linkedIssue   `json:"subtasks"`
	IssueLinks  []issueLink     `json:"issuelinks"`
	Comment     *commentPage    `json:"comment"`
}

type named struct {
	Name string `json:"name"`
}

type linkedIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type issueLink struct {
	Type struct {
		Inward  string `json:"inward"`
		Outward string `json:"outward"`
	} `json:"type"`
	InwardIssue  *linkedIssue `json:"inwardIssue"`
	OutwardIssue *linkedIssue `json:"outwardIssue"`
}

type commentPage struct {
	Comments []struct {
		Author named           `json:"author"`
		Body   json.RawMessage `json:"body"`
	} `json:"comments"`
}

// MaxCommentsRead bounds how much of a comment thread contributes text.
//
// A long-running ticket accumulates hundreds of comments, most of them status
// updates and build notifications. The first several carry the substance; the
// tail dilutes every term frequency the scoring is built on.
const MaxCommentsRead = 20

// issueProse is the part of an issue a person actually wrote: its summary, the
// words on its labels and components, its description, and its discussion.
//
// This is what relevance judges, and it is deliberately NOT what is stored.
// issueText below writes field labels — "Summary:", "Status:", "Priority:" —
// which a reader of the stored document wants and a scorer must not see.
// Measured on a real ticket, ten of its twelve tokens were those labels and
// their values; every ticket carried the same ten, so every ticket resembled
// every other; and because the labels are Title Case the entity recogniser
// read "Summary", "Progress" and "Priority" as NAMES and weighted them triple,
// even fusing "In Progress" and "Priority" across a line break into a
// multi-word name that exists nowhere. Removing them moved an irrelevant
// ticket from 0.243 to 0.120 and a relevant one from 0.383 to 0.484.
//
// Status, type and priority are omitted entirely rather than stripped of their
// labels: "Epic", "To Do" and "Medium" are drawn from a fixed vocabulary
// shared by every issue on the site, so they can only ever make two issues
// look more alike.
func issueProse(issue issueResponse) string {
	var b strings.Builder
	line := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			b.WriteString(value)
			b.WriteString("\n")
		}
	}

	line(issue.Fields.Summary)
	// Labels and components are words somebody chose for this issue, so they
	// are content — the label names ("Labels:") are not.
	line(strings.Join(issue.Fields.Labels, ", "))
	for _, c := range issue.Fields.Components {
		line(c.Name)
	}
	line(flattenADF(issue.Fields.Description))
	if issue.Fields.Comment != nil {
		for i, comment := range issue.Fields.Comment.Comments {
			if i >= MaxCommentsRead {
				break
			}
			line(flattenADF(comment.Body))
		}
	}
	return strings.TrimSpace(b.String())
}

// issueText renders an issue as the document a crawl stores.
//
// Structured deliberately: the summary and description first, because that is
// what the issue is ABOUT, then the metadata a person would want back when
// this document is retrieved from a knowledge base, then the discussion. An
// issue is not prose and pretending otherwise loses what makes it useful.
//
// Scoring uses issueProse instead; see the note there.
func issueText(issue issueResponse) string {
	var b strings.Builder
	write := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			b.WriteString(label)
			b.WriteString(": ")
			b.WriteString(value)
			b.WriteString("\n")
		}
	}

	// The key is written into the text as well as onto the title, because the
	// title is not scored and not stored — an issue somebody searches for by
	// key would otherwise be findable by every word except its name.
	write("Issue", issue.Key)
	write("Summary", issue.Fields.Summary)
	write("Type", issue.Fields.IssueType.Name)
	write("Status", issue.Fields.Status.Name)
	write("Priority", issue.Fields.Priority.Name)
	if len(issue.Fields.Labels) > 0 {
		write("Labels", strings.Join(issue.Fields.Labels, ", "))
	}
	if len(issue.Fields.Components) > 0 {
		names := make([]string, 0, len(issue.Fields.Components))
		for _, c := range issue.Fields.Components {
			names = append(names, c.Name)
		}
		write("Components", strings.Join(names, ", "))
	}

	if description := flattenADF(issue.Fields.Description); description != "" {
		b.WriteString("\n")
		b.WriteString(description)
		b.WriteString("\n")
	}

	if issue.Fields.Comment != nil && len(issue.Fields.Comment.Comments) > 0 {
		b.WriteString("\nComments:\n")
		for i, comment := range issue.Fields.Comment.Comments {
			if i >= MaxCommentsRead {
				break
			}
			if body := flattenADF(comment.Body); body != "" {
				b.WriteString(body)
				b.WriteString("\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// flattenADF turns Atlassian Document Format into plain text.
//
// v3 of the API returns rich text as a JSON tree rather than a string, so
// there is no way around walking it. Only the "text" leaves matter here;
// everything else in the format describes appearance, and a crawl scores
// words. Block-level nodes end a line so that words either side of a boundary
// do not run together into a token that is neither.
//
// The field is json.RawMessage because older sites and some endpoints still
// return a plain string, and a type error on one issue should not fail a run.
func flattenADF(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// The plain-string case, from v2-era responses.
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return strings.TrimSpace(plain)
	}
	var node adfNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	var b strings.Builder
	walkADF(&b, node)
	return strings.TrimSpace(collapseBlankLines(b.String()))
}

type adfNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Content []adfNode       `json:"content"`
	Attrs   json.RawMessage `json:"attrs"`
}

// adfBlock are the node types worth a line break after.
var adfBlock = map[string]bool{
	"paragraph": true, "heading": true, "listItem": true, "codeBlock": true,
	"blockquote": true, "rule": true, "tableRow": true, "panel": true,
	"bulletList": true, "orderedList": true, "table": true, "mediaSingle": true,
}

func walkADF(b *strings.Builder, node adfNode) {
	if node.Type == "text" && node.Text != "" {
		b.WriteString(node.Text)
	}
	// hardBreak carries no text but is one.
	if node.Type == "hardBreak" {
		b.WriteString("\n")
	}
	for _, child := range node.Content {
		walkADF(b, child)
	}
	if adfBlock[node.Type] {
		b.WriteString("\n")
	}
}

// collapseBlankLines keeps paragraph structure without letting the block rule
// above produce runs of empty lines.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, trimmed)
	}
	return strings.Join(out, "\n")
}
