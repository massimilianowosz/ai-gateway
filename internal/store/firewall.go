package store

import (
	"context"
	"time"
)

// FirewallKind is what a FirewallRule's pattern is matched against.
const (
	FirewallKindMCPServer = "mcp_server"
	FirewallKindTool      = "tool"
	FirewallKindFileRead  = "file_read"
	FirewallKindFileWrite = "file_write"
)

// FirewallRule declares one MCP server, tool or file path a session's traffic
// must not touch. Unlike the provider allowlist, which is a property of a key,
// this reads what the model actually asked to do — the same tool and file
// extraction hivetrace already performs for tracing, evaluated against
// declared rules instead of only reported.
//
// The pattern is stored as the operator typed it, wildcards and all, using
// the same syntax as a watchlist term: everything is literal except *, which
// matches a run of non-space characters.
type FirewallRule struct {
	ID      uint   `json:"id" gorm:"primaryKey;autoIncrement"`
	Kind    string `json:"kind" gorm:"column:kind;not null;uniqueIndex:idx_firewall_rule"`
	Label   string `json:"label" gorm:"column:label;not null"`
	Pattern string `json:"pattern" gorm:"column:pattern;not null;uniqueIndex:idx_firewall_rule"`
	// Enabled lets an operator retire a rule without losing how it was
	// written, the same reasoning as WatchlistTerm.Enabled.
	Enabled   bool      `json:"enabled" gorm:"column:enabled;default:true"`
	CreatedAt time.Time `json:"created_at" gorm:"column:created_at"`
}

func (FirewallRule) TableName() string { return "gw_firewall_rules" }

// ListFirewallRules returns every rule, enabled or not, oldest first so the
// console shows a stable order.
func (s *GormStore) ListFirewallRules(ctx context.Context) ([]FirewallRule, error) {
	var rules []FirewallRule
	err := s.db.WithContext(ctx).Order("id ASC").Find(&rules).Error
	return rules, err
}

func (s *GormStore) CreateFirewallRule(ctx context.Context, rule *FirewallRule) error {
	rule.CreatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Create(rule).Error
}

func (s *GormStore) SetFirewallRuleEnabled(ctx context.Context, id uint, enabled bool) error {
	return s.db.WithContext(ctx).Model(&FirewallRule{}).
		Where("id = ?", id).Update("enabled", enabled).Error
}

func (s *GormStore) DeleteFirewallRule(ctx context.Context, id uint) error {
	return s.db.WithContext(ctx).Delete(&FirewallRule{}, id).Error
}
