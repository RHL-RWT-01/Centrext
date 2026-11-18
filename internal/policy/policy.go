// Package policy decides what an actor may do. Application services call it
// before acting; transports never make these decisions.
package policy

import (
	"slices"

	"centrext/internal/domain"
)

// Outcome is the result of evaluating a proposal.
type Outcome int

const (
	Deny Outcome = iota
	AutoAccept
	NeedsApproval
)

func (o Outcome) String() string {
	switch o {
	case AutoAccept:
		return "auto_accept"
	case NeedsApproval:
		return "needs_approval"
	}
	return "deny"
}

// Decision says what happens to a proposal and with which trust.
type Decision struct {
	Outcome Outcome
	Trust   domain.Trust // trust of the revision if accepted now
	Expires bool         // silent observations expire unless approved (ADR-015)
	Reason  string
}

// ProposalRequest is the part of a proposal policy looks at.
type ProposalRequest struct {
	Kind   domain.ProposalKind
	Origin domain.ProposalOrigin
	Type   domain.ContextType
}

// TrustForOrigin assigns trust from how an item arrived (ADR-013).
// It never reads anything the agent claims about trust.
func TrustForOrigin(o domain.ProposalOrigin) domain.Trust {
	switch o {
	case domain.OriginUser:
		return domain.TrustUserVerified
	case domain.OriginExplicit, domain.OriginSuggested:
		return domain.TrustSessionConfirmed
	case domain.OriginSilent:
		return domain.TrustAgentReported
	}
	return domain.TrustUntrusted
}

// Types an agent may suggest saving (ADR-015).
var suggestibleTypes = []domain.ContextType{
	domain.TypeDecision, domain.TypeArchitecture, domain.TypeConvention, domain.TypePreference,
}

// Types whose revision always needs approval (section 5 capability defaults).
var approvalOnReviseTypes = []domain.ContextType{
	domain.TypeDecision, domain.TypeArchitecture,
}

func deny(reason string) Decision { return Decision{Outcome: Deny, Reason: reason} }

// EvaluateProposal decides whether a proposal is accepted now, waits for the
// user's approval, or is refused. target is the context being revised or
// superseded, nil otherwise.
func EvaluateProposal(actor domain.Actor, req ProposalRequest, target *domain.Context) Decision {
	if actor.IsUser() {
		if req.Origin != domain.OriginUser {
			return deny("user writes must use the user origin")
		}
		return Decision{Outcome: AutoAccept, Trust: domain.TrustUserVerified}
	}

	if actor.Agent == nil || !actor.Agent.Active() {
		return deny("agent is not active")
	}
	if req.Origin == domain.OriginUser {
		return deny("agents cannot claim user origin")
	}
	trust := TrustForOrigin(req.Origin)

	switch req.Kind {
	case domain.KindCreate:
		switch req.Origin {
		case domain.OriginSilent:
			if req.Type != domain.TypeObservation {
				return deny("without confirmation, agents may only add observations")
			}
			return Decision{Outcome: AutoAccept, Trust: trust, Expires: true}
		case domain.OriginSuggested:
			if !slices.Contains(suggestibleTypes, req.Type) {
				return deny("agents may only suggest decisions, architecture, conventions and preferences")
			}
		}
		return Decision{Outcome: AutoAccept, Trust: trust}

	case domain.KindRevise:
		if req.Origin == domain.OriginSilent {
			return deny("revisions need the user's confirmation")
		}
		if target == nil {
			return deny("revision target missing")
		}
		if target.HeadTrust == domain.TrustUserVerified {
			return Decision{Outcome: NeedsApproval, Trust: trust, Reason: "target is user_verified"}
		}
		if slices.Contains(approvalOnReviseTypes, target.Type) {
			return Decision{Outcome: NeedsApproval, Trust: trust, Reason: "revising a " + string(target.Type) + " needs approval"}
		}
		return Decision{Outcome: AutoAccept, Trust: trust}

	case domain.KindSupersede:
		if req.Origin == domain.OriginSilent {
			return deny("superseding needs the user's confirmation")
		}
		return Decision{Outcome: NeedsApproval, Trust: trust, Reason: "superseding needs approval"}

	case domain.KindCreateNamespace:
		if req.Origin == domain.OriginSilent {
			return deny("agents cannot create namespaces silently")
		}
		return Decision{Outcome: NeedsApproval, Trust: trust, Reason: "new namespaces need approval"}
	}
	return deny("unknown proposal kind")
}

// CanAccessNamespace checks read and propose access. Nesting never grants
// access: an agent needs a grant for that exact namespace, or an all-namespaces grant.
func CanAccessNamespace(actor domain.Actor, namespaceID string, grants []domain.Grant) error {
	if actor.IsUser() {
		return nil
	}
	if actor.Agent == nil || !actor.Agent.Active() {
		return domain.ErrUnauthenticated
	}
	for _, g := range grants {
		if g.AgentID == actor.Agent.ID && (g.NamespaceID == "" || g.NamespaceID == namespaceID) {
			return nil
		}
	}
	return domain.Forbiddenf("agent has no access to namespace %s", namespaceID)
}

// RequireUser allows only the human owner (review, delete, manage agents and namespaces).
func RequireUser(actor domain.Actor, action string) error {
	if actor.IsUser() {
		return nil
	}
	return domain.Forbiddenf("%s requires the user, outside the chat", action)
}
