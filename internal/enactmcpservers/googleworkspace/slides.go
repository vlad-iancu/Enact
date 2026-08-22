package googleworkspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A presentation's text lives on shapes inside page elements, the same
// runs-not-strings shape as Docs.
type slidesPresentation struct {
	PresentationID string       `json:"presentationId"`
	Title          string       `json:"title"`
	Slides         []slidesPage `json:"slides"`
}

type slidesPage struct {
	ObjectID     string `json:"objectId"`
	PageElements []struct {
		ObjectID string `json:"objectId"`
		Shape    *struct {
			Text *struct {
				TextElements []struct {
					TextRun *struct {
						Content string `json:"content"`
					} `json:"textRun"`
				} `json:"textElements"`
			} `json:"text"`
		} `json:"shape"`
	} `json:"pageElements"`
}

type createPresentationArgs struct {
	Title string `json:"title" jsonschema:"the presentation title"`
}

type getPresentationArgs struct {
	PresentationID string `json:"presentation_id" jsonschema:"the presentation to read"`
}

type batchUpdatePresentationArgs struct {
	PresentationID string `json:"presentation_id" jsonschema:"the presentation to change"`
	Requests       string `json:"requests" jsonschema:"a JSON array of Slides API request objects, e.g. [{\"createSlide\":{}}] or [{\"insertText\":{\"objectId\":\"...\",\"text\":\"...\"}}]"`
}

func registerSlides(server *mcp.Server, c *client) {
	addTool(server, &mcp.Tool{
		Name:        "create_presentation",
		Description: "Create a Google Slides presentation.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args createPresentationArgs) (string, any, error) {
		if strings.TrimSpace(args.Title) == "" {
			return "", nil, fmt.Errorf("title is required")
		}
		var created slidesPresentation
		if err := c.post(ctx, slidesBase+"/presentations", map[string]any{"title": args.Title}, &created); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Created %q (id %s) with %d slide(s).",
				created.Title, created.PresentationID, len(created.Slides)),
			map[string]any{"presentation_id": created.PresentationID, "title": created.Title}, nil
	})

	addTool(server, &mcp.Tool{
		Name: "get_presentation",
		Description: "Read a presentation: its title, its slides' ids, and the text on each — " +
			"the object ids are what batch_update_presentation needs to target.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args getPresentationArgs) (string, any, error) {
		if strings.TrimSpace(args.PresentationID) == "" {
			return "", nil, fmt.Errorf("presentation_id is required")
		}
		var presentation slidesPresentation
		if err := c.get(ctx, slidesBase+"/presentations/"+url.PathEscape(args.PresentationID), nil, &presentation); err != nil {
			return "", nil, err
		}
		type slideSummary struct {
			ObjectID string `json:"object_id"`
			Text     string `json:"text"`
		}
		slides := make([]slideSummary, 0, len(presentation.Slides))
		var summary strings.Builder
		fmt.Fprintf(&summary, "%s (id %s), %d slide(s):\n",
			presentation.Title, presentation.PresentationID, len(presentation.Slides))
		for i, page := range presentation.Slides {
			text := slideText(page)
			slides = append(slides, slideSummary{ObjectID: page.ObjectID, Text: text})
			fmt.Fprintf(&summary, "\n[%d] slide %s\n%s\n", i+1, page.ObjectID, truncate(text, 2000))
		}
		return summary.String(), map[string]any{
			"presentation_id": presentation.PresentationID,
			"title":           presentation.Title,
			"slides":          slides,
		}, nil
	})

	addTool(server, &mcp.Tool{
		Name: "batch_update_presentation",
		Description: "Apply Slides API requests to a presentation — creating slides, inserting text, " +
			"deleting objects. Requests are given as a JSON array, exactly as the Slides API defines them.",
		Annotations: mutating(true),
	}, func(ctx context.Context, args batchUpdatePresentationArgs) (string, any, error) {
		if strings.TrimSpace(args.PresentationID) == "" {
			return "", nil, fmt.Errorf("presentation_id is required")
		}
		// The requests arrive as a JSON string rather than a typed argument:
		// the Slides request union has dozens of members, and reproducing it
		// in a schema would be a worse guide for the model than the API's own
		// documentation, which it already knows.
		var requests []map[string]any
		if err := json.Unmarshal([]byte(args.Requests), &requests); err != nil {
			return "", nil, fmt.Errorf("requests must be a JSON array of Slides API requests: %w", err)
		}
		if len(requests) == 0 {
			return "", nil, fmt.Errorf("requests is empty; give at least one Slides API request")
		}
		var out struct {
			Replies []map[string]any `json:"replies"`
		}
		endpoint := slidesBase + "/presentations/" + url.PathEscape(args.PresentationID) + ":batchUpdate"
		if err := c.post(ctx, endpoint, map[string]any{"requests": requests}, &out); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Applied %d request(s) to %s.", len(requests), args.PresentationID),
			map[string]any{"replies": out.Replies}, nil
	})
}

// slideText gathers the text runs on one slide.
func slideText(page slidesPage) string {
	var out strings.Builder
	for _, element := range page.PageElements {
		if element.Shape == nil || element.Shape.Text == nil {
			continue
		}
		for _, textElement := range element.Shape.Text.TextElements {
			if textElement.TextRun != nil {
				out.WriteString(textElement.TextRun.Content)
			}
		}
	}
	return strings.TrimSpace(out.String())
}
