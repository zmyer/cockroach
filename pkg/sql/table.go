// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)
// Author: Andrei Matei (andreimatei1@gmail.com)

package sql

import (
	"bytes"
	"fmt"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/jobs"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
)

var testDisableTableLeases bool

// TestDisableTableLeases disables table leases and returns
// a function that can be used to enable it.
func TestDisableTableLeases() func() {
	testDisableTableLeases = true
	return func() {
		testDisableTableLeases = false
	}
}

// tableKey implements sqlbase.DescriptorKey.
type tableKey struct {
	parentID sqlbase.ID
	name     string
}

func (tk tableKey) Key() roachpb.Key {
	return sqlbase.MakeNameMetadataKey(tk.parentID, tk.name)
}

func (tk tableKey) Name() string {
	return tk.name
}

// GetKeysForTableDescriptor retrieves the KV keys corresponding
// to the zone, name and descriptor of a table.
func GetKeysForTableDescriptor(
	tableDesc *sqlbase.TableDescriptor,
) (zoneKey roachpb.Key, nameKey roachpb.Key, descKey roachpb.Key) {
	zoneKey = sqlbase.MakeZoneKey(tableDesc.ID)
	nameKey = sqlbase.MakeNameMetadataKey(tableDesc.ParentID, tableDesc.GetName())
	descKey = sqlbase.MakeDescMetadataKey(tableDesc.ID)
	return
}

// A unique id for a particular table descriptor version.
type tableVersionID struct {
	id      sqlbase.ID
	version sqlbase.DescriptorVersion
}

// SchemaAccessor provides helper methods for using the SQL schema.
type SchemaAccessor interface {
	// NB: one can use GetTableDescFromID() to retrieve a descriptor for
	// a table from a transaction using its ID, assuming it was loaded
	// in the transaction already.

	// notifySchemaChange notifies that an outstanding schema change
	// exists for the table.
	notifySchemaChange(tableDesc *sqlbase.TableDescriptor, mutationID sqlbase.MutationID)

	// writeTableDesc effectively writes a table descriptor to the
	// database within the current planner transaction.
	writeTableDesc(ctx context.Context, tableDesc *sqlbase.TableDescriptor) error
}

var _ SchemaAccessor = &planner{}

func (p *planner) getVirtualTabler() VirtualTabler {
	return &p.session.virtualSchemas
}

// getTableOrViewDesc returns a table descriptor for either a table or view,
// or nil if the descriptor is not found.
func getTableOrViewDesc(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, tn *parser.TableName,
) (*sqlbase.TableDescriptor, error) {
	virtual, err := vt.getVirtualTableDesc(tn)
	if err != nil || virtual != nil {
		if sqlbase.IsUndefinedRelationError(err) {
			return nil, nil
		}
		return virtual, err
	}

	dbDesc, err := MustGetDatabaseDesc(ctx, txn, vt, tn.Database())
	if err != nil {
		return nil, err
	}

	desc := sqlbase.TableDescriptor{}
	found, err := getDescriptor(ctx, txn, tableKey{parentID: dbDesc.ID, name: tn.Table()}, &desc)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &desc, nil
}

// getTableDesc returns a table descriptor for a table, or nil if the
// descriptor is not found. If you want the "not found" condition to
// return an error, use mustGetTableDesc() instead.
//
// Returns an error if the underlying table descriptor actually
// represents a view rather than a table.
func getTableDesc(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, tn *parser.TableName,
) (*sqlbase.TableDescriptor, error) {
	desc, err := getTableOrViewDesc(ctx, txn, vt, tn)
	if err != nil {
		return desc, err
	}
	if desc != nil && !desc.IsTable() {
		return nil, sqlbase.NewWrongObjectTypeError(tn, "table")
	}
	return desc, nil
}

