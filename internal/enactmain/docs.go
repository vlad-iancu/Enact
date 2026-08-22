package enactmain

import (
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	restful "github.com/emicklei/go-restful/v3"

	appdocs "enact/docs"
	"enact/internal/requesthelper"
)

// docPage is one documentation page.
type docPage struct {
	// Slug addresses the page: GET /docs/{slug}.
	Slug string `json:"slug"`
	// Title is the page's own "# " heading, used as the navigation label.
	Title string `json:"title"`
	// Order is the numeric prefix of the file name, which is how navigation
	// is sequenced. Not part of the slug: "01-getting-started.md" is served
	// as "getting-started", so renumbering the menu never changes a URL.
	Order int `json:"order"`

	// markdown is the file verbatim, including the heading — the frontend
	// renders the whole page and should not have to re-attach a title.
	markdown string
}

// docPrefix matches the ordering prefix: "01-getting-started" -> 1, slug
// "getting-started".
var docPrefix = regexp.MustCompile(`^(\d+)[-_](.+)$`)

// loadDocs reads every embedded page and fails if one breaks the rule that a
// page begins with its title.
//
// Failing at startup rather than skipping the file is deliberate. The pages
// are compiled into the binary, so a missing heading is a mistake that exists
// from the moment it is committed and shows up the first time anyone runs the
// service — whereas quietly dropping the page from navigation would surface
// as "where did that document go" much later, in a deployment.
func loadDocs() ([]docPage, error) {
	entries, err := fs.ReadDir(appdocs.FS, "app")
	if err != nil {
		return nil, fmt.Errorf("read embedded documentation: %w", err)
	}
	pages := make([]docPage, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		raw, err := fs.ReadFile(appdocs.FS, path.Join("app", name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		title, ok := docTitle(string(raw))
		if !ok {
			return nil, fmt.Errorf(
				"docs/app/%s does not begin with a %q heading, which is what names it in the navigation", name, "# ")
		}
		slug := strings.TrimSuffix(name, ".md")
		order := 0
		if m := docPrefix.FindStringSubmatch(slug); m != nil {
			order, _ = strconv.Atoi(m[1])
			slug = m[2]
		}
		pages = append(pages, docPage{Slug: slug, Title: title, Order: order, markdown: string(raw)})
	}
	// Ordered by prefix, then by slug so unnumbered pages are still stable
	// rather than following directory order.
	sort.SliceStable(pages, func(i, j int) bool {
		if pages[i].Order != pages[j].Order {
			return pages[i].Order < pages[j].Order
		}
		return pages[i].Slug < pages[j].Slug
	})
	return pages, nil
}

// docTitle returns the text of the leading "# " heading.
//
// Blank lines before it are tolerated; anything else is not. A page whose
// first content is a paragraph has no navigation label, and guessing one from
// the file name would hide the mistake.
func docTitle(markdown string) (string, bool) {
	for _, line := range strings.Split(markdown, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "# ")), true
		}
		return "", false
	}
	return "", false
}

type docsResponse struct {
	Documents []docPage `json:"documents"`
}

// docsWebService serves the product documentation.
//
// No session filter: documentation describes the product, not a user's data,
// and someone deciding whether to sign up — or stuck at the "you need an
// organization" step — is exactly who needs to read it.
func (a *MainAPI) docsWebService() *restful.WebService {
	ws := new(restful.WebService)
	ws.Path("/docs").Produces(restful.MIME_JSON)

	ws.Route(ws.GET("").
		To(a.listDocs).
		Doc("List the documentation pages in navigation order; each title is the page's own heading").
		Returns(http.StatusOK, "OK", docsResponse{}))

	ws.Route(ws.GET("/{slug}").
		To(a.getDoc).
		Param(ws.PathParameter("slug", "page slug, as returned by GET /docs")).
		Produces("text/markdown").
		Doc("Fetch one page as markdown, heading included, for the client to render").
		Returns(http.StatusOK, "OK", "").
		Returns(http.StatusNotFound, "No such page", errorResponse{}))

	return ws
}

func (a *MainAPI) listDocs(req *restful.Request, resp *restful.Response) {
	requesthelper.Logger(req, a.logger).Info("documentation listed", "pages", len(a.docs))
	// Copied so the response cannot alias the loaded pages.
	out := make([]docPage, len(a.docs))
	copy(out, a.docs)
	requesthelper.WriteJSON(req, resp, http.StatusOK, docsResponse{Documents: out})
}

func (a *MainAPI) getDoc(req *restful.Request, resp *restful.Response) {
	slug := req.PathParameter("slug")
	logger := requesthelper.Logger(req, a.logger).WithFields("slug", slug)
	for _, page := range a.docs {
		if page.Slug != slug {
			continue
		}
		logger.Info("documentation page served", "title", page.Title, "bytes", len(page.markdown))
		// Markdown verbatim, not JSON-wrapped: the client renders it, and a
		// wrapper would only mean unwrapping it again.
		resp.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		resp.WriteHeader(http.StatusOK)
		if _, err := resp.Write([]byte(page.markdown)); err != nil {
			logger.Warn("failed to write documentation page", "err", err)
		}
		return
	}
	logger.Warn("no such documentation page")
	requesthelper.WriteError(req, resp, http.StatusNotFound, fmt.Sprintf("no documentation page %q", slug))
}
