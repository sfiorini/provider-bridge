package configgraph

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"providerbridge/internal/db"
	"providerbridge/internal/service/store"

	_ "modernc.org/sqlite"
)

// TestPatchSecretPlaceholderPreservesCommaKeyAndRotation (S-5-2): PATCHing
// api_key with the "******" mask on a provider whose stored key is the comma
// list "k1,k2" must keep the stored key AND leave the store's key_rotation
// row for the provider untouched.
//
// NOTE (orchestrator ruling, mechanism discrepancy): the story text says "no
// manager reload occurs on placeholder PATCH" — in fact api_key is
// hot-reloadable in the schema, so runtime.Reload DOES run on this PATCH.
// Rotation state survives because ProviderManager.Reload copies the active
// index across reloads (S-3-6). We therefore assert the OUTCOMES (stored
// config keeps "k1,k2"; key_rotation row unchanged) and additionally pin the
// reload count to document the actual mechanism. No production change.
func TestPatchSecretPlaceholderPreservesCommaKeyAndRotation(t *testing.T) {
	ctx := context.Background()

	cfg := testConfig()
	anthropicDef := cfg.ProviderDefs["anthropic"]
	anthropicDef.APIKey = "k1,k2"
	cfg.ProviderDefs["anthropic"] = anthropicDef

	// Real in-memory SQLite config store (harness pattern: service/api tests).
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open error = %v", err)
	}
	t.Cleanup(func() { database.Close() })

	c := store.NewConfigStoreConsumer(nil)
	tables := c.Tables()
	tableNames := make(map[string]string, len(tables))
	for _, tbl := range tables {
		realName := "config_store_" + tbl.Name
		tableNames[tbl.Name] = realName
		ddl := strings.ReplaceAll(tbl.Schema, "{{table}}", realName)
		if _, err := database.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("create table %q: %v", realName, err)
		}
	}
	ts := &rotationTestDB{db: database, tables: tableNames}
	if err := c.BindStore(ts); err != nil {
		t.Fatalf("BindStore() error = %v", err)
	}
	cs := c.Store()
	if cs == nil {
		t.Fatal("Store() returned nil")
	}
	if err := cs.SeedFromConfig(&cfg); err != nil {
		t.Fatalf("SeedFromConfig() error = %v", err)
	}

	// Advance the rotation index for the provider before the PATCH.
	if err := cs.SetProviderKeyIndex(ctx, "anthropic", 1); err != nil {
		t.Fatalf("SetProviderKeyIndex() error = %v", err)
	}
	idx, err := cs.LoadProviderKeyIndexes(ctx)
	if err != nil {
		t.Fatalf("LoadProviderKeyIndexes() error = %v", err)
	}
	if v, ok := idx["anthropic"]; !ok || v != 1 {
		t.Fatalf("LoadProviderKeyIndexes()[\"anthropic\"] = %d (ok=%v), want 1 before PATCH", v, ok)
	}

	rev, err := cs.CurrentRevision()
	if err != nil {
		t.Fatalf("CurrentRevision() error = %v", err)
	}
	rt := &fakeRuntime{current: cfg}
	svc := NewService(cs, rt, nil)

	resp, err := svc.Patch(ctx, PatchRequest{
		BaseRevision: rev,
		Changes: []PatchOp{
			{Kind: ResourceProvider, ID: "anthropic", Field: "api_key", Value: secretMask},
		},
	})
	if err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	if resp.Result != ResultCommitted {
		t.Fatalf("Patch().Result = %q, want %q (errors: %+v)", resp.Result, ResultCommitted, resp.Errors)
	}
	// api_key is hot-reloadable: the runtime reload DOES run (see note above).
	if rt.reloadCalls != 1 {
		t.Fatalf("runtime reload calls = %d, want 1 (placeholder PATCH is hot-reloadable)", rt.reloadCalls)
	}

	// Outcome 1: the stored config keeps the comma-separated key list.
	loaded, err := cs.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() after PATCH error = %v", err)
	}
	if got := loaded.ProviderDefs["anthropic"].APIKey; got != "k1,k2" {
		t.Fatalf("LoadAll().ProviderDefs[anthropic].APIKey = %q, want %q", got, "k1,k2")
	}

	// Outcome 2: the key_rotation row for the provider is unchanged.
	idx, err = cs.LoadProviderKeyIndexes(ctx)
	if err != nil {
		t.Fatalf("LoadProviderKeyIndexes() after PATCH error = %v", err)
	}
	if len(idx) != 1 {
		t.Fatalf("LoadProviderKeyIndexes() after PATCH = %v, want exactly 1 entry", idx)
	}
	if v, ok := idx["anthropic"]; !ok || v != 1 {
		t.Fatalf("LoadProviderKeyIndexes() after PATCH[\"anthropic\"] = %d (ok=%v), want 1 (rotation state untouched)", v, ok)
	}
}

// rotationTestDB implements db.Store backed by an in-memory SQLite database.
type rotationTestDB struct {
	db     *sql.DB
	tables map[string]string
}

func (s *rotationTestDB) ConsumerName() string { return "config_store" }
func (s *rotationTestDB) Dialect() db.Dialect  { return db.DialectSQLite }
func (s *rotationTestDB) Table(localName string) (string, error) {
	realName, ok := s.tables[localName]
	if !ok {
		return "", db.ErrTableNotRegistered
	}
	return realName, nil
}

func (s *rotationTestDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

func (s *rotationTestDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

func (s *rotationTestDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *rotationTestDB) WithTx(ctx context.Context, fn func(db.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&rotationTestTx{tx: tx, tables: s.tables}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type rotationTestTx struct {
	tx     *sql.Tx
	tables map[string]string
}

func (t *rotationTestTx) Table(localName string) (string, error) {
	realName, ok := t.tables[localName]
	if !ok {
		return "", db.ErrTableNotRegistered
	}
	return realName, nil
}

func (t *rotationTestTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

func (t *rotationTestTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

func (t *rotationTestTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}
