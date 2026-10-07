package migrations

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ext-workflow: issue_assignee_type_check is shared with upstream. ext_0003
// widens it to admit 'workflow', but a LATER upstream migration that redefines
// the constraint would silently drop 'workflow' on databases that already
// applied ext_0003 (the runner applies the new numeric migration after it).
// These tests are the guard described in the ext-workflow design spec, section 2.

const assigneeConstraintName = "issue_assignee_type_check"

// TestAssigneeConstraintOnlyRedefinedByKnownMigrations fails when a migration
// other than the known ones touches the constraint. A later ext_NNNN
// migration cannot repair it: on a database with workflow-assigned issues the
// upstream migration's ADD CONSTRAINT fails validation before any later
// migration runs, so the deploy stops there. The fix is to patch that
// upstream migration in the fork so its list includes 'workflow', then add
// the file to the known map below.
func TestAssigneeConstraintOnlyRedefinedByKnownMigrations(t *testing.T) {
	known := map[string]bool{
		"084_squad.up.sql":                          true, // adds 'squad'
		"084_squad.down.sql":                        true,
		"ext_0003_issue_assignee_workflow.up.sql":   true, // adds 'workflow'
		"ext_0003_issue_assignee_workflow.down.sql": true,
	}
	files, err := filepath.Glob(filepath.Join(realMigrationsDir(t), "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var unexpected []string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(f)
		if strings.Contains(string(body), assigneeConstraintName) && !known[name] {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Fatalf("migrations %v touch %s. Without 'workflow' in its list it fails on databases with workflow-assigned issues, and no later ext_ migration runs first. Patch that migration in the fork to include 'workflow' in the allowed list, then register the file in this test", unexpected, assigneeConstraintName)
	}
}

// TestAssigneeConstraintAdmitsWorkflowAfterAllMigrations checks the real
// database (migrated by `make test` / `go run ./cmd/migrate up` before the
// suite runs).
func TestAssigneeConstraintAdmitsWorkflowAfterAllMigrations(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()

	var def string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'issue'::regclass
	`, assigneeConstraintName).Scan(&def); err != nil {
		t.Fatalf("read %s: %v", assigneeConstraintName, err)
	}
	for _, want := range []string{"member", "agent", "squad", "workflow"} {
		if !strings.Contains(def, "'"+want+"'") {
			t.Errorf("%s = %s; want it to admit %q", assigneeConstraintName, def, want)
		}
	}
}

// TestIssueAssigneeWorkflowMigration runs ext_0003 up and down against a
// minimal issue table in an isolated schema.
func TestIssueAssigneeWorkflowMigration(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	const schema = "ext_issue_assignee_workflow_migration_test"
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	cleanup := func() { _, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") }
	cleanup()
	t.Cleanup(func() { cleanup(); pool.Close() })
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE issue (
			id UUID NOT NULL DEFAULT gen_random_uuid(),
			assignee_type TEXT,
			assignee_id UUID
		);
		ALTER TABLE issue ADD CONSTRAINT issue_assignee_type_check
			CHECK (assignee_type IN ('member', 'agent', 'squad'));
	`); err != nil {
		t.Fatalf("create v0.6.1 issue table: %v", err)
	}

	insertWorkflow := `INSERT INTO issue (assignee_type, assignee_id) VALUES ('workflow', gen_random_uuid())`
	assertInsertCheckViolation(t, ctx, pool, insertWorkflow)

	applyMigrationFile(t, ctx, pool, "ext_0003_issue_assignee_workflow.up.sql")
	if _, err := pool.Exec(ctx, insertWorkflow); err != nil {
		t.Fatalf("workflow assignee after ext_0003 up: %v", err)
	}
	// Idempotent re-apply.
	applyMigrationFile(t, ctx, pool, "ext_0003_issue_assignee_workflow.up.sql")

	applyMigrationFile(t, ctx, pool, "ext_0003_issue_assignee_workflow.down.sql")
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM issue WHERE assignee_type = 'workflow'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("down left %d workflow-assigned rows", remaining)
	}
	assertInsertCheckViolation(t, ctx, pool, insertWorkflow)
}
