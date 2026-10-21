package outsidecall

import (
	"context"

	"github.com/loveRyujin/lantern/core/workspace"
	"github.com/loveRyujin/lantern/internal/seed"
)

func assigned(ctx context.Context, h *workspace.Handle) error {
	fn := seed.RecordExportApproval
	return fn(ctx, h, "id", "user_contacts")
}