// getViewDesc returns a table descriptor for a table, or nil if the
// descriptor is not found.
//
// Returns an error if the underlying table descriptor actually
// represents a table rather than a view.
func getViewDesc(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, tn *parser.TableName,
) (*sqlbase.TableDescriptor, error) {
	desc, err := getTableOrViewDesc(ctx, txn, vt, tn)
	if err != nil {
		return desc, err
	}
	if desc != nil && !desc.IsView() {
		return nil, sqlbase.NewWrongObjectTypeError(tn, "view")
	}
	return desc, nil
}

// mustGetTableOrViewDesc returns a table descriptor for either a table or
// view, or an error if the descriptor is not found. allowAdding when set allows
// a table descriptor in the ADD state to also be returned.
func mustGetTableOrViewDesc(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, tn *parser.TableName, allowAdding bool,
) (*sqlbase.TableDescriptor, error) {
	desc, err := getTableOrViewDesc(ctx, txn, vt, tn)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, sqlbase.NewUndefinedRelationError(tn)
	}
	if err := filterTableState(desc); err != nil {
		if !allowAdding && err != errTableAdding {
			return nil, err
		}
	}
	return desc, nil
}

// mustGetTableDesc returns a table descriptor for a table, or an error if
// the descriptor is not found. allowAdding when set allows a table descriptor
// in the ADD state to also be returned.
func mustGetTableDesc(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, tn *parser.TableName, allowAdding bool,
) (*sqlbase.TableDescriptor, error) {
	desc, err := getTableDesc(ctx, txn, vt, tn)
	if err != nil {
		return nil, err
	}
	if desc == nil {
		return nil, sqlbase.NewUndefinedRelationError(tn)
	}
	if err := filterTableState(desc); err != nil {
		if !allowAdding && err != errTableAdding {
			return nil, err
		}
	}
	return desc, nil
}

var errTableDropped = errors.New("table is being dropped")
var errTableAdding = errors.New("table is being added")

func filterTableState(tableDesc *sqlbase.TableDescriptor) error {
	switch {
	case tableDesc.Dropped():
		return errTableDropped
	case tableDesc.Adding():
		return errTableAdding
	case tableDesc.State != sqlbase.TableDescriptor_PUBLIC:
		return errors.Errorf("table in unknown state: %s", tableDesc.State.String())
	}
	return nil
}

// TableCollection is a collection of tables held by a single session that
// serves SQL requests, or a background job using a table descriptor.
type TableCollection struct {
	timestamp hlc.Timestamp
	// A collection of table descriptor valid for the timestamp.
	// They are released once the transaction using them is complete.
	// If the transaction gets pushed and the timestamp changes,
	// the tables are released.
	tables []*sqlbase.TableDescriptor

	// leaseMgr manages acquiring and releasing per-table leases.
	leaseMgr *LeaseManager
	// databaseCache is used as a cache for database names.
	// TODO(andrei): get rid of it and replace it with a leasing system for
	// database descriptors.
	databaseCache *databaseCache
}

func (tc *TableCollection) maybeChangeTimestamp(ctx context.Context, txn *client.Txn) {
	if tc.timestamp != (hlc.Timestamp{}) && tc.timestamp != txn.OrigTimestamp() {
		txn.ResetDeadline()
		tc.releaseTables(ctx)
	}
}

