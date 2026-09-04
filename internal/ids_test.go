package internal

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDSN matches the DSN the other DB-backed tests use.
const testDSN = "postgres://postgres@localhost:5432/test"

// testASIN returns an ASIN unique to this run. The mapping table is permanent
// by design, so tests can't reuse fixed ASINs without depending on whether a
// previous run already allocated them.
func testASIN(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("B%09d", rand.IntN(1_000_000_000))
}

func newTestMapper(t *testing.T) (*IDMapper, context.Context) {
	t.Helper()
	ctx := t.Context()
	m, err := NewIDMapper(ctx, testDSN)
	require.NoError(t, err)
	return m, ctx
}

func TestIDMapper(t *testing.T) {
	m, ctx := newTestMapper(t)
	asin := testASIN(t)

	bookID, err := m.ID(ctx, kindBook, asin, "The Final Empire")
	require.NoError(t, err)
	require.NotZero(t, bookID)

	t.Run("is stable", func(t *testing.T) {
		// The client persists these IDs, so a second call must return the
		// same one or the library is orphaned on the next refresh.
		again, err := m.ID(ctx, kindBook, asin, "The Final Empire")
		require.NoError(t, err)
		assert.Equal(t, bookID, again)
	})

	t.Run("survives a cold cache", func(t *testing.T) {
		// A fresh mapper has an empty in-memory cache and must recover the
		// same ID from Postgres.
		cold, err := NewIDMapper(ctx, testDSN)
		require.NoError(t, err)

		got, err := cold.ID(ctx, kindBook, asin, "The Final Empire")
		require.NoError(t, err)
		assert.Equal(t, bookID, got)
	})

	t.Run("namespaces by kind", func(t *testing.T) {
		// One ASIN backs both a work and its edition, and the client requires
		// the two to be distinct.
		workID, err := m.ID(ctx, kindWork, asin, "The Final Empire")
		require.NoError(t, err)
		assert.NotEqual(t, bookID, workID)

		authorID, err := m.ID(ctx, kindAuthor, asin, "")
		require.NoError(t, err)
		assert.NotEqual(t, bookID, authorID)
		assert.NotEqual(t, workID, authorID)
	})

	t.Run("fits in an int32", func(t *testing.T) {
		// The client parses every ForeignId as a signed 32-bit integer.
		assert.LessOrEqual(t, bookID, int64(math.MaxInt32))
		assert.Positive(t, bookID)
	})

	t.Run("resolves back to its ASIN", func(t *testing.T) {
		got, err := m.ASIN(ctx, kindBook, bookID)
		require.NoError(t, err)
		assert.Equal(t, asin, got)
	})

	t.Run("records the label", func(t *testing.T) {
		ref, err := m.Ref(ctx, bookID)
		require.NoError(t, err)
		assert.Equal(t, "The Final Empire", ref.label)
		assert.Equal(t, kindBook, ref.kind)
	})

	t.Run("rejects the wrong kind", func(t *testing.T) {
		// Asking for a book with an author's ID is a bug, not a miss.
		_, err := m.ASIN(ctx, kindAuthor, bookID)
		require.Error(t, err)
		assert.ErrorIs(t, err, errBadRequest)
	})

	t.Run("rejects an empty ASIN", func(t *testing.T) {
		_, err := m.ID(ctx, kindBook, "", "")
		require.Error(t, err)
		assert.ErrorIs(t, err, errBadRequest)
	})

	t.Run("reports unknown IDs as missing", func(t *testing.T) {
		// This is what a client holding IDs from a different metadata source
		// looks like, and it has to 404 rather than resolve to something else.
		_, err := m.Ref(ctx, math.MaxInt32)
		require.Error(t, err)
		assert.ErrorIs(t, err, errNotFound)

		_, err = m.Ref(ctx, 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, errBadRequest)
	})
}

// TestIDMapperConcurrent covers the upsert-returning query under contention.
// Search maps several results at once and the controller refreshes authors in
// the background, so the same ASIN is routinely requested concurrently; if
// that allocated two IDs the library would end up with duplicates.
func TestIDMapperConcurrent(t *testing.T) {
	m, ctx := newTestMapper(t)
	asin := testASIN(t)

	const workers = 16

	ids := make([]int64, workers)
	errs := make([]error, workers)

	wg := sync.WaitGroup{}
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = m.ID(ctx, kindWork, asin, "concurrent")
		}()
	}
	wg.Wait()

	for i := range workers {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i], "worker %d disagreed", i)
	}
	assert.NotZero(t, ids[0])
}

// TestIDMapperSeparateASINs is a sanity check that distinct ASINs never share
// an ID, since a collision silently merges two books in the client.
func TestIDMapperSeparateASINs(t *testing.T) {
	m, ctx := newTestMapper(t)

	seen := map[int64]string{}
	for range 25 {
		asin := testASIN(t)
		id, err := m.ID(ctx, kindBook, asin, "")
		require.NoError(t, err)

		if prev, ok := seen[id]; ok {
			t.Fatalf("ID %d assigned to both %s and %s", id, prev, asin)
		}
		seen[id] = asin
	}
}
