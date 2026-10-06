// Package db defines a generic persistence abstraction layer for Provider Bridge.
//
// Providers manage database connections (SQLite, D1). Consumers declare table
// schemas and access the database through a Store interface with table-name
// isolation.
package db

import (
	"context"
	"database/sql"
)

// Dialect identifies the database backend dialect.
type Dialect string

const (
	DialectSQLite Dialect = "sqlite"
	DialectD1     Dialect = "d1"
)

// Features describes backend-specific capabilities that consumers can use to
// adjust their DDL or queries.
type Features struct {
	SupportsPragma bool // PRAGMA statements are supported
	SupportsWAL    bool // WAL journal mode is supported
	WorkerBound    bool // Running inside a Cloudflare Worker (limited lifecycle)
}

// Provider is the interface for database connection lifecycle management.
// Consumers never access the Provider directly; they go through Store.
type Provider interface {
	// Name returns a unique identifier for this provider (e.g. "db_sqlite").
	Name() string

	// Dialect returns the database dialect.
	Dialect() Dialect

	// Open establishes the database connection.
	Open(ctx context.Context) error

	// DB returns the underlying *sql.DB. Must only be called after Open succeeds.
	DB() *sql.DB

	// Ping verifies the connection is still alive.
	Ping(ctx context.Context) error

	// Close closes the database connection.
	Close() error

	// Features returns the capability flags for this backend.
	Features() Features
}
