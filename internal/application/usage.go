package application

import (
	"context"
	"errors"

	"centrext/internal/domain"
	"centrext/internal/policy"
)

// MaxUsedPerCall caps how many ids one context_used call may report.
const MaxUsedPerCall = 20

// UsedResult says which reported ids were recorded.
type UsedResult struct {
	Recorded []string
	Unknown  []string // not found, or not visible to this agent
}

// MarkUsed records that an agent relied on saved context (ADR-015). Centrext
// can't see conversations, so "used" is what the agent reports. It changes no
// context and grants nothing; ids the agent can't read count as unknown.
func (s *Service) MarkUsed(ctx context.Context, actor domain.Actor, ids []string) (UsedResult, error) {
	var res UsedResult
	if actor.IsUser() {
		return res, domain.Invalidf("only agents report usage")
	}
	if len(ids) == 0 {
		return res, domain.Invalidf("give at least one context id")
	}
	if len(ids) > MaxUsedPerCall {
		return res, domain.Invalidf("at most %d ids per call", MaxUsedPerCall)
	}
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			c, err := s.authorizeContext(tx, actor, id)
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrForbidden) {
				res.Unknown = append(res.Unknown, id)
				continue
			}
			if err != nil {
				return err
			}
			now := s.now()
			if err := tx.RecordUsage(domain.UsageEvent{ID: newID("use", now), At: now, AgentID: actor.ID,
				ContextID: c.ID, Version: c.CurrentVersion, Kind: domain.UsageUsed}); err != nil {
				return err
			}
			res.Recorded = append(res.Recorded, c.ID)
		}
		return nil
	})
	return res, err
}

// CaptureStats returns the capture metrics (ADR-015) for the user.
func (s *Service) CaptureStats(ctx context.Context, actor domain.Actor) ([]domain.CaptureStat, error) {
	if err := policy.RequireUser(actor, "reading metrics"); err != nil {
		return nil, err
	}
	var out []domain.CaptureStat
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		out, err = tx.CaptureStats()
		return err
	})
	return out, err
}
