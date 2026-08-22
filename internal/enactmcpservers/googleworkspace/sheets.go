package googleworkspace

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type spreadsheetProperties struct {
	Title string `json:"title"`
}

type sheetProperties struct {
	SheetID int    `json:"sheetId"`
	Title   string `json:"title"`
	Index   int    `json:"index"`
}

type listSpreadsheetsArgs struct {
	MaxResults int `json:"max_results,omitempty" jsonschema:"how many spreadsheets to return (default 20, max 100)"`
}

type createSpreadsheetArgs struct {
	Title      string   `json:"title" jsonschema:"the spreadsheet title"`
	SheetNames []string `json:"sheet_names,omitempty" jsonschema:"names for the initial sheets; defaults to one called Sheet1"`
}

type getSpreadsheetInfoArgs struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"the spreadsheet to describe"`
}

type readValuesArgs struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"the spreadsheet to read"`
	Range         string `json:"range" jsonschema:"an A1 range such as 'Sheet1!A1:D20', or just a sheet name for everything on it"`
}

type modifyValuesArgs struct {
	SpreadsheetID string     `json:"spreadsheet_id" jsonschema:"the spreadsheet to write to"`
	Range         string     `json:"range" jsonschema:"an A1 range such as 'Sheet1!A1:C3'; its size should match the values given"`
	Values        [][]string `json:"values" jsonschema:"rows of cell values, outer list rows and inner list columns"`
	Append        bool       `json:"append,omitempty" jsonschema:"append after the last row of the range instead of overwriting it"`
}

type createSheetArgs struct {
	SpreadsheetID string `json:"spreadsheet_id" jsonschema:"the spreadsheet to add a sheet to"`
	Title         string `json:"title" jsonschema:"the new sheet's name"`
}

