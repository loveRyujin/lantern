package outsidecall

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	alias "github.com/loveRyujin/lantern/internal/seed"
)

func viaAlias(ctx context.Context, h *workspace.Handle) error {
	return alias.RecordExportApproval(ctx, h, "id", "user_contacts")
}
