// Command migrate applies or rolls back versioned SQL migrations.
//
// Usage:
//
//	migrate [-path DIR] [-database URL] up
//	migrate [-path DIR] [-database URL] down
//	migrate [-path DIR] [-database URL] version
//	migrate [-path DIR] [-database URL] force VERSION
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	path := fs.String("path", "migrations", "directory containing .up.sql/.down.sql files")
	database := fs.String("database", os.Getenv("RELAYFORGE_DATABASE_URL"), "PostgreSQL URL (default RELAYFORGE_DATABASE_URL)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 && fs.NArg() != 2 {
		return errors.New("usage: migrate [-path DIR] [-database URL] <up|down|version|force VERSION>")
	}
	if *database == "" {
		return errors.New("database URL is required (flag -database or RELAYFORGE_DATABASE_URL)")
	}

	url, err := migrateURL(*database)
	if err != nil {
		return err
	}

	source, err := iofs.New(os.DirFS(*path), ".")
	if err != nil {
		return fmt.Errorf("open migration directory %q: %w", *path, err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, url)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			fmt.Fprintln(os.Stderr, "migrate: close source:", srcErr)
		}
		if dbErr != nil {
			fmt.Fprintln(os.Stderr, "migrate: close database:", dbErr)
		}
	}()

	switch command := fs.Arg(0); command {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		return reportVersion(m)
	case "down":
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return err
		}
		fmt.Println("migrations rolled back")
		return nil
	case "version":
		return reportVersion(m)
	case "force":
		if fs.NArg() != 2 {
			return errors.New("force requires a version: migrate force VERSION")
		}
		var v int
		if _, err := fmt.Sscanf(fs.Arg(1), "%d", &v); err != nil {
			return fmt.Errorf("invalid version %q: %w", fs.Arg(1), err)
		}
		if err := m.Force(v); err != nil {
			return err
		}
		fmt.Printf("forced schema version %d\n", v)
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func reportVersion(m *migrate.Migrate) error {
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		fmt.Println("schema version: none")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("schema version: %d dirty=%t\n", v, dirty)
	return nil
}

// migrateURL maps postgres/postgresql to the scheme registered by the pgx/v5
// migration driver. Credentials and query parameters are preserved.
func migrateURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse database URL: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		u.Scheme = "pgx5"
		return u.String(), nil
	default:
		return "", fmt.Errorf("unsupported database URL scheme %q", u.Scheme)
	}
}