func registerSheets(server *mcp.Server, c *client) {
	addTool(server, &mcp.Tool{
		Name: "list_spreadsheets",
		Description: "List the user's Google Sheets. This searches Drive, because the Sheets API " +
			"can only address a spreadsheet already known by id.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args listSpreadsheetsArgs) (string, any, error) {
		max := clamp(args.MaxResults, 20, 100)
		query := url.Values{}
		query.Set("q", "mimeType='application/vnd.google-apps.spreadsheet' and trashed = false")
		query.Set("pageSize", strconv.Itoa(max))
		query.Set("fields", driveFileFields)
		var listing struct {
			Files []driveFile `json:"files"`
		}
		if err := c.get(ctx, driveBase+"/files", query, &listing); err != nil {
			return "", nil, err
		}
		return renderDriveFiles(fmt.Sprintf("%d spreadsheet(s)", len(listing.Files)), listing.Files),
			map[string]any{"spreadsheets": listing.Files}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "create_spreadsheet",
		Description: "Create a Google Sheets spreadsheet.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args createSpreadsheetArgs) (string, any, error) {
		if strings.TrimSpace(args.Title) == "" {
			return "", nil, fmt.Errorf("title is required")
		}
		body := map[string]any{"properties": spreadsheetProperties{Title: args.Title}}
		if len(args.SheetNames) > 0 {
			sheets := make([]map[string]any, 0, len(args.SheetNames))
			for _, name := range args.SheetNames {
				sheets = append(sheets, map[string]any{"properties": map[string]any{"title": name}})
			}
			body["sheets"] = sheets
		}
		var created struct {
			SpreadsheetID  string `json:"spreadsheetId"`
			SpreadsheetURL string `json:"spreadsheetUrl"`
			Properties     spreadsheetProperties
		}
		if err := c.post(ctx, sheetsBase+"/spreadsheets", body, &created); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Created %q (id %s).", args.Title, created.SpreadsheetID),
			map[string]any{"spreadsheet_id": created.SpreadsheetID, "url": created.SpreadsheetURL}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "get_spreadsheet_info",
		Description: "Describe a spreadsheet: its title and the sheets it contains, with their names and ids.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args getSpreadsheetInfoArgs) (string, any, error) {
		if strings.TrimSpace(args.SpreadsheetID) == "" {
			return "", nil, fmt.Errorf("spreadsheet_id is required")
		}
		var info struct {
			SpreadsheetID string                `json:"spreadsheetId"`
			Properties    spreadsheetProperties `json:"properties"`
			Sheets        []struct {
				Properties sheetProperties `json:"properties"`
			} `json:"sheets"`
		}
		query := url.Values{}
		query.Set("fields", "spreadsheetId,properties.title,sheets.properties")
		if err := c.get(ctx, sheetsBase+"/spreadsheets/"+url.PathEscape(args.SpreadsheetID), query, &info); err != nil {
			return "", nil, err
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "%s (id %s) has %d sheet(s):\n", info.Properties.Title, info.SpreadsheetID, len(info.Sheets))
		sheets := make([]sheetProperties, 0, len(info.Sheets))
		for _, s := range info.Sheets {
			sheets = append(sheets, s.Properties)
			fmt.Fprintf(&summary, "- %s (sheet id %d)\n", s.Properties.Title, s.Properties.SheetID)
		}
		return summary.String(),
			map[string]any{"spreadsheet_id": info.SpreadsheetID, "title": info.Properties.Title, "sheets": sheets}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "read_sheet_values",
		Description: "Read a range of cells and return them as rows.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args readValuesArgs) (string, any, error) {
		if strings.TrimSpace(args.SpreadsheetID) == "" || strings.TrimSpace(args.Range) == "" {
			return "", nil, fmt.Errorf("spreadsheet_id and range are required")
		}
		var out struct {
			Range  string     `json:"range"`
			Values [][]string `json:"values"`
		}
		endpoint := sheetsBase + "/spreadsheets/" + url.PathEscape(args.SpreadsheetID) +
			"/values/" + url.PathEscape(args.Range)
		if err := c.get(ctx, endpoint, nil, &out); err != nil {
			return "", nil, err
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "%s — %d row(s):\n", out.Range, len(out.Values))
		for i, row := range out.Values {
			if i >= 200 {
				fmt.Fprintf(&summary, "… %d more row(s)\n", len(out.Values)-i)
				break
			}
			fmt.Fprintf(&summary, "%s\n", strings.Join(row, "\t"))
		}
		return summary.String(), map[string]any{"range": out.Range, "values": out.Values}, nil
	})

	addTool(server, &mcp.Tool{
		Name: "modify_sheet_values",
		Description: "Write cell values. By default this OVERWRITES the range; " +
			"set append to add rows after the existing data instead.",
		Annotations: mutating(true),
	}, func(ctx context.Context, args modifyValuesArgs) (string, any, error) {
		if strings.TrimSpace(args.SpreadsheetID) == "" || strings.TrimSpace(args.Range) == "" {
			return "", nil, fmt.Errorf("spreadsheet_id and range are required")
		}
		if len(args.Values) == 0 {
			return "", nil, fmt.Errorf("values is required; give at least one row")
		}
		endpoint := sheetsBase + "/spreadsheets/" + url.PathEscape(args.SpreadsheetID) +
			"/values/" + url.PathEscape(args.Range)
		body := map[string]any{"values": args.Values}

		// USER_ENTERED so "=SUM(A1:A9)" becomes a formula and "5" a number,
		// which is what someone writing a spreadsheet means. RAW would store
		// both as text.
		if args.Append {
			endpoint += ":append?valueInputOption=USER_ENTERED&insertDataOption=INSERT_ROWS"
			var out struct {
				Updates struct {
					UpdatedRange string `json:"updatedRange"`
					UpdatedRows  int    `json:"updatedRows"`
					UpdatedCells int    `json:"updatedCells"`
				} `json:"updates"`
			}
			if err := c.post(ctx, endpoint, body, &out); err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("Appended %d row(s) to %s (%d cells).",
					out.Updates.UpdatedRows, out.Updates.UpdatedRange, out.Updates.UpdatedCells),
				out.Updates, nil
		}
		endpoint += "?valueInputOption=USER_ENTERED"
		var out struct {
			UpdatedRange string `json:"updatedRange"`
			UpdatedRows  int    `json:"updatedRows"`
			UpdatedCells int    `json:"updatedCells"`
		}
		if err := c.put(ctx, endpoint, body, &out); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Wrote %d cell(s) across %d row(s) in %s.",
			out.UpdatedCells, out.UpdatedRows, out.UpdatedRange), out, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "create_sheet",
		Description: "Add a new sheet (tab) to an existing spreadsheet.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args createSheetArgs) (string, any, error) {
		if strings.TrimSpace(args.SpreadsheetID) == "" || strings.TrimSpace(args.Title) == "" {
			return "", nil, fmt.Errorf("spreadsheet_id and title are required")
		}
		requests := []map[string]any{{
			"addSheet": map[string]any{"properties": map[string]any{"title": args.Title}},
		}}
		var out struct {
			Replies []struct {
				AddSheet struct {
					Properties sheetProperties `json:"properties"`
				} `json:"addSheet"`
			} `json:"replies"`
		}
		endpoint := sheetsBase + "/spreadsheets/" + url.PathEscape(args.SpreadsheetID) + ":batchUpdate"
		if err := c.post(ctx, endpoint, map[string]any{"requests": requests}, &out); err != nil {
			return "", nil, err
		}
		created := sheetProperties{Title: args.Title}
		if len(out.Replies) > 0 {
			created = out.Replies[0].AddSheet.Properties
		}
		return fmt.Sprintf("Added sheet %q (sheet id %d).", created.Title, created.SheetID), created, nil
	})
}
