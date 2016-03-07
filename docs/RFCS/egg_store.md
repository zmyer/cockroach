- Feature Name: Egg Store
- Status: draft
- Start Date: 2016-03-06
- Authors: Spencer Kimball
- RFC PR: (PR # after acceptance of initial draft)
- Cockroach Issue: (one or more # from the issue tracker)


# Summary

Add an integral blob storage facility to CockroachDB. The blob storage
can be accessed both via SQL (i.e. `BLOB` type columns) or via an
S3-like RESTful API. Both server endpoints would use a blob client
which accesses and mutates blob metadata using SQL system tables.  The
CockroachDB node gets `ChunkStores`, an RPC-handling endpoint
analogous to CockroachDB `Stores`, but which provide read, write and
stat of the file data which make up blobs. A `ChunkStore` would
provide a maintenance scanner for verifying on-disk bytes, similar to
the `Store` scanner queues.

CockroachDB `Stores` would include a new replica scanner queue to do
routine maintenance on the tuples of blob metadata which may reside
within a replica being scanned. Maintenance would re-replicate and
respond to configuration changes.

# Motivation

A common question from users of an RDBMS is whether it's acceptable to
store large column values (e.g. 1M, 10M, 100M, ...). The answer is
usually "no". With CockroachDB set to triplicate data, write
amplification is 12x, as each of the three replicas gets written to
the Raft log, which includes two writes to RocksDB `WAL` and RocksDB
`SSTables`, and then to the FSM, which again includes two writes to
RocksDB `WAL` and RocksDB `SSTables`. If you're going to support
arbitrarily large blobs in column values, you want write amplification
to instead be 3x.

Assume a primary use case for CockroachDB is enterprises which require
either scale, customization or regulatory adherence mandating they own
the technical stack. For these users closed source, vendor lock-in,
and price will be impetus for avoiding public cloud backing services
such as AWS S3 or DynamoDB. For such users, an RDBMS may well be the
most vital piece of infrastructure, but a blob storage system is often
not far behind. If price dictates a move off of AWS or GCE, a platform
which provides both core pieces of infrastructure, and especially one
which does it in an integrated package, will provide a simple but
powerful solution.

# Detailed design

If you squint at CockroachDB, the architecture already provides much
of what is necessary to build a distributed blob storage system. In
particular:

- Gateway to proxy client requests to access blob data
- Distributed, Transactional (!) blob metadata storage
- Disk storage nodes with maintenance scanning queues
- Collection and dissemination of storage allocation metrics
- Stats, historical time series, admin UI

## Timeline

A review of CockroachDB's current architecture provides a tantalizing
glimpse at a blob storage system not very far into the distance.

### Phase 1

The first phase would require two new system tables for blob metadata:
files and stripes, a simple REST gateway supporting CRUD operations, a
blob client supporting replicated blobs, and a simple chunk storage
server.

### Phase 2

The second phase could provide the necessary pieces to maintain blob
integrity. On the chunk storage servers, a file verifier would read
chunks periodically and verify checksums. Two new scanners on
CockroachDB `Stores` would maintain blob metadata by scanning the rows
in the blob system tables. The blob file metadata would be scrutinized
by a quota manager. The blob stripe metadata would be scrutinized by
a stripe manager.

### Phase 3

Add support for blob storage via the SQL interface. This would allow
arbitrarily-sized blob column values to be inserted and updated with
transactional integrity alongside other column values. The value
actually stored in the column would be a pointer to the blob. The blob
would in turn require a back pointer to the SQL tuple containing the
blob pointer to provide garbage collection in the event that a blob
was partially written before a SQL transaction was aborted via a
gateway or client crash.

## Blob Metadata

### Maintenance Queues

#### Quota Manager

#### Stripe Manager

TODO

## S3-like REST Gateway

TODO

## Blob Client

TODO

## ChunkStore

TODO

### Maintenance

## SQL Integration

TODO

## Architecture By Diagram

Blue indicates functionality to add.

![Broad Architecture](/resource/doc/egg_store.png?raw=true)

# Drawbacks

# Alternatives

# Unresolved questions
