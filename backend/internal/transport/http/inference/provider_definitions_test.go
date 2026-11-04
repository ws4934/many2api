package inference

import (
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/grok/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/grok/console"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/grok/web"
)

// grokDefinitions returns the production declarations of the Grok providers.
var grokDefinitions = provider.NewRegistry(
	cli.NewAdapter(cli.Config{}, nil),
	web.NewAdapter(web.Config{}, nil, nil, nil, nil),
	console.NewAdapter(console.Config{}, nil, nil, nil),
).Definition
