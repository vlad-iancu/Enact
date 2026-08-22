package googleworkspace

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// driveFile is the subset of Drive's file resource these tools report.
type driveFile struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MimeType     string   `json:"mimeType"`
	Size         string   `json:"size,omitempty"`
	ModifiedTime string   `json:"modifiedTime,omitempty"`
	WebViewLink  string   `json:"webViewLink,omitempty"`
	Parents      []string `json:"parents,omitempty"`
}

// driveFileFields is what to ask for. Drive returns almost nothing unless
// asked, and asking for everything is slow on large folders.
const driveFileFields = "files(id,name,mimeType,size,modifiedTime,webViewLink,parents),nextPageToken"

// exportFormats maps a Google-native type to the export Drive can produce.
//
// Native documents have no bytes to download — `alt=media` fails on them, and
// `files/{id}/export` is the only way to read one. This is the distinction the
// Python reference handles in drive_helpers.py and the one most easily missed.
var exportFormats = map[string]string{
	"application/vnd.google-apps.document":     "text/plain",
	"application/vnd.google-apps.spreadsheet":  "text/csv",
	"application/vnd.google-apps.presentation": "text/plain",
}

// maxFileBytes caps what a tool will pull into a model's context.
const maxFileBytes = 512 << 10

type searchDriveArgs struct {
	Query      string `json:"query" jsonschema:"what to look for; plain words match file names and content, or use Drive query syntax such as \"mimeType='application/pdf'\""`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"how many files to return (default 20, max 100)"`
}

type listDriveItemsArgs struct {
	FolderID   string `json:"folder_id,omitempty" jsonschema:"the folder to list (default 'root')"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"how many items to return (default 50, max 200)"`
}

type getDriveContentArgs struct {
	FileID string `json:"file_id" jsonschema:"the file to read, as returned by search_drive_files"`
}

type createDriveFileArgs struct {
	Name     string `json:"name" jsonschema:"the file name"`
	Content  string `json:"content,omitempty" jsonschema:"plain-text contents; omit to create an empty file"`
	FolderID string `json:"folder_id,omitempty" jsonschema:"the parent folder (default 'root')"`
	MimeType string `json:"mime_type,omitempty" jsonschema:"the MIME type (default text/plain). Use application/vnd.google-apps.folder to create a folder"`
}

func registerDrive(server *mcp.Server, c *client) {
	addTool(server, &mcp.Tool{
		Name:        "search_drive_files",
		Description: "Search the user's Google Drive by name and content.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args searchDriveArgs) (string, any, error) {
		if strings.TrimSpace(args.Query) == "" {
			return "", nil, fmt.Errorf("query is required")
		}
		max := clamp(args.MaxResults, 20, 100)
		// A bare phrase is turned into a Drive query; anything already
		// containing an operator is passed through as the caller wrote it.
		q := args.Query
		if !strings.ContainsAny(q, "=<>") && !strings.Contains(q, " contains ") {
			q = fmt.Sprintf("fullText contains '%s' and trashed = false", escapeDriveLiteral(q))
		}
		query := url.Values{}
		query.Set("q", q)
		query.Set("pageSize", strconv.Itoa(max))
		query.Set("fields", driveFileFields)

		var listing struct {
			Files []driveFile `json:"files"`
		}
		if err := c.get(ctx, driveBase+"/files", query, &listing); err != nil {
			return "", nil, err
		}
		return renderDriveFiles(fmt.Sprintf("%d file(s) matching %q", len(listing.Files), args.Query), listing.Files),
			map[string]any{"files": listing.Files}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "list_drive_items",
		Description: "List what is directly inside a Drive folder.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args listDriveItemsArgs) (string, any, error) {
		folder := defaultString(args.FolderID, "root")
		max := clamp(args.MaxResults, 50, 200)
		query := url.Values{}
		query.Set("q", fmt.Sprintf("'%s' in parents and trashed = false", escapeDriveLiteral(folder)))
		query.Set("pageSize", strconv.Itoa(max))
		query.Set("fields", driveFileFields)

		var listing struct {
			Files []driveFile `json:"files"`
		}
		if err := c.get(ctx, driveBase+"/files", query, &listing); err != nil {
			return "", nil, err
		}
		return renderDriveFiles(fmt.Sprintf("%d item(s) in folder %s", len(listing.Files), folder), listing.Files),
			map[string]any{"files": listing.Files, "folder_id": folder}, nil
	})

	addTool(server, &mcp.Tool{
		Name: "get_drive_file_content",
		Description: "Read a Drive file's text. Google Docs, Sheets and Slides are exported to text; " +
			"other files are downloaded as-is and returned only if they are textual.",
		Annotations: readOnly(),
	}, func(ctx context.Context, args getDriveContentArgs) (string, any, error) {
		if strings.TrimSpace(args.FileID) == "" {
			return "", nil, fmt.Errorf("file_id is required")
		}
		meta := url.Values{}
		meta.Set("fields", "id,name,mimeType,size,modifiedTime,webViewLink")
		var file driveFile
		if err := c.get(ctx, driveBase+"/files/"+url.PathEscape(args.FileID), meta, &file); err != nil {
			return "", nil, err
		}

		var payload []byte
		var err error
		if format, native := exportFormats[file.MimeType]; native {
			query := url.Values{}
			query.Set("mimeType", format)
			payload, err = c.raw(ctx, driveBase+"/files/"+url.PathEscape(args.FileID)+"/export", query, maxFileBytes)
		} else {
			query := url.Values{}
			query.Set("alt", "media")
			payload, err = c.raw(ctx, driveBase+"/files/"+url.PathEscape(args.FileID), query, maxFileBytes)
		}
		if err != nil {
			return "", nil, err
		}
		text := string(payload)
		if !looksTextual(text) {
			return fmt.Sprintf("%s (%s) is not a text file; %d bytes were not returned.",
					file.Name, file.MimeType, len(payload)),
				map[string]any{"file": file, "binary": true}, nil
		}
		return fmt.Sprintf("%s (%s)\n\n%s", file.Name, file.MimeType, truncate(text, 40000)),
			map[string]any{"file": file, "content": text}, nil
	})

	addTool(server, &mcp.Tool{
		Name:        "create_drive_file",
		Description: "Create a file or folder in Drive with optional text contents.",
		Annotations: mutating(false),
	}, func(ctx context.Context, args createDriveFileArgs) (string, any, error) {
		if strings.TrimSpace(args.Name) == "" {
			return "", nil, fmt.Errorf("name is required")
		}
		mimeType := defaultString(args.MimeType, "text/plain")
		metadata := map[string]any{"name": args.Name, "mimeType": mimeType}
		if folder := strings.TrimSpace(args.FolderID); folder != "" {
			metadata["parents"] = []string{folder}
		}
		// Metadata-only create, then a content upload if there is any. Two
		// calls instead of a multipart body: simpler, and folders (which
		// cannot carry content) take the same path.
		var created driveFile
		if err := c.post(ctx, driveBase+"/files?fields=id,name,mimeType,webViewLink", metadata, &created); err != nil {
			return "", nil, err
		}
		if args.Content != "" && mimeType != "application/vnd.google-apps.folder" {
			upload := "https://www.googleapis.com/upload/drive/v3/files/" + url.PathEscape(created.ID) + "?uploadType=media"
			if err := c.uploadText(ctx, upload, mimeType, args.Content); err != nil {
				return "", nil, fmt.Errorf("file %s was created but its contents could not be written: %w", created.ID, err)
			}
		}
		return fmt.Sprintf("Created %s (id %s, %s).", created.Name, created.ID, created.MimeType), created, nil
	})
}

// renderDriveFiles is the shared listing summary.
func renderDriveFiles(heading string, files []driveFile) string {
	var out strings.Builder
	out.WriteString(heading)
	if len(files) == 0 {
		out.WriteString(".")
		return out.String()
	}
	out.WriteString(":\n")
	for _, f := range files {
		fmt.Fprintf(&out, "- %s (id %s, %s%s)\n", f.Name, f.ID, f.MimeType, optional(", modified ", f.ModifiedTime))
	}
	return out.String()
}

// escapeDriveLiteral escapes a value going into a Drive query string literal,
// so a name containing a quote cannot change the query's meaning.
func escapeDriveLiteral(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `'`, `\'`)
}

// looksTextual reports whether a payload can go into a model's context. A NUL
// byte is the cheap, reliable tell for binary.
func looksTextual(s string) bool {
	if strings.IndexByte(s, 0) >= 0 {
		return false
	}
	return true
}

func clamp(value, fallback, max int) int {
	if value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}