// getTableVersion returns a table descriptor with a version suitable for
// the transaction. The table must be released by calling tc.releaseTables().
//
// TODO(vivek): The suitability of the table version returned is only partial
// checked. The expiration time for the table descriptor is added as a deadline
// for the transaction. The ModificationTime of the table is not compared to the
// timestamp of the transaction. Fix this to be such that:
// table.ModificationTime <= txn.Timestamp < expirationTime
//
// TODO(vivek): Rollback most of #6418. #6418 introduced a transaction deadline
// that is enforced at the KV layer, and was introduced to manager the validity
// window of a table descriptor. Since we will be checking for the valid use
// of a table descriptor here, we do not need the extra check at the KV layer.
func (tc *TableCollection) getTableVersion(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, tn *parser.TableName,
) (*sqlbase.TableDescriptor, error) {
	if log.V(2) {
		log.Infof(ctx, "planner acquiring lease on table '%s'", tn)
	}

	isSystemDB := tn.Database() == sqlbase.SystemDB.Name
	isVirtualDB := vt.getVirtualDatabaseDesc(tn.Database()) != nil
	if isSystemDB || isVirtualDB || testDisableTableLeases {
		// We don't go through the normal lease mechanism for:
		// - system tables. The system.lease and system.descriptor table, in
		//   particular, are problematic because they are used for acquiring
		//   leases itself, creating a chicken&egg problem.
		// - virtual tables. These tables' descriptors are not persisted,
		//   so they cannot be leased. Instead, we simply return the static
		//   descriptor and rely on the immutability privileges set on the
		//   descriptors to cause upper layers to reject mutations statements.
		tbl, err := mustGetTableDesc(ctx, txn, vt, tn, false /*allowAdding*/)
		if err != nil {
			return nil, err
		}
		return tbl, nil
	}

	dbID, err := tc.databaseCache.getDatabaseID(ctx, txn, vt, tn.Database())
	if err != nil {
		return nil, err
	}

	// If the txn has been pushed the table collection is released and
	// txn deadline is reset.
	tc.maybeChangeTimestamp(ctx, txn)

	// First, look to see if we already have the table.
	// This ensures that, once a SQL transaction resolved name N to id X, it will
	// continue to use N to refer to X even if N is renamed during the
	// transaction.
	for _, table := range tc.tables {
		if table.Name == string(tn.TableName) &&
			table.ParentID == dbID {
			if log.V(2) {
				log.Infof(ctx, "found table in table collection for table '%s'", tn)
			}
			return table, nil
		}
	}

	table, expiration, err := tc.leaseMgr.AcquireByName(ctx, txn, dbID, tn.Table())
	if err != nil {
		if err == sqlbase.ErrDescriptorNotFound {
			// Transform the descriptor error into an error that references the
			// table's name.
			return nil, sqlbase.NewUndefinedRelationError(tn)
		}
		return nil, err
	}
	tc.timestamp = txn.OrigTimestamp()
	tc.tables = append(tc.tables, table)
	if log.V(2) {
		log.Infof(ctx, "added table '%s' to table collection", tn)
	}
	// If the table we just acquired expires before the txn's deadline, reduce
	// the deadline.
	txn.UpdateDeadlineMaybe(expiration)
	return table, nil
}

// getTableVersionByID is a by-ID variant of getTableVersion (i.e. uses same cache).
func (tc *TableCollection) getTableVersionByID(
	ctx context.Context, txn *client.Txn, tableID sqlbase.ID,
) (*sqlbase.TableDescriptor, error) {
	if log.V(2) {
		log.Infof(ctx, "planner getting table on table ID %d", tableID)
	}

	if testDisableTableLeases {
		table, err := sqlbase.GetTableDescFromID(ctx, txn, tableID)
		if err != nil {
			return nil, err
		}
		if err := filterTableState(table); err != nil {
			return nil, err
		}
		return table, nil
	}

	// If the txn has been pushed the table collection is released and
	// txn deadline is reset.
	tc.maybeChangeTimestamp(ctx, txn)

	// First, look to see if we already have the table -- including those
	// via `getTableVersion`.
	for _, table := range tc.tables {
		if table.ID == tableID {
			if log.V(2) {
				log.Infof(ctx, "found table %d in table cache", tableID)
			}
			return table, nil
		}
	}

	table, expiration, err := tc.leaseMgr.Acquire(ctx, txn, tableID, 0)
	if err != nil {
		if err == sqlbase.ErrDescriptorNotFound {
			// Transform the descriptor error into an error that references the
			// table's ID.
			return nil, sqlbase.NewUndefinedRelationError(
				&parser.TableRef{TableID: int64(tableID)})
		}
		return nil, err
	}
	tc.timestamp = txn.OrigTimestamp()
	tc.tables = append(tc.tables, table)
	if log.V(2) {
		log.Infof(ctx, "added table '%s' to table collection", table.Name)
	}
	// If the table we just acquired expires before the txn's deadline, reduce
	// the deadline.
	txn.UpdateDeadlineMaybe(expiration)
	return table, nil
}

