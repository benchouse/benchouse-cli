// Package agents registers every agent the CLI supports. Import it for its
// side effect.
package agents

import (
	_ "github.com/benchouse/benchouse-cli/internal/agents/cortex"
	_ "github.com/benchouse/benchouse-cli/internal/agents/cube"
	_ "github.com/benchouse/benchouse-cli/internal/agents/genie"
	_ "github.com/benchouse/benchouse-cli/internal/agents/hex"
	_ "github.com/benchouse/benchouse-cli/internal/agents/langfuse"
	_ "github.com/benchouse/benchouse-cli/internal/agents/lightdash"
	_ "github.com/benchouse/benchouse-cli/internal/agents/looker"
	_ "github.com/benchouse/benchouse-cli/internal/agents/metabase"
	_ "github.com/benchouse/benchouse-cli/internal/agents/nao"
	_ "github.com/benchouse/benchouse-cli/internal/agents/omni"
	_ "github.com/benchouse/benchouse-cli/internal/agents/sigma"
	_ "github.com/benchouse/benchouse-cli/internal/agents/thoughtspot"
	_ "github.com/benchouse/benchouse-cli/internal/agents/wrenai"
)
