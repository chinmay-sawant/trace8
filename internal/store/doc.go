// Package store is trace8's local keyword store: records of keywords
// plus a text or blob payload, indexed in memory and persisted as an
// append-only log. Open replays the log once; after that reads are map
// lookups and never touch disk. Writes append one record and are
// durable immediately. A lock file keeps the store to one writer at a
// time, and Compact rewrites the log without tombstones through a temp
// file plus rename. Every exported method is safe for concurrent use.
package store