// releaseTables releases all tables currently held by the Session.
func (tc *TableCollection) releaseTables(ctx context.Context) {
	if len(tc.tables) > 0 {
		log.VEventf(ctx, 2, "releasing %d tables", len(tc.tables))
		for _, table := range tc.tables {
			if err := tc.leaseMgr.Release(table); err != nil {
				log.Warning(ctx, err)
			}
		}
		tc.tables = tc.tables[:0]
	}
}

// getTableNames retrieves the list of qualified names of tables
// present in the given database.
func getTableNames(
	ctx context.Context,
	txn *client.Txn,
	vt VirtualTabler,
	dbDesc *sqlbase.DatabaseDescriptor,
	dbNameOriginallyOmitted bool,
) (parser.TableNames, error) {
	if e, ok := vt.getVirtualSchemaEntry(dbDesc.Name); ok {
		return e.tableNames(dbNameOriginallyOmitted), nil
	}

	prefix := sqlbase.MakeNameMetadataKey(dbDesc.ID, "")
	sr, err := txn.Scan(ctx, prefix, prefix.PrefixEnd(), 0)
	if err != nil {
		return nil, err
	}

	var tableNames parser.TableNames
	for _, row := range sr {
		_, tableName, err := encoding.DecodeUnsafeStringAscending(
			bytes.TrimPrefix(row.Key, prefix), nil)
		if err != nil {
			return nil, err
		}
		tn := parser.TableName{
			DatabaseName:            parser.Name(dbDesc.Name),
			TableName:               parser.Name(tableName),
			DBNameOriginallyOmitted: dbNameOriginallyOmitted,
		}
		tableNames = append(tableNames, tn)
	}
	return tableNames, nil
}

func (p *planner) getAliasedTableName(n parser.TableExpr) (*parser.TableName, error) {
	if ate, ok := n.(*parser.AliasedTableExpr); ok {
		n = ate.Expr
	}
	table, ok := n.(*parser.NormalizableTableName)
	if !ok {
		return nil, errors.Errorf("TODO(pmattis): unsupported FROM: %s", n)
	}
	return table.NormalizeWithDatabaseName(p.session.Database)
}

// createSchemaChangeJob finalizes the current mutations in the table
// descriptor and creates a schema change job in the system.jobs table.
// The identifiers of the mutations and newly-created job are written to a new
// MutationJob in the table descriptor.
func (p *planner) createSchemaChangeJob(
	ctx context.Context, tableDesc *sqlbase.TableDescriptor, stmt string,
) (sqlbase.MutationID, error) {
	mutationID, err := tableDesc.FinalizeMutation()
	if err != nil {
		return sqlbase.InvalidMutationID, err
	}
	jobRecord := jobs.Record{
		Description:   stmt,
		Username:      p.User(),
		DescriptorIDs: sqlbase.IDs{tableDesc.GetID()},
		Details:       jobs.SchemaChangeDetails{},
	}
	job := jobs.NewJob(p.ExecCfg().DB, InternalExecutor{LeaseManager: p.session.tables.leaseMgr}, jobRecord)
	if err := job.WithTxn(p.txn).Created(ctx); err != nil {
		return sqlbase.InvalidMutationID, nil
	}
	tableDesc.MutationJobs = append(tableDesc.MutationJobs, sqlbase.TableDescriptor_MutationJob{
		MutationID: mutationID, JobID: *job.ID()})
	return mutationID, nil
}

// notifySchemaChange implements the SchemaAccessor interface.
func (p *planner) notifySchemaChange(
	tableDesc *sqlbase.TableDescriptor, mutationID sqlbase.MutationID,
) {
	sc := SchemaChanger{
		tableID:    tableDesc.GetID(),
		mutationID: mutationID,
		nodeID:     p.evalCtx.NodeID,
		leaseMgr:   p.LeaseMgr(),
	}
	p.session.TxnState.schemaChangers.queueSchemaChanger(sc)
}

