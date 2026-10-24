package main

import "testing"

func TestMigrateURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{
			"postgres://user:pass@localhost:5432/db?sslmode=disable",
			"pgx5://user:pass@localhost:5432/db?sslmode=disable",
		},
		{
			"postgresql://user:pass@db.example.com/relayforge",
			"pgx5://user:pass@db.example.com/relayforge",
		},
	}
	for _, tc := range tests {
		got, err := migrateURL(tc.in)
		if err != nil {
			t.Fatalf("migrateURL(%q) error = %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("migrateURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMigrateURLRejectsOtherSchemes(t *testing.T) {
	if _, err := migrateURL("mysql://localhost/db"); err == nil {
		t.Fatal("expected an error for a non-PostgreSQL URL")
	}
}
