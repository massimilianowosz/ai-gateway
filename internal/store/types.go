package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// StringList is a []string that serializes to JSON in the database.
//
// A nil list and an empty one are different values, and both round-trip:
// NULL means "no whitelist configured" while "[]" means "an empty whitelist",
// which denies everything. Writing "[]" for nil would have made every row
// created without a whitelist indistinguishable from one that revokes access.
type StringList []string

func (s StringList) Value() (driver.Value, error) {
	if s == nil {
		return nil, nil
	}
	b, err := json.Marshal(s)
	return string(b), err
}

func (s *StringList) Scan(value any) error {
	if value == nil {
		*s = nil
		return nil
	}
	var bytes []byte
	switch v := value.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	default:
		return fmt.Errorf("cannot scan %T into StringList", value)
	}
	return json.Unmarshal(bytes, s)
}
