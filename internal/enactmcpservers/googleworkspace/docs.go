package googleworkspace

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A Google document is a tree of structural elements, each paragraph a list
// of runs. There is no "text" field anywhere — the text has to be gathered
// from the runs, which is what docText does.
type docsDocument struct {
	DocumentID string `json:"documentId"`
	Title      string `json:"title"`
	Body       struct {
		Content []docsStructuralElement `json:"content"`
	} `json:"body"`
}

type docsStructuralElement struct {
	StartIndex int `json:"startIndex"`
	EndIndex   int `json:"endIndex"`
	Paragraph  *struct {
		Elements []struct {
			TextRun *struct {
				Content string `json:"content"`
			} `json:"textRun"`
		} `json:"elements"`
	} `json:"paragraph"`
	Table *struct {
		TableRows []struct {
			TableCells []struct {
				Content []docsStructuralElement `json:"content"`
			} `json:"tableCells"`
		} `json:"tableRows"`
	} `json:"table"`
}

type createDocArgs struct {
	Title   string `json:"title" jsonschema:"the document title"`
	Content string `json:"content,omitempty" jsonschema:"initial body text"`
}

type getDocArgs struct {
	DocumentID string `json:"document_id" jsonschema:"the document to read"`
}

type insertDocTextArgs struct {
	DocumentID string `json:"document_id" jsonschema:"the document to write to"`
	Text       string `json:"text" jsonschema:"the text to insert"`
	Index      int    `json:"index,omitempty" jsonschema:"1-based character index to insert at; omit to append to the end"`
}

type findReplaceDocArgs struct {
	DocumentID string `json:"document_id" jsonschema:"the document to change"`
	Find       string `json:"find" jsonschema:"the text to look for"`
	Replace    string `json:"replace" jsonschema:"what to put in its place"`
	MatchCase  bool   `json:"match_case,omitempty" jsonschema:"whether the search is case sensitive"`
}

func registerDocs(server *mcp.Server, c *client) {
	addTool(server, &mcp.Tool{
		Name:        "create_doc",
		Description: "Create a Google Doc, optionally with initial text.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args createDocArgs) (string, any, error) {
		if strings.TrimSpace(args.Title) == "" {
			return "", nil, fmt.Errorf("title is required")
		}
		var created docsDocument
		if err := c.post(ctx, docsBase+"/documents", map[string]any{"title": args.Title}, &created); err != nil {
			return "", nil, err
		}
		if args.Content != "" {
			// Index 1, not 0: index 0 is before the document's implicit
			// opening segment and Docs rejects it.
			requests := []map[string]any{{
				"insertText": map[string]any{
					"location": map[string]any{"index": 1},
					"text":     args.Content,
				},
			}}
			if err := c.post(ctx, docsBase+"/documents/"+created.DocumentID+":batchUpdate",
				map[string]any{"requests": requests}, nil); err != nil {
				return "", nil, fmt.Errorf("document %s was created but its text could not be written: %w",
					created.DocumentID, err)
			}
		}
		return fmt.Sprintf("Created %q (id %s).", created.Title, created.DocumentID),
			map[string]any{"document_id": created.DocumentID, "title": created.Title}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "get_doc_content",
		Description: "Read a Google Doc as plain text, including the text inside tables.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args getDocArgs) (string, any, error) {
		if strings.TrimSpace(args.DocumentID) == "" {
			return "", nil, fmt.Errorf("document_id is required")
		}
		var doc docsDocument
		if err := c.get(ctx, docsBase+"/documents/"+url.PathEscape(args.DocumentID), nil, &doc); err != nil {
			return "", nil, err
		}
		text := docText(doc.Body.Content)
		return fmt.Sprintf("%s\n\n%s", doc.Title, truncate(text, 40000)),
			map[string]any{"document_id": doc.DocumentID, "title": doc.Title, "content": text}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "insert_doc_text",
		Description: "Insert text into a Google Doc, at a position or appended to the end.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args insertDocTextArgs) (string, any, error) {
		if strings.TrimSpace(args.DocumentID) == "" || args.Text == "" {
			return "", nil, fmt.Errorf("document_id and text are required")
		}
		location := map[string]any{"index": 1}
		if args.Index > 0 {
			location["index"] = args.Index
		} else {
			// Appending means finding the end, and the end is the last
			// element's endIndex minus one: Docs counts a trailing newline
			// that cannot be written past.
			var doc docsDocument
			if err := c.get(ctx, docsBase+"/documents/"+url.PathEscape(args.DocumentID), nil, &doc); err != nil {
				return "", nil, err
			}
			if end := docEndIndex(doc.Body.Content); end > 1 {
				location["index"] = end
			}
		}
		requests := []map[string]any{{
			"insertText": map[string]any{"location": location, "text": args.Text},
		}}
		if err := c.post(ctx, docsBase+"/documents/"+url.PathEscape(args.DocumentID)+":batchUpdate",
			map[string]any{"requests": requests}, nil); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Inserted %d character(s) into %s at index %v.",
				len(args.Text), args.DocumentID, location["index"]),
			map[string]any{"document_id": args.DocumentID, "inserted": len(args.Text)}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "find_and_replace_doc",
		Description: "Replace every occurrence of some text in a Google Doc.",
		Annotations: mutating(true),
	}, func(ctx context.Context, args findReplaceDocArgs) (string, any, error) {
		if strings.TrimSpace(args.DocumentID) == "" || args.Find == "" {
			return "", nil, fmt.Errorf("document_id and find are required")
		}
		requests := []map[string]any{{
			"replaceAllText": map[string]any{
				"containsText": map[string]any{"text": args.Find, "matchCase": args.MatchCase},
				"replaceText":  args.Replace,
			},
		}}
		var out struct {
			Replies []struct {
				ReplaceAllText struct {
					OccurrencesChanged int `json:"occurrencesChanged"`
				} `json:"replaceAllText"`
			} `json:"replies"`
		}
		if err := c.post(ctx, docsBase+"/documents/"+url.PathEscape(args.DocumentID)+":batchUpdate",
			map[string]any{"requests": requests}, &out); err != nil {
			return "", nil, err
		}
		changed := 0
		if len(out.Replies) > 0 {
			changed = out.Replies[0].ReplaceAllText.OccurrencesChanged
		}
		return fmt.Sprintf("Replaced %d occurrence(s) of %q in %s.", changed, args.Find, args.DocumentID),
			map[string]any{"occurrences_changed": changed}, nil
	})
}

// docText walks the structural elements and joins their runs, descending into
// table cells so a document's tables are not silently dropped.
func docText(content []docsStructuralElement) string {
	var out strings.Builder
	var walk func(elements []docsStructuralElement)
	walk = func(elements []docsStructuralElement) {
		for _, element := range elements {
			if element.Paragraph != nil {
				for _, run := range element.Paragraph.Elements {
					if run.TextRun != nil {
						out.WriteString(run.TextRun.Content)
					}
				}
			}
			if element.Table != nil {
				for _, row := range element.Table.TableRows {
					for _, cell := range row.TableCells {
						walk(cell.Content)
					}
				}
			}
		}
	}
	walk(content)
	return out.String()
}

// docEndIndex is the position just past the last character, which is where an
// append has to go.
func docEndIndex(content []docsStructuralElement) int {
	end := 0
	for _, element := range content {
		if element.EndIndex > end {
			end = element.EndIndex
		}
	}
	if end > 1 {
		// Docs will not accept an insert at the very final newline.
		return end - 1
	}
	return 1
}
