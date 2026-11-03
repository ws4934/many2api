package inference

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/gin-gonic/gin"
)

func TestSearchDegradedErrorsPreserveAPIProtocolsAndChineseMessage(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		writer := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(writer)
		failure := &gateway.UpstreamFailure{HTTPStatus: http.StatusBadGateway, Code: audit.DegradeClassSearch, PublicMessage: provider.ErrWebSearchPlaceholder.Error()}
		if anthropic {
			writeGatewayAnthropicError(ctx, failure)
		} else {
			writeGatewayError(ctx, failure)
		}
		body := writer.Body.String()
		if writer.Code != http.StatusBadGateway || !strings.Contains(body, audit.DegradeClassSearch) || !strings.Contains(body, provider.ErrWebSearchPlaceholder.Error()) || strings.Contains(body, "Grok") {
			t.Fatalf("status=%d body=%s", writer.Code, body)
		}
	}
	for _, protocol := range []streamProtocol{streamProtocolResponses, streamProtocolChat, streamProtocolAnthropic} {
		cause := fmt.Errorf("wrapped: %w", provider.ErrWebSearchPlaceholder)
		if got := classifyCopyError(context.Background(), cause); got != audit.DegradeClassSearch {
			t.Fatalf("code=%s", got)
		}
		body := string(streamAbortTrailer(protocol, cause, responseMetadata{}, &responsesCompatState{}))
		if !strings.Contains(body, audit.DegradeClassSearch) || !strings.Contains(body, provider.ErrWebSearchPlaceholder.Error()) || strings.Contains(body, "response.completed") {
			t.Fatalf("protocol=%v body=%s", protocol, body)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classifyCopyError(ctx, errors.Join(context.Canceled, provider.ErrWebSearchPlaceholder)); got != "client_stream_interrupted" {
		t.Fatalf("client cancellation classified as %s", got)
	}
}
