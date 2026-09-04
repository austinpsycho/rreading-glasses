package internal

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// idKind namespaces a surrogate ID so that one ASIN can back both a work and
// its edition without the two colliding.
type idKind string

const (
	kindWork   idKind = "w"
	kindBook   idKind = "b"
	kindAuthor idKind = "a"
	kindSeries idKind = "s"
)

// asinRef is the ASIN a surrogate ID was minted for.
type asinRef struct {
	kind  idKind
	asin  string
	label string
}

// IDMapper assigns stable 32-bit surrogate IDs to Audible ASINs.
//
// Audible keys everything by ASIN, but the client parses every ForeignId as a
// signed 32-bit integer, so ASINs cannot be passed through. Hashing an ASIN
// into that space is not safe: the birthday bound for int32 is only ~65k
// entries, and an ID collision silently corrupts a library. IDs are therefore
// allocated from a Postgres sequence and persisted, which also keeps them
// stable across reboots and re-scans.
//
// The in-memory maps are a read cache in front of Postgres. They are unbounded
// but only ever hold IDs this instance has actually touched, which for a
// personal library is small.
type IDMapper struct {
	db *pgxpool.Pool

	mu     sync.RWMutex
	toID   map[string]int64
	toASIN map[int64]asinRef
}

// NewIDMapper creates an IDMapper backed by the given DSN.
func NewIDMapper(ctx context.Context, dsn string) (*IDMapper, error) {
	db, err := newDB(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating db: %w", err)
	}
	return &IDMapper{
		db:     db,
		toID:   map[string]int64{},
		toASIN: map[int64]asinRef{},
	}, nil
}

func cacheKey(kind idKind, asin string) string {
	return string(kind) + ":" + asin
}

// ID returns the surrogate ID for an ASIN, allocating one if this is the first
// time we've seen it. The label is an optional human-readable name recorded
// alongside the mapping; it is only written on insert and is used to recover
// series titles, which Audible offers no way to look up by ASIN.
func (m *IDMapper) ID(ctx context.Context, kind idKind, asin string, label string) (int64, error) {
	if asin == "" {
		return 0, errors.Join(errBadRequest, errors.New("empty ASIN"))
	}

	key := cacheKey(kind, asin)

	m.mu.RLock()
	id, ok := m.toID[key]
	m.mu.RUnlock()
	if ok {
		return id, nil
	}

	// Upsert-returning: the CTE inserts if absent, and the UNION ALL arm reads
	// back the existing row on conflict. DO NOTHING is used rather than a
	// no-op DO UPDATE so that repeat calls don't rewrite the label.
	const q = `
WITH ins AS (
	INSERT INTO asin_id (kind, asin, label) VALUES ($1, $2, $3)
	ON CONFLICT (kind, asin) DO NOTHING
	RETURNING id
)
SELECT id FROM ins
UNION ALL
SELECT id FROM asin_id WHERE kind = $1 AND asin = $2
LIMIT 1;`

	err := m.db.QueryRow(ctx, q, string(kind), asin, label).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("mapping %s %q: %w", kind, asin, err)
	}

	m.mu.Lock()
	m.toID[key] = id
	m.toASIN[id] = asinRef{kind: kind, asin: asin, label: label}
	m.mu.Unlock()

	return id, nil
}

// Ref resolves a surrogate ID back to the ASIN it was minted for. It returns
// errNotFound if the ID was never allocated, which is what happens when a
// client asks for an ID left over from a different metadata source.
func (m *IDMapper) Ref(ctx context.Context, id int64) (asinRef, error) {
	if id == 0 {
		return asinRef{}, errors.Join(errBadRequest, errors.New("missing ID"))
	}

	m.mu.RLock()
	ref, ok := m.toASIN[id]
	m.mu.RUnlock()
	if ok {
		return ref, nil
	}

	var kind, asin, label string
	err := m.db.QueryRow(ctx,
		`SELECT kind, asin, COALESCE(label, '') FROM asin_id WHERE id = $1;`, id,
	).Scan(&kind, &asin, &label)
	if err != nil {
		return asinRef{}, errors.Join(errNotFound, fmt.Errorf("unknown ID %d: %w", id, err))
	}

	ref = asinRef{kind: idKind(kind), asin: asin, label: label}

	m.mu.Lock()
	m.toASIN[id] = ref
	m.toID[cacheKey(ref.kind, asin)] = id
	m.mu.Unlock()

	return ref, nil
}

// ASIN resolves a surrogate ID back to its ASIN, requiring it to be of the
// expected kind. Asking for a book ID with a work ID is a bug rather than a
// missing record, so the kinds are checked rather than coerced.
func (m *IDMapper) ASIN(ctx context.Context, kind idKind, id int64) (string, error) {
	ref, err := m.Ref(ctx, id)
	if err != nil {
		return "", err
	}
	if ref.kind != kind {
		return "", errors.Join(errBadRequest,
			fmt.Errorf("ID %d is a %q, wanted a %q", id, ref.kind, kind))
	}
	return ref.asin, nil
}
