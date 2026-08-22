// Command academicctl contains lifecycle commands for the Analytics database.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 || args[0] != "migrate" {
		return errors.New("usage: academicctl migrate up|down")
	}
	url := os.Getenv("CAMPUS_ACADEMIC_MIGRATION_URL")
	if url == "" {
		return errors.New("CAMPUS_ACADEMIC_MIGRATION_URL is required")
	}
	directory := os.Getenv("CAMPUS_ACADEMIC_MIGRATIONS_DIR")
	if directory == "" {
		directory = "migrations"
	}
	directory, err := filepath.Abs(directory)
	if err != nil {
		return fmt.Errorf("resolve migrations directory: %w", err)
	}
	migrator, err := migrate.New("file://"+directory, url)
	if err != nil {
		return fmt.Errorf("create migration runner: %w", err)
	}
	defer func() { _, _ = migrator.Close() }()
	switch args[1] {
	case "up":
		err = migrator.Up()
	case "down":
		err = migrator.Down()
	default:
		return errors.New("usage: academicctl migrate up|down")
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run migration %s: %w", args[1], err)
	}
	return nil
}
