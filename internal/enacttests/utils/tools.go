package utils

// ToolRegistryAudience is the tool registry's S2S key id.
const ToolRegistryAudience = "enact-tool-registry"

// ToolRegistryURL builds a URL against the tool registry service.
func (t *T) ToolRegistryURL(path string) string { return t.Env.ToolRegistryURL + path }
