package outsidecall

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func direct(ctx context.Context, h *workspace.Handle) error {
	return seed.RecordExportApproval(ctx, h, "id", "user_contacts")
}
