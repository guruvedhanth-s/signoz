package slack

import (
	"context"
	"testing"

	"github.com/guruvedhanth-s/signoz/sre-sidekick/internal/config"
)

func TestPermissiveAuthorizerAllowsUnconfiguredAdvisoryMVP(t *testing.T) {
	result := (PermissiveAuthorizer{}).Authorize(context.Background(), AuthorizationRequest{UserID: "U1", Required: RoleApprover})
	if !result.Allowed {
		t.Fatal("permissive authorizer denied an unconfigured user")
	}
}

func TestStaticAuthorizerOperatorImpliesApprover(t *testing.T) {
	authorizer, err := NewStaticAuthorizer(config.AuthorizationConfig{
		Operators: config.AuthorizationRoleConfig{Users: []string{"U-operator"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []Role{RoleApprover, RoleOperator} {
		result := authorizer.Authorize(context.Background(), AuthorizationRequest{UserID: "U-operator", Required: role})
		if !result.Allowed {
			t.Errorf("operator was denied role %s: %s", role, result.Reason)
		}
	}
}

func TestStaticAuthorizerDeniesUnknownUser(t *testing.T) {
	authorizer, err := NewStaticAuthorizer(config.AuthorizationConfig{
		Approvers: config.AuthorizationRoleConfig{Users: []string{"U-approver"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := authorizer.Authorize(context.Background(), AuthorizationRequest{UserID: "U-other", Required: RoleApprover})
	if result.Allowed || result.Reason == "" {
		t.Fatalf("authorization result = %+v, want a denied result with a reason", result)
	}
}

func TestStaticAuthorizerResolvesGroups(t *testing.T) {
	authorizer, err := NewStaticAuthorizer(config.AuthorizationConfig{
		Approvers: config.AuthorizationRoleConfig{Groups: []string{"sre-oncall"}},
	}, map[string][]string{"sre-oncall": {"U-oncall"}})
	if err != nil {
		t.Fatal(err)
	}
	if result := authorizer.Authorize(context.Background(), AuthorizationRequest{UserID: "U-oncall", Required: RoleApprover}); !result.Allowed {
		t.Fatalf("group member was denied: %+v", result)
	}
}
