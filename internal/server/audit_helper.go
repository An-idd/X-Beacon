package server

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"github.com/An-idd/x-beacon/internal/audit"
	"github.com/An-idd/x-beacon/internal/auth"
	"github.com/An-idd/x-beacon/internal/server/middleware"
)

// recordAudit writes an audit row using the actor pulled from the
// request context. Failure is logged but not surfaced — the admin
// action already succeeded by the time we get here, and we'd rather
// lose an audit row than mislead the operator with a false 500.
//
// Recorder may be the audit.Nop() instance (no DB); calls then go
// to a no-op without touching the DB. Logging stays gated on
// recorder type so the dev-mode log isn't spammed.
func recordAudit(
	ctx context.Context,
	recorder audit.Recorder,
	r *http.Request,
	action audit.Action,
	targetType, targetID string,
	metadata any,
	logger *zap.Logger,
) {
	if recorder == nil {
		return
	}
	p := auth.PrincipalFrom(ctx)
	actorID, actorLabel := "", ""
	if p != nil {
		actorID = p.ID
		actorLabel = p.Name
	}
	if actorID == "" {
		// Should be unreachable — audit recordings happen behind
		// scope-guarded routes — but defend anyway.
		actorID = "unknown"
	}

	entry := audit.Entry{
		ActorID:    actorID,
		ActorLabel: actorLabel,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Metadata:   metadata,
		RequestID:  middleware.RequestIDFrom(ctx),
	}
	if err := recorder.Record(ctx, entry); err != nil {
		logger.Warn("audit record failed",
			zap.String("action", string(action)),
			zap.String("target_type", targetType),
			zap.String("target_id", targetID),
			zap.Error(err))
	}
}