// writeTableDesc implements the SchemaAccessor interface.
func (p *planner) writeTableDesc(ctx context.Context, tableDesc *sqlbase.TableDescriptor) error {
	if isVirtualDescriptor(tableDesc) {
		panic(fmt.Sprintf("Virtual Descriptors cannot be stored, found: %v", tableDesc))
	}
	// Some statements setTestingVerifyMetadata to verify the descriptor they
	// have written, but if they are followed by other statements that modify
	// the descriptor the verification of the overwritten descriptor cannot be
	// done.
	p.session.setTestingVerifyMetadata(nil)

	descKey := sqlbase.MakeDescMetadataKey(tableDesc.GetID())
	descVal := sqlbase.WrapDescriptor(tableDesc)
	if p.session.Tracing.KVTracingEnabled() {
		log.VEventf(ctx, 2, "Put %s -> %s", descKey, descVal)
	}
	return p.txn.Put(ctx, descKey, descVal)
}

// expandTableGlob expands pattern into a list of tables represented
// as a parser.TableNames.
func expandTableGlob(
	ctx context.Context,
	txn *client.Txn,
	vt VirtualTabler,
	database string,
	pattern parser.TablePattern,
) (parser.TableNames, error) {
	if t, ok := pattern.(*parser.TableName); ok {
		if err := t.QualifyWithDatabase(database); err != nil {
			return nil, err
		}
		return parser.TableNames{*t}, nil
	}

	glob := pattern.(*parser.AllTablesSelector)

	if err := glob.QualifyWithDatabase(database); err != nil {
		return nil, err
	}

	dbDesc, err := MustGetDatabaseDesc(ctx, txn, vt, string(glob.Database))
	if err != nil {
		return nil, err
	}

	tableNames, err := getTableNames(ctx, txn, vt, dbDesc, glob.DBNameOriginallyOmitted)
	if err != nil {
		return nil, err
	}
	return tableNames, nil
}

// searchAndQualifyDatabase augments the table name with the database
// where it was found. It searches first in the session current
// database, if that's defined, otherwise the search path.  The
// provided TableName is modified in-place in case of success, and
// left unchanged otherwise.
// The table name must not be qualified already.
func (p *planner) searchAndQualifyDatabase(ctx context.Context, tn *parser.TableName) error {
	t := *tn

	descFunc := p.session.tables.getTableVersion
	if p.avoidCachedDescriptors {
		// AS OF SYSTEM TIME queries need to fetch the table descriptor at the
		// specified time, and never lease anything. The proto transaction already
		// has its timestamps set correctly so getTableOrViewDesc will fetch with
		// the correct timestamp.
		descFunc = getTableOrViewDesc
	}

	if p.session.Database != "" {
		t.DatabaseName = parser.Name(p.session.Database)
		desc, err := descFunc(ctx, p.txn, p.getVirtualTabler(), &t)
		if err != nil && !sqlbase.IsUndefinedRelationError(err) && !sqlbase.IsUndefinedDatabaseError(err) {
			return err
		}
		if desc != nil {
			// Table was found, use this name.
			*tn = t
			return nil
		}
	}

	// Not found using the current session's database, so try
	// the search path instead.
	for _, database := range p.session.SearchPath {
		t.DatabaseName = parser.Name(database)
		desc, err := descFunc(ctx, p.txn, p.getVirtualTabler(), &t)
		if err != nil && !sqlbase.IsUndefinedRelationError(err) && !sqlbase.IsUndefinedDatabaseError(err) {
			return err
		}
		if desc != nil {
			// The table or view exists in this database, so return it.
			*tn = t
			return nil
		}
	}

	return sqlbase.NewUndefinedRelationError(&t)
}

// getQualifiedTableName returns the database-qualified name of the table
// or view represented by the provided descriptor.
func (p *planner) getQualifiedTableName(
	ctx context.Context, desc *sqlbase.TableDescriptor,
) (string, error) {
	dbDesc, err := sqlbase.GetDatabaseDescFromID(ctx, p.txn, desc.ParentID)
	if err != nil {
		return "", err
	}
	tbName := parser.TableName{
		DatabaseName: parser.Name(dbDesc.Name),
		TableName:    parser.Name(desc.Name),
	}
	return tbName.String(), nil
}

