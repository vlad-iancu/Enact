package requesthelper

import (
	"fmt"
	"io"
	"net/http"

	restful "github.com/emicklei/go-restful/v3"
)

// formParseMemory is how much of a parsed multipart form is held in memory
// before spilling to temp files (the request-wide cap is maxBytes below).
const formParseMemory = 32 << 20

// UploadedFile is one file received in a multipart upload, fully read.
type UploadedFile struct {
	Filename string
	Content  []byte
}

// ReadUploadedFiles parses the request's multipart form and returns EVERY
// file uploaded under the given field, each validated to be non-empty. This
// exists because http.Request.FormFile silently returns only the first part
// of a field, which loses all but one file of a multi-file upload.
//
// The request body is capped at maxBytes across all parts. On failure it
// returns a non-zero HTTP status and a client-facing message; the caller is
// expected to write those and stop.
func ReadUploadedFiles(req *restful.Request, resp *restful.Response, field string, maxBytes int64) ([]UploadedFile, int, string) {
	r := req.Request
	r.Body = http.MaxBytesReader(resp.ResponseWriter, r.Body, maxBytes)

	if err := r.ParseMultipartForm(formParseMemory); err != nil {
		return nil, http.StatusBadRequest, fmt.Sprintf("invalid multipart form: %v", err)
	}
	headers := r.MultipartForm.File[field]
	if len(headers) == 0 {
		return nil, http.StatusBadRequest, fmt.Sprintf("missing %q file part", field)
	}

	files := make([]UploadedFile, 0, len(headers))
	for _, header := range headers {
		file, err := header.Open()
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Sprintf("failed to open uploaded file %q: %v", header.Filename, err)
		}
		content, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil {
			return nil, http.StatusBadRequest, fmt.Sprintf("failed to read uploaded file %q: %v", header.Filename, err)
		}
		if len(content) == 0 {
			return nil, http.StatusBadRequest, fmt.Sprintf("uploaded file %q must not be empty", header.Filename)
		}
		files = append(files, UploadedFile{Filename: header.Filename, Content: content})
	}
	return files, 0, ""
}
