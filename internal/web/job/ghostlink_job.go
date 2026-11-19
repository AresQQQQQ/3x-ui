package job

import (
	"context"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink"
	"github.com/mhsanaei/3x-ui/v3/internal/logger"
	"github.com/mhsanaei/3x-ui/v3/internal/web/service"
)

// GhostLinkJob converges the supervised GhostLink process to the
// panel's persisted desired state. It is independent from every Xray job.
type GhostLinkJob struct{ ctx context.Context }

func NewGhostLinkJob(ctx context.Context) *GhostLinkJob {
	return &GhostLinkJob{ctx: ctx}
}

func (j *GhostLinkJob) Run() {
	if err := ghostlink.DefaultInstaller().Step(j.ctx); err != nil && j.ctx.Err() == nil {
		logger.Warning("GhostLink installation:", err)
	}
	inboundService := &service.InboundService{}
	usageCtx, usageCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer usageCancel()
	result, usageErr := inboundService.CollectGhostLinkUsage(usageCtx)
	if usageErr != nil {
		logger.Warning("GhostLink usage collection failed:", usageErr)
	}
	if result.UploadDelta > 0 || result.DownloadDelta > 0 || result.Pending > 0 {
		logger.Debugf(
			"GhostLink usage collection: users=%d charged=%d baselines=%d up=%d down=%d pending=%d",
			result.UsersSeen,
			result.UsersCharged,
			result.Baselines,
			result.UploadDelta,
			result.DownloadDelta,
			result.Pending,
		)
	}
	if usageErr != nil {
		logger.Debug("GhostLink job completed with diagnostics:", usageErr)
	}
}