// findTableContainingIndex returns the name of the table containing
// an index of the given name. An error is returned if the index is
// not found or if the index name is ambiguous (i.e. exists in
// multiple tables).
func (p *planner) findTableContainingIndex(
	ctx context.Context, txn *client.Txn, vt VirtualTabler, dbName parser.Name, idxName parser.Name,
) (result *parser.TableName, err error) {
	dbDesc, err := MustGetDatabaseDesc(ctx, txn, vt, string(dbName))
	if err != nil {
		return nil, err
	}

	tns, err := getTableNames(ctx, txn, vt, dbDesc, false)
	if err != nil {
		return nil, err
	}

	result = nil
	for i := range tns {
		tn := &tns[i]
		tableDesc, err := mustGetTableDesc(
			ctx, p.txn, p.getVirtualTabler(), tn, true, /*allowAdding*/
		)
		if err != nil {
			return nil, err
		}
		_, dropped, err := tableDesc.FindIndexByName(string(idxName))
		if err != nil || dropped {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("index name %q is ambiguous (found in %s and %s)",
				idxName, tn.String(), result.String())
		}
		result = tn
	}
	if result == nil {
		return nil, fmt.Errorf("index %q does not exist", idxName)
	}
	return result, nil
}

// expandIndexName ensures that the index name is qualified with a
// table name, and searches the table name if not yet specified. It returns
// the TableName of the underlying table for convenience.
func (p *planner) expandIndexName(
	ctx context.Context, index *parser.TableNameWithIndex,
) (*parser.TableName, error) {
	tn, err := index.Table.NormalizeWithDatabaseName(p.session.Database)
	if err != nil {
		return nil, err
	}

	if index.SearchTable {
		realTableName, err := p.findTableContainingIndex(ctx, p.txn, p.getVirtualTabler(), tn.DatabaseName, tn.TableName)
		if err != nil {
			return nil, err
		}
		index.Index = tn.TableName
		index.Table.TableNameReference = realTableName
		tn = realTableName
	}
	return tn, nil
}

// getTableAndIndex returns the table and index descriptors for a table
// (primary index) or table-with-index. Only one of table and tableWithIndex can
// be set.  This is useful for statements that have both table and index
// variants (like `ALTER TABLE/INDEX ... SPLIT AT ...`).
// It can return indexes that are being rolled out.
func (p *planner) getTableAndIndex(
	ctx context.Context,
	table *parser.NormalizableTableName,
	tableWithIndex *parser.TableNameWithIndex,
	privilege privilege.Kind,
) (*sqlbase.TableDescriptor, *sqlbase.IndexDescriptor, error) {
	var tn *parser.TableName
	var err error
	if tableWithIndex == nil {
		// Variant: ALTER TABLE
		tn, err = table.NormalizeWithDatabaseName(p.session.Database)
	} else {
		// Variant: ALTER INDEX
		tn, err = p.expandIndexName(ctx, tableWithIndex)
	}
	if err != nil {
		return nil, nil, err
	}
	tableDesc, err := getTableDesc(ctx, p.txn, p.getVirtualTabler(), tn)
	if err != nil {
		return nil, nil, err
	}
	if tableDesc == nil {
		return nil, nil, sqlbase.NewUndefinedRelationError(tn)
	}
	if err := p.CheckPrivilege(tableDesc, privilege); err != nil {
		return nil, nil, err
	}

	// Determine which index to use.
	var index sqlbase.IndexDescriptor
	if tableWithIndex == nil {
		index = tableDesc.PrimaryIndex
	} else {
		idx, dropped, err := tableDesc.FindIndexByName(string(tableWithIndex.Index))
		if err != nil {
			return nil, nil, err
		}
		if dropped {
			return nil, nil, fmt.Errorf("index %q being dropped", tableWithIndex.Index)
		}
		index = idx
	}
	return tableDesc, &index, nil
}
