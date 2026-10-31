package proxy

import (
	"net/http"
	"regexp"
)

// readonlyPath is the §9 allowlist with an optional /vX.Y prefix.
var readonlyPath = regexp.MustCompile(`^(/v[0-9.]+)?/(_ping|version|events|containers/json|containers/[^/]+/json)$`)

// Allowed reports whether a request may pass in readonly mode.
func Allowed(method, path string) bool {
	return (method == http.MethodGet || method == http.MethodHead) && readonlyPath.MatchString(path)
}
