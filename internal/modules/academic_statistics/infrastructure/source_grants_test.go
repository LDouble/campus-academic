package infrastructure

import (
	"strings"
	"testing"
)

func TestValidateSourceGrants(t *testing.T) {
	tests := []struct {
		name        string
		grants      []string
		allowWrites bool
		wantError   string
	}{
		{
			name:   "external source remains read only",
			grants: []string{"GRANT USAGE ON *.* TO `source`@`%`", "GRANT SELECT ON `campus_source`.* TO `source`@`%`"},
		},
		{
			name:        "managed source accepts exact upsert permissions",
			grants:      []string{"GRANT USAGE ON *.* TO `source`@`%`", "GRANT SELECT, INSERT, UPDATE ON `campus_source`.* TO `source`@`%`"},
			allowWrites: true,
		},
		{
			name:        "managed source rejects delete",
			grants:      []string{"GRANT SELECT, INSERT, UPDATE, DELETE ON `campus_source`.* TO `source`@`%`"},
			allowWrites: true,
			wantError:   "non-read-only grant",
		},
		{
			name:        "managed source requires writes",
			grants:      []string{"GRANT SELECT ON `campus_source`.* TO `source`@`%`"},
			allowWrites: true,
			wantError:   "requires SELECT, INSERT, UPDATE",
		},
		{
			name:      "external source rejects writes",
			grants:    []string{"GRANT SELECT, INSERT, UPDATE ON `campus_source`.* TO `source`@`%`"},
			wantError: "non-read-only grant",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSourceGrants(test.grants, test.allowWrites)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validateSourceGrants() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validateSourceGrants() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}
