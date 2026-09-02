package crawls

import (
	"fmt"

	"github.com/andybalholm/cascadia"
)

// CheckSelector reports whether a CSS selector compiles.
//
// It lives in the domain package so the API can reject a bad selector at
// creation, using exactly the compiler the crawler will later run — a selector
// that validates here and fails there would be the worst of both worlds.
func CheckSelector(selector string) error {
	if _, err := cascadia.Compile(selector); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
