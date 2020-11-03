package msc2836

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/matrix-org/dendrite/internal/config"
	"github.com/matrix-org/dendrite/internal/sqlutil"
	"github.com/matrix-org/gomatrixserverlib"
)

type Database interface {
	// StoreRelation stores the parent->child and child->parent relationship for later querying.
	// Also stores the event metadata e.g timestamp
	StoreRelation(ctx context.Context, ev *gomatrixserverlib.HeaderedEvent) error
	// ChildrenForParent returns the event IDs of events who have the given `eventID` as an m.relationship with the
	// provided `relType`.
	ChildrenForParent(ctx context.Context, eventID, relType string) ([]string, error)
}

type DB struct {
	db                          *sql.DB
	writer                      sqlutil.Writer
	insertEdgeStmt              *sql.Stmt
	insertNodeStmt              *sql.Stmt
	selectChildrenForParentStmt *sql.Stmt
}

// NewDatabase loads the database for msc2836
func NewDatabase(dbOpts *config.DatabaseOptions) (Database, error) {
	if dbOpts.ConnectionString.IsPostgres() {
		return newPostgresDatabase(dbOpts)
	}
	return newSQLiteDatabase(dbOpts)
}

func newPostgresDatabase(dbOpts *config.DatabaseOptions) (Database, error) {
	d := DB{
		writer: sqlutil.NewDummyWriter(),
	}
	var err error
	if d.db, err = sqlutil.Open(dbOpts); err != nil {
		return nil, err
	}
	_, err = d.db.Exec(`
	CREATE TABLE IF NOT EXISTS msc2836_edges (
		parent_event_id TEXT NOT NULL,
		child_event_id TEXT NOT NULL,
		rel_type TEXT NOT NULL,
		CONSTRAINT msc2836_edges UNIQUE (parent_event_id, child_event_id, rel_type)
	);

	CREATE TABLE IF NOT EXISTS msc2836_nodes (
		event_id TEXT PRIMARY KEY NOT NULL,
		origin_server_ts BIGINT NOT NULL,
		room_id TEXT NOT NULL
	);
	`)
	if err != nil {
		return nil, err
	}
	if d.insertEdgeStmt, err = d.db.Prepare(`
		INSERT INTO msc2836_edges(parent_event_id, child_event_id, rel_type) VALUES($1, $2, $3) ON CONFLICT DO NOTHING
	`); err != nil {
		return nil, err
	}
	if d.insertNodeStmt, err = d.db.Prepare(`
		INSERT INTO msc2836_nodes(event_id, origin_server_ts, room_id) VALUES($1, $2, $3) ON CONFLICT DO NOTHING
	`); err != nil {
		return nil, err
	}
	if d.selectChildrenForParentStmt, err = d.db.Prepare(`
		SELECT child_event_id FROM msc2836_edges WHERE parent_event_id = $1 AND rel_type = $2
	`); err != nil {
		return nil, err
	}
	return &d, err
}

func newSQLiteDatabase(dbOpts *config.DatabaseOptions) (Database, error) {
	d := DB{
		writer: sqlutil.NewExclusiveWriter(),
	}
	var err error
	if d.db, err = sqlutil.Open(dbOpts); err != nil {
		return nil, err
	}
	_, err = d.db.Exec(`
	CREATE TABLE IF NOT EXISTS msc2836_edges (
		parent_event_id TEXT NOT NULL,
		child_event_id TEXT NOT NULL,
		rel_type TEXT NOT NULL,
		UNIQUE (parent_event_id, child_event_id, rel_type)
	);

	CREATE TABLE IF NOT EXISTS msc2836_nodes (
		event_id TEXT PRIMARY KEY NOT NULL,
		origin_server_ts BIGINT NOT NULL,
		room_id TEXT NOT NULL
	);
	`)
	if err != nil {
		return nil, err
	}
	if d.insertEdgeStmt, err = d.db.Prepare(`
		INSERT INTO msc2836_edges(parent_event_id, child_event_id, rel_type) VALUES($1, $2, $3) ON CONFLICT (parent_event_id, child_event_id, rel_type) DO NOTHING
	`); err != nil {
		return nil, err
	}
	if d.insertNodeStmt, err = d.db.Prepare(`
		INSERT INTO msc2836_nodes(event_id, origin_server_ts, room_id) VALUES($1, $2, $3) ON CONFLICT DO NOTHING
	`); err != nil {
		return nil, err
	}
	if d.selectChildrenForParentStmt, err = d.db.Prepare(`
		SELECT child_event_id FROM msc2836_edges WHERE parent_event_id = $1 AND rel_type = $2
	`); err != nil {
		return nil, err
	}
	return &d, nil
}

func (p *DB) StoreRelation(ctx context.Context, ev *gomatrixserverlib.HeaderedEvent) error {
	parent, child, relType := parentChildEventIDs(ev)
	if parent == "" || child == "" {
		return nil
	}
	return p.writer.Do(p.db, nil, func(txn *sql.Tx) error {
		_, err := txn.Stmt(p.insertEdgeStmt).ExecContext(ctx, parent, child, relType)
		if err != nil {
			return err
		}
		_, err = txn.Stmt(p.insertNodeStmt).ExecContext(ctx, ev.EventID(), ev.OriginServerTS(), ev.RoomID())
		return err
	})
}

func (p *DB) ChildrenForParent(ctx context.Context, eventID, relType string) ([]string, error) {
	rows, err := p.selectChildrenForParentStmt.QueryContext(ctx, eventID, relType)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // nolint: errcheck
	var children []string
	for rows.Next() {
		var childID string
		if err := rows.Scan(&childID); err != nil {
			return nil, err
		}
		children = append(children, childID)
	}
	return children, nil
}

func parentChildEventIDs(ev *gomatrixserverlib.HeaderedEvent) (parent, child, relType string) {
	if ev == nil {
		return
	}
	body := struct {
		Relationship struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relationship"`
	}{}
	if err := json.Unmarshal(ev.Content(), &body); err != nil {
		return
	}
	if body.Relationship.EventID == "" || body.Relationship.RelType == "" {
		return
	}
	return body.Relationship.EventID, ev.EventID(), body.Relationship.RelType
}
