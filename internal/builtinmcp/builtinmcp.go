// Package builtinmcp names the MCP servers the platform hosts itself.
//
// It is a domain package rather than part of either service because two need
// it and services do not import each other (ADR-0009): enact-mcp-servers
// mounts these paths, and enact-main tells the browser where they are so a
// person never has to type an internal hostname.
//
// The list is code, not data — the same call as the model catalogue in
// internal/models. A built-in server ships with the binary; there is nothing
// to configure and nothing to migrate.
package builtinmcp

import "strings"

// AliasHost is the hostname a built-in server is registered under.
//
// It is deliberately NOT a real address. In development nothing resolves it;
// in the cluster DNS resolves the name but not the port. The tool registry
// maps it to wherever enact-mcp-servers actually is, at dial time — which is
// what keeps a deployment detail out of a user-visible field and out of the
// stored record, and lets one record work in every environment.
//
// Nothing outside the registry should dial a URL built on it.
const AliasHost = "enact-mcp-servers"

// AliasURL is the base a built-in server is registered at. No port: supplying
// one is the problem this exists to remove.
const AliasURL = "http://" + AliasHost

// Server describes one built-in MCP server.
type Server struct {
	// ID is the stable identifier, and the sensible default for the id an
	// organization registers it under.
	ID string `json:"id"`
	// Name and Description are for the person choosing.
	Name        string `json:"name"`
	Description string `json:"description"`
	// Path is where enact-mcp-servers mounts it. Callers join it to that
	// service's base URL — see URL.
	Path string `json:"-"`
	// TransportType is what to register it as. Unlike a credential, this is
	// a property of the server itself and the same for everyone.
	TransportType string `json:"transport_type"`
}

// Deliberately NOT here: which provider to use.
//
// A provider is named by the organization that registered it — one calls it
// "gmail", another "google-workspace", a third "google" — and a catalogue
// compiled into the binary cannot know any of them. Publishing a name here
// would either mean nothing or, worse, be prefilled into a form and refer to
// a provider that organization does not have.
//
// The organization already knows its own providers: the user picks one when
// they register the server, and the tool_access_requirements they attach are
// what bind the two together.

// URL is where a server is reachable, given the base URL of the service that
// hosts it.
//
// The base comes from configuration, exactly like every other service
// address, so the deployment's topology stays in the deployment: localhost in
// development, a compose hostname in the cluster, and neither of them
// something a user is asked to know.
func (s Server) URL(baseURL string) string {
	return strings.TrimRight(baseURL, "/") + s.Path
}

// servers is everything the platform hosts.
var servers = []Server{
	{
		ID:   "google-workspace",
		Name: "Google Workspace",
		Description: "Gmail, Google Calendar, Drive, Docs, Sheets and Slides. " +
			"Acts as the user who runs it, using their connected Google account.",
		Path:          "/mcp-servers/google-workspace/mcp",
		TransportType: "streamable-http",
	},
}

// Servers returns the built-in servers.
func Servers() []Server {
	out := make([]Server, len(servers))
	copy(out, servers)
	return out
}
