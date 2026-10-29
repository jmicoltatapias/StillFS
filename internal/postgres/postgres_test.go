package postgres

import "testing"

func TestMigrationVersion(t *testing.T) {
	for _, tt := range []struct {
		name    string
		version int64
		wantErr bool
	}{{"0001_initial.sql", 1, false}, {"0012_add_index.sql", 12, false}, {"initial.sql", 0, true}, {"0000_initial.sql", 0, true}} {
		t.Run(tt.name, func(t *testing.T) {
			version, err := migrationVersion(tt.name)
			if (err != nil) != tt.wantErr || version != tt.version {
				t.Fatalf("migrationVersion(%q) = (%d, %v)", tt.name, version, err)
			}
		})
	}
}
