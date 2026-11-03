package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Only match observed upstream tool queries, never answer text or citation
// domains. Token-overlap heuristics reject legitimate translated searches.
func isPlaceholderSearchQuery(query string) bool {
	switch strings.Join(strings.Fields(strings.ToLower(query)), " ") {
	case "current information and recent sources",
		"best expert recommendations and evidence",
		"latest updates and authoritative references":
		return true
	}
	return false
}

type degradedSearchError struct {
	query     string
	frame     []byte
	truncated bool
}

func (e *degradedSearchError) Error() string {
	return fmt.Sprintf("%s: query=%q", provider.ErrWebSearchPlaceholder, e.query)
}

func (e *degradedSearchError) Unwrap() error { return provider.ErrWebSearchPlaceholder }

// The HTTP exchange really succeeded; preserve its 200 and the offending
// frame in diagnostics rather than inventing an upstream 502 response.
func (e *degradedSearchError) UpstreamDiagnostic() (string, provider.DiagnosticResponse) {
	return "", provider.DiagnosticResponse{
		StatusCode: http.StatusOK, Status: "200 OK", Body: e.frame, BodyTruncated: e.truncated,
	}
}
