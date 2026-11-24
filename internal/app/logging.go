package app

import (
	"log/slog"
	"time"
)

type routingReason string

const (
	routingReasonOwnerRemoved     routingReason = "owner_removed"
	routingReasonOwnerPaused      routingReason = "owner_paused"
	routingReasonOwnerSignedOut   routingReason = "owner_signed_out"
	routingReasonOwnerNotRoutable routingReason = "owner_not_routable"
)

func routingLogAttrs(candidate routingCandidate, now time.Time) []any {
	status := candidate.status(now)
	priority, prioritized := candidate.routingPriority(now)
	prioritized = prioritized && status == accountPriority
	attrs := []any{
		"account", candidate.id,
		"status", status,
		"routing_mode", candidate.mode,
		"reset_priority", prioritized,
		"banked_resets_known", candidate.resetCredits.known,
		"banked_resets", candidate.resetCredits.count,
		"usage_limit_reached", candidate.spent,
		"cooldown_until", candidate.cooldown,
		"last_used_at", candidate.lastUsed,
		"reauth", candidate.reauth,
		"primary", windowLogValue(candidate.primary),
		"secondary", windowLogValue(candidate.secondary),
		"fable", windowLogValue(candidate.fable),
	}
	if prioritized {
		attrs = append(attrs,
			"reset_priority_expires_at", priority.expiresAt,
			"reset_priority_remaining_percent", priority.remainingPercent,
		)
	}
	return attrs
}

func logResponseUsage(log *slog.Logger, thread, account, model, serviceTier string, metadata turnMetadata, duration time.Duration, usage responseUsage) {
	log.Debug("response usage",
		"thread", thread,
		"turn", metadata.TurnID,
		"request_kind", metadata.RequestKind,
		"account", account,
		"model", model,
		"service_tier", serviceTier,
		"duration", duration,
		"input_tokens", usage.InputTokens,
		"cached_tokens", usage.InputDetails.CachedTokens,
		"cache_write_tokens", usage.InputDetails.CacheWriteTokens,
		"output_tokens", usage.OutputTokens,
		"reasoning_tokens", usage.OutputDetails.ReasoningTokens,
	)
}

func windowLogValue(value window) slog.Value {
	return slog.GroupValue(
		slog.Bool("known", value.known()),
		slog.Float64("used_percent", value.usedPercent),
		slog.Int("window_minutes", value.minutes),
		slog.Time("reset_at", value.resetsAt),
		slog.Time("seen_at", value.seenAt),
	)
}
