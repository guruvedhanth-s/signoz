package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
)

// Role is the minimum permission needed for an interactive action.
type Role string

const (
	RoleApprover Role = "approver"
	RoleOperator Role = "operator"
)

// AuthorizationRequest is evaluated at Slack click time, never when the
// message is rendered. Slack messages are shared by all viewers.
type AuthorizationRequest struct {
	UserID    string
	ChannelID string
	ThreadTS  string
	ActionID  string
	Required  Role
}

type AuthorizationResult struct {
	Allowed bool
	Role    Role
	Reason  string
}

// Authorizer is deliberately a small Slack-side policy boundary. The
// coordinator asks it before changing session state or invoking an actuator.
type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) AuthorizationResult
}

// PermissiveAuthorizer preserves advisory MVP behavior when no rosters are
// configured. It is intentionally explicit rather than a nil special case.
type PermissiveAuthorizer struct{}

func (PermissiveAuthorizer) Authorize(context.Context, AuthorizationRequest) AuthorizationResult {
	return AuthorizationResult{Allowed: true, Role: RoleApprover, Reason: "authorization is not configured"}
}

// StaticAuthorizer matches Slack user IDs and resolved user-group members.
// Operators imply approvers, so an operator can perform every approver action.
type StaticAuthorizer struct {
	approvers map[string]struct{}
	operators map[string]struct{}
}

func NewStaticAuthorizer(cfg config.AuthorizationConfig, groups map[string][]string) (*StaticAuthorizer, error) {
	a := &StaticAuthorizer{approvers: map[string]struct{}{}, operators: map[string]struct{}{}}
	add := func(dst map[string]struct{}, users []string, groupNames []string) error {
		for _, user := range users {
			user = strings.TrimSpace(user)
			if user != "" {
				dst[user] = struct{}{}
			}
		}
		for _, group := range groupNames {
			group = strings.TrimPrefix(strings.TrimSpace(group), "@")
			members, ok := groups[group]
			if !ok {
				return fmt.Errorf("authorization group %q could not be resolved", group)
			}
			for _, user := range members {
				if user = strings.TrimSpace(user); user != "" {
					dst[user] = struct{}{}
				}
			}
		}
		return nil
	}
	if err := add(a.approvers, cfg.Approvers.Users, cfg.Approvers.Groups); err != nil {
		return nil, err
	}
	if err := add(a.operators, cfg.Operators.Users, cfg.Operators.Groups); err != nil {
		return nil, err
	}
	for user := range a.operators {
		a.approvers[user] = struct{}{}
	}
	return a, nil
}

func (a *StaticAuthorizer) Authorize(_ context.Context, req AuthorizationRequest) AuthorizationResult {
	user := strings.TrimSpace(req.UserID)
	if user == "" {
		return AuthorizationResult{Reason: "Slack user identity is missing"}
	}
	if req.Required == RoleOperator {
		if _, ok := a.operators[user]; ok {
			return AuthorizationResult{Allowed: true, Role: RoleOperator}
		}
		return AuthorizationResult{Reason: "user is not in the operator roster"}
	}
	if _, ok := a.approvers[user]; ok {
		role := RoleApprover
		if _, operator := a.operators[user]; operator {
			role = RoleOperator
		}
		return AuthorizationResult{Allowed: true, Role: role}
	}
	return AuthorizationResult{Reason: "user is not in the approver or operator roster"}
}

// SlackUserGroupLister is the only Slack API surface needed during startup.
type SlackUserGroupLister interface {
	GetUserGroupsContext(context.Context, ...slack.GetUserGroupsOption) ([]slack.UserGroup, error)
}

// NewConfiguredAuthorizer resolves configured Slack group handles once during
// startup. Static user IDs require no Slack API call. A configured but missing
// group fails closed rather than silently granting access to nobody.
func NewConfiguredAuthorizer(ctx context.Context, cfg config.AuthorizationConfig, api SlackUserGroupLister) (Authorizer, error) {
	if len(cfg.Approvers.Users) == 0 && len(cfg.Approvers.Groups) == 0 && len(cfg.Operators.Users) == 0 && len(cfg.Operators.Groups) == 0 {
		return PermissiveAuthorizer{}, nil
	}
	groups := map[string][]string{}
	needed := map[string]struct{}{}
	for _, roster := range []config.AuthorizationRoleConfig{cfg.Approvers, cfg.Operators} {
		for _, group := range roster.Groups {
			needed[strings.TrimPrefix(strings.TrimSpace(group), "@")] = struct{}{}
		}
	}
	if len(needed) > 0 {
		if api == nil {
			return nil, fmt.Errorf("Slack API is required to resolve configured authorization groups")
		}
		all, err := api.GetUserGroupsContext(ctx, slack.GetUserGroupsOptionIncludeUsers(true))
		if err != nil {
			return nil, fmt.Errorf("list Slack user groups: %w", err)
		}
		for _, group := range all {
			if _, ok := needed[group.Handle]; ok {
				groups[group.Handle] = append([]string(nil), group.Users...)
			}
		}
	}
	return NewStaticAuthorizer(cfg, groups)
}
